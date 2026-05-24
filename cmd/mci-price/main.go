// mci-price 는 WTG (Winway Trading Gateway) 의 FX 시세 fan-out 서비스.
//
// 흐름:
//
//	mymq broker (PRICE exchange, unsolicited)
//	   ↓ subscribe
//	internal/price.Server
//	   ├→ Conflation (latest 1)
//	   ├→ gRPC PriceService stream (mci-edge-price 향)
//	   └→ Aggregator (옵션, ChartDSN 설정 시)
//	         ├→ JSONCookerDecoder + SymbolMap → Quote
//	         ├→ Bar OHLC 누적 (6 timeframe)
//	         └→ BarCloseHandler → Archiver → TimescaleDB
//
// 사용 예:
//
//	# 1) 기본 (broker → gRPC stream + stdout dump 처음 10건)
//	mci-price --listen=:8082 --grpc=:8083 --broker-host=10.0.0.10 --print=10
//
//	# 2) 봉 영속화까지 활성 (ChartDSN + SymbolsFile)
//	mci-price --listen=:8082 \
//	          --broker-host=10.0.0.10 \
//	          --chart-dsn='postgres://wtg:secret@db:5432/wtg' \
//	          --symbols=etc/symbols.json
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/internal/price"
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

func main() {
	cfg, err := price.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-price: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-price 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", cfg.BrokerHost, cfg.BrokerPort)),
		slog.String("queue", cfg.QueueName),
		slog.String("exchange", cfg.ExchangeName),
		slog.String("symbols_file", cfg.SymbolsFile),
		slog.Bool("chart_enabled", cfg.ChartDSN != ""),
		slog.Int("print", cfg.PrintFirstN),
	)

	srv := price.NewServer(cfg, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 1) stdout dump consumer — 1차 prototype 디버깅용.
	if cfg.PrintFirstN > 0 {
		var printed atomic.Int32
		srv.AddConsumer(price.TickConsumerFunc(func(t *price.Tick) {
			if printed.Load() >= int32(cfg.PrintFirstN) {
				return
			}
			n := printed.Add(1)
			if n > int32(cfg.PrintFirstN) {
				return
			}
			fmt.Printf("[tick %d] mkid=%d symbol=%q seq=%d type=%d body_len=%d\n",
				n, t.MarketID, t.Symbol, t.SeqNum, t.Type, len(t.Body))
		}))
	}

	// 2) etcd 클라이언트 (옵션 — endpoints 있을 때만).
	//    설정되면 SymbolMap / PricingTable / Profiles 가 watch 로 hot reload.
	var (
		etcdCli        *clientv3.Client
		etcdSymWatch   *quote.EtcdSymbolWatcher
		etcdTblWatch   *pricing.EtcdTableWatcher
		etcdProfileSrc *price.EtcdProfileSource
	)
	if len(cfg.EtcdEndpoints) > 0 {
		clientCfg := clientv3.Config{
			Endpoints:   cfg.EtcdEndpoints,
			DialTimeout: cfg.EtcdDialTimeout,
			Username:    cfg.EtcdUsername,
			Password:    cfg.EtcdPassword,
		}
		if cfg.EtcdTLSCertFile != "" || cfg.EtcdTLSKeyFile != "" || cfg.EtcdTLSCAFile != "" {
			tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
				CertFile:     cfg.EtcdTLSCertFile,
				KeyFile:      cfg.EtcdTLSKeyFile,
				ServerCAFile: cfg.EtcdTLSCAFile,
				ServerName:   cfg.EtcdTLSServerName,
			})
			if err != nil {
				logger.Error("etcd TLS 구성 실패", slog.Any("error", err))
				os.Exit(1)
			}
			clientCfg.TLS = tlsCfg
		}
		var err error
		etcdCli, err = clientv3.New(clientCfg)
		if err != nil {
			logger.Error("etcd dial 실패", slog.Any("error", err))
			os.Exit(1)
		}
		defer etcdCli.Close()
		logger.Info("etcd 활성",
			slog.Any("endpoints", cfg.EtcdEndpoints),
			slog.String("prefix", cfg.EtcdPrefix),
			slog.Bool("tls", clientCfg.TLS != nil),
			slog.Bool("mtls", cfg.EtcdTLSCertFile != ""),
		)
	}

	// 3) SymbolMap — etcd watch 우선, 없으면 파일.
	symbols := quote.NewSymbolMap()
	if etcdCli != nil {
		var err error
		etcdSymWatch, err = quote.NewEtcdSymbolWatcher(ctx, quote.EtcdSymbolWatcherOptions{
			Client: etcdCli,
			Prefix: cfg.EtcdPrefix + "quote/symbols/",
			M:      symbols,
			Logger: logger,
		})
		if err != nil {
			logger.Error("SymbolMap watcher 시작 실패", slog.Any("error", err))
			os.Exit(1)
		}
		defer etcdSymWatch.Close()
	} else if cfg.SymbolsFile != "" {
		entries, err := loadSymbolEntries(cfg.SymbolsFile)
		if err != nil {
			logger.Error("symbols 파일 로드 실패", slog.Any("error", err))
			os.Exit(1)
		}
		symbols.Replace(entries)
		logger.Info("SymbolMap 파일 로드",
			slog.String("file", cfg.SymbolsFile),
			slog.Int("count", len(entries)),
		)
	}

	// 3) GRPCServer 사전 생성 (옵션) — onClose 체인과 PricingConsumer 가 fan-out
	//    publisher 로 직접 참조. cfg.GRPCAddr 가 비어있어도 GRPCServer 만 만들어
	//    두면 SubscribeQuote/SubscribeBar 호출은 가능 (단, Serve 는 안 함).
	var grpcSrv *price.GRPCServer
	if cfg.GRPCAddr != "" {
		grpcSrv = price.NewGRPCServer(logger, cfg.GRPCBufSize)
		srv.AttachGRPC(grpcSrv)
	}

	// QuoteID stack (pkg/quoteid) — Generator + Registry. cfg.QuoteIDInstance
	// 가 비어 있으면 비활성 (양쪽 nil 반환 → PricingConsumer 가 fallback).
	quoteIDGen, quoteIDReg, quoteIDCloser := wireQuoteID(cfg, logger)
	defer quoteIDCloser()
	if quoteIDReg != nil {
		validator := price.NewQuoteValidationServer(quoteIDReg, logger)
		if len(cfg.QuoteIDEnginesAllowlist) > 0 {
			validator.SetEngineAllowlist(cfg.QuoteIDEnginesAllowlist)
			logger.Info("QuoteValidationService RBAC 활성 (정적)",
				slog.Any("engines", cfg.QuoteIDEnginesAllowlist))
		}
		// etcd watch — 정적 슬라이스를 덮어쓰며 hot reload.
		if etcdCli != nil && cfg.QuoteIDEnginesEtcdPrefix != "" {
			w, err := quoteid.NewEtcdAllowlistWatcher(ctx, quoteid.EtcdAllowlistWatcherOptions{
				Client:  etcdCli,
				Prefix:  cfg.QuoteIDEnginesEtcdPrefix,
				OnApply: validator.SetEngineAllowlistMap,
				Logger:  logger,
			})
			if err != nil {
				logger.Error("EngineAllowlist etcd watcher 구성 실패", slog.Any("error", err))
				os.Exit(1)
			}
			defer w.Close()
			logger.Info("QuoteValidationService RBAC 활성 (etcd hot reload)",
				slog.String("prefix", cfg.QuoteIDEnginesEtcdPrefix))
		}
		if grpcSrv != nil {
			grpcSrv.AttachValidator(validator)
			logger.Info("QuoteValidationService 등록 (gRPC) — 매칭 엔진이 같은 gRPC 포트로 호출")
		}
		// HTTP gateway — 비-Go FIX gateway / 운영 도구 호환.
		srv.AttachQuoteValidator(validator)
	}

	// 4) Archiver + Aggregator wiring (옵션 — ChartDSN 있을 때만).
	var (
		pool     *pgxpool.Pool
		archiver *price.Archiver
		onClose  price.BarCloseHandler
	)
	if cfg.ChartDSN != "" {
		poolCfg, err := pgxpool.ParseConfig(cfg.ChartDSN)
		if err != nil {
			logger.Error("ChartDSN 파싱 실패", slog.Any("error", err))
			os.Exit(1)
		}
		poolCfg.MaxConns = int32(cfg.ChartPoolMaxConns)
		pool, err = pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			logger.Error("pgxpool 생성 실패", slog.Any("error", err))
			os.Exit(1)
		}
		defer pool.Close()

		archiver = price.NewArchiver(
			price.NewPgxInserter(pool),
			price.ArchiverOptions{
				QueueSize:     cfg.ArchiverQueueSize,
				FlushInterval: cfg.ArchiverFlushInterval,
				BatchMax:      cfg.ArchiverBatchMax,
				Logger:        logger,
			},
		)
		go func() {
			_ = archiver.Run(ctx)
		}()
		onClose = archiver.OnBarClose
		logger.Info("Archiver 활성",
			slog.Int("queue", cfg.ArchiverQueueSize),
			slog.Duration("flush", cfg.ArchiverFlushInterval),
			slog.Int("batch", cfg.ArchiverBatchMax),
		)
	} else {
		// no-op — chart 비활성 시 봉이 close 되어도 아무것도 안 함.
		onClose = func(*quote.Bar) {}
	}

	// onClose 에 gRPC bar publish 도 chaining — mci-chart 가 SubscribeBar 로
	// 받아 ws 라이브 갱신. GRPCServer 가 미설정이면 no-op.
	if grpcSrvLocal := grpcSrv; grpcSrvLocal != nil {
		baseOnClose := onClose
		onClose = func(b *quote.Bar) {
			baseOnClose(b)
			grpcSrvLocal.PublishBar(b)
		}
		logger.Info("Bar 라이브 publish 활성 (PriceService.SubscribeBar)")
	}

	// 4) Aggregator — broker tick → 봉 누적.
	//    SymbolMap 비어있어도 Aggregator 는 동작 (모든 tick drop). 운영 중 SymbolMap
	//    이 채워지면 즉시 처리 시작.
	agg := price.NewAggregator(symbols, price.JSONCookerDecoder(), onClose)
	go agg.RunSweeper(ctx, cfg.AggregatorSweepInterval)
	srv.AddConsumer(price.TickConsumerFunc(agg.OnTick))
	logger.Info("Aggregator 활성 (JSONCookerDecoder)",
		slog.Duration("sweep", cfg.AggregatorSweepInterval),
		slog.Int("symbols", symbols.Size()),
	)

	// 6) PricingConsumer — etcd watch 또는 정적 파일 둘 다 지원.
	//    broker tick → PricingTable.Apply (Profile 별) → MultiQuotePublisher
	//        → broker (ExchangeQuote)  : 외부 audit / non-edge consumer 용
	//        → gRPC SubscribeQuote     : mci-edge-price 로의 직접 stream
	var pc *price.PricingConsumer
	switch {
	case etcdCli != nil:
		pcInst, tblW, profSrc, err := wirePricingConsumerEtcd(ctx, cfg, symbols, srv, grpcSrv, etcdCli, quoteIDGen, quoteIDReg, logger)
		if err != nil {
			logger.Error("PricingConsumer (etcd) 구성 실패", slog.Any("error", err))
			os.Exit(1)
		}
		pc = pcInst
		etcdTblWatch = tblW
		etcdProfileSrc = profSrc
		srv.AddConsumer(price.TickConsumerFunc(pc.OnTick))
	case cfg.PricingFile != "" && cfg.ProfilesFile != "":
		pcInst, err := wirePricingConsumer(cfg, symbols, srv, grpcSrv, quoteIDGen, quoteIDReg, logger)
		if err != nil {
			logger.Error("PricingConsumer 구성 실패", slog.Any("error", err))
			os.Exit(1)
		}
		pc = pcInst
		srv.AddConsumer(price.TickConsumerFunc(pc.OnTick))
	default:
		logger.Info("PricingConsumer 비활성 (etcd / 정적 파일 모두 미설정)")
	}
	if etcdTblWatch != nil {
		defer etcdTblWatch.Close()
	}
	if etcdProfileSrc != nil {
		defer etcdProfileSrc.Close()
	}

	// 6) Server 시작 (블로킹).
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-price 종료", slog.Any("error", err))
		os.Exit(1)
	}

	// 7) 최종 통계 출력.
	if archiver != nil {
		s := archiver.Stats()
		logger.Info("Archiver 최종 통계",
			slog.Uint64("enqueued", s.Enqueued),
			slog.Uint64("inserted", s.Inserted),
			slog.Uint64("dropped", s.Dropped),
			slog.Uint64("failed", s.Failed),
		)
	}
	if pc != nil {
		s := pc.Stats()
		logger.Info("PricingConsumer 최종 통계",
			slog.Uint64("ticks_in", s.TicksIn),
			slog.Uint64("ticks_dropped", s.TicksDropped),
			slog.Uint64("quotes_published", s.QuotesPublished),
			slog.Uint64("publish_errors", s.PublishErrors),
		)
	}
	logger.Info("mci-price 정상 종료")
}

// wirePricingConsumerEtcd 는 etcd watch 기반 PricingConsumer 를 구성.
// PricingTable 과 ProfileSource 모두 etcd 에서 hot reload.
// 반환된 watcher / source 는 호출자가 Close 책임.
func wirePricingConsumerEtcd(
	ctx context.Context,
	cfg price.Config,
	symbols *quote.SymbolMap,
	srv *price.Server,
	grpcSrv *price.GRPCServer,
	cli *clientv3.Client,
	quoteIDGen *quoteid.Generator,
	quoteIDReg quoteid.Registry,
	logger *slog.Logger,
) (*price.PricingConsumer, *pricing.EtcdTableWatcher, *price.EtcdProfileSource, error) {
	// PricingTable etcd watcher.
	store := pricing.NewStore()
	tblW, err := pricing.NewEtcdTableWatcher(ctx, pricing.EtcdTableWatcherOptions{
		Client: cli,
		Key:    cfg.EtcdPrefix + "pricing/table",
		Store:  store,
		Logger: logger,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("PricingTable watcher: %w", err)
	}

	// Profile etcd watcher.
	profSrc, err := price.NewEtcdProfileSource(ctx, price.EtcdProfileSourceOptions{
		Client: cli,
		Prefix: cfg.EtcdPrefix + "price/profiles/",
		Logger: logger,
	})
	if err != nil {
		_ = tblW.Close()
		return nil, nil, nil, fmt.Errorf("ProfileSource watcher: %w", err)
	}

	publishers := []price.QuotePublisher{price.NewMymqQuotePublisher(srv)}
	if grpcSrv != nil {
		publishers = append(publishers, grpcSrv)
	}
	pc := price.NewPricingConsumer(price.PricingConsumerOptions{
		Store:                  store,
		Symbols:                symbols,
		Decoder:                price.JSONCookerDecoder(),
		Publisher:              price.NewMultiQuotePublisher(publishers...),
		Profiles:               profSrc,
		Logger:                 logger,
		QuoteIDGen:             quoteIDGen,
		QuoteIDRegistry:        quoteIDReg,
		QuoteValidity:          cfg.QuoteIDValidity,
		QuoteRegistryTimeout:   cfg.QuoteIDRegistryTimeout,
	})
	logger.Info("PricingConsumer (etcd watch) 활성",
		slog.String("table_key", cfg.EtcdPrefix+"pricing/table"),
		slog.String("profile_prefix", cfg.EtcdPrefix+"price/profiles/"),
		slog.Bool("grpc_publish", grpcSrv != nil),
	)
	return pc, tblW, profSrc, nil
}

// wirePricingConsumer 는 PricingFile/ProfilesFile 을 읽어 PricingConsumer 를 구성.
// Publisher 는 broker + (있으면) gRPC 둘 다로 fan-out.
func wirePricingConsumer(cfg price.Config, symbols *quote.SymbolMap, srv *price.Server, grpcSrv *price.GRPCServer, quoteIDGen *quoteid.Generator, quoteIDReg quoteid.Registry, logger *slog.Logger) (*price.PricingConsumer, error) {
	// PricingTable 로드.
	tblBody, err := readFile(cfg.PricingFile)
	if err != nil {
		return nil, fmt.Errorf("pricing 파일 읽기: %w", err)
	}
	tbl, err := pricing.ParsePricingTable(tblBody)
	if err != nil {
		return nil, fmt.Errorf("pricing 파일 파싱: %w", err)
	}
	store := pricing.NewStore()
	store.Replace(tbl)

	// 활성 Profile 카탈로그 로드.
	profiles, err := loadProfiles(cfg.ProfilesFile)
	if err != nil {
		return nil, fmt.Errorf("profiles 파일: %w", err)
	}

	// Publisher fan-out — broker 항상, gRPC 는 옵션.
	publishers := []price.QuotePublisher{price.NewMymqQuotePublisher(srv)}
	if grpcSrv != nil {
		publishers = append(publishers, grpcSrv)
	}

	pc := price.NewPricingConsumer(price.PricingConsumerOptions{
		Store:                  store,
		Symbols:                symbols,
		Decoder:                price.JSONCookerDecoder(),
		Publisher:              price.NewMultiQuotePublisher(publishers...),
		Profiles:               &price.StaticProfileSource{Profiles: profiles},
		Logger:                 logger,
		QuoteIDGen:             quoteIDGen,
		QuoteIDRegistry:        quoteIDReg,
		QuoteValidity:          cfg.QuoteIDValidity,
		QuoteRegistryTimeout:   cfg.QuoteIDRegistryTimeout,
	})
	logger.Info("PricingConsumer 활성",
		slog.String("pricing", cfg.PricingFile),
		slog.String("profiles", cfg.ProfilesFile),
		slog.Int64("pricing_version", tbl.Version),
		slog.Int("profile_count", len(profiles)),
		slog.Bool("grpc_publish", grpcSrv != nil),
	)
	return pc, nil
}

// loadProfiles 는 JSON 배열 ([]session.Profile) 을 읽는다.
func loadProfiles(path string) ([]session.Profile, error) {
	body, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var out []session.Profile
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("JSON parse: %w", err)
	}
	return out, nil
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// loadSymbolEntries 는 JSON 배열로 직렬화된 []quote.SymbolEntry 파일을 읽는다.
//
// 파일 예시:
//
//	[
//	  {"symbol":"USDKRW","pair":"USD/KRW","active":true},
//	  {"symbol":"EURKRW","pair":"EUR/KRW","active":true}
//	]
func loadSymbolEntries(path string) ([]quote.SymbolEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	var out []quote.SymbolEntry
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, fmt.Errorf("JSON parse: %w", err)
	}
	return out, nil
}

// wireQuoteID — pkg/quoteid 의 Generator + Registry 를 cfg 에서 구성.
//
//	cfg.QuoteIDInstance == ""  → quoteid 비활성 (nil, nil, no-op closer).
//	cfg.QuoteIDRedisAddr == "" → MemoryRegistry (dev / 단일 인스턴스).
//	cfg.QuoteIDRedisAddr != "" → RedisRegistry (운영 active-active 공유).
//
// closer 는 Redis 클라이언트 lifecycle 정리 — Redis 미사용이면 no-op.
func wireQuoteID(cfg price.Config, logger *slog.Logger) (*quoteid.Generator, quoteid.Registry, func()) {
	if cfg.QuoteIDInstance == "" {
		return nil, nil, func() {}
	}
	gen := quoteid.NewGenerator(cfg.QuoteIDInstance)

	if cfg.QuoteIDRedisAddr == "" {
		reg := quoteid.NewMemoryRegistry(cfg.QuoteIDGrace)
		logger.Info("QuoteID 활성 (MemoryRegistry)",
			slog.String("instance", cfg.QuoteIDInstance),
			slog.Duration("validity", cfg.QuoteIDValidity),
			slog.Duration("grace", cfg.QuoteIDGrace))
		return gen, reg, func() {}
	}

	addrs := splitTrim(cfg.QuoteIDRedisAddr, ",")
	var rdb redis.UniversalClient
	// mode 결정 — 명시 우선, 없으면 addr 수로 auto.
	mode := strings.ToLower(cfg.QuoteIDRedisMode)
	if mode == "" {
		if len(addrs) > 1 {
			mode = "sentinel"
		} else {
			mode = "direct"
		}
	}
	switch mode {
	case "cluster":
		// Redis Cluster — slot 분산. pkg/quoteid 의 키는 `{<id>}` hash tag 를
		// 써서 q / c 두 키가 same slot 으로 보장된다. ClusterClient 가 자동
		// MOVED 처리 + slot 재계산.
		rdb = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    addrs,
			Password: cfg.QuoteIDRedisPassword,
		})
	case "sentinel":
		// Sentinel FailoverClient — master failover 자동.
		rdb = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.QuoteIDRedisMaster,
			SentinelAddrs: addrs,
			Password:      cfg.QuoteIDRedisPassword,
			DB:            cfg.QuoteIDRedisDB,
		})
	default: // direct
		rdb = redis.NewClient(&redis.Options{
			Addr:     addrs[0],
			Password: cfg.QuoteIDRedisPassword,
			DB:       cfg.QuoteIDRedisDB,
		})
	}
	reg := quoteid.NewRedisRegistry(rdb, quoteid.RedisRegistryOptions{
		Prefix: cfg.QuoteIDRedisPrefix,
		Grace:  cfg.QuoteIDGrace,
	})
	logger.Info("QuoteID 활성 (RedisRegistry)",
		slog.String("instance", cfg.QuoteIDInstance),
		slog.String("redis_mode", mode),
		slog.Any("redis_addrs", addrs),
		slog.String("redis_master", cfg.QuoteIDRedisMaster),
		slog.Duration("validity", cfg.QuoteIDValidity),
		slog.Duration("grace", cfg.QuoteIDGrace))
	return gen, reg, func() { _ = rdb.Close() }
}

// splitTrim — sep 으로 split + 각 토큰 trim, 빈 토큰 제외.
func splitTrim(s, sep string) []string {
	raw := strings.Split(s, sep)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
