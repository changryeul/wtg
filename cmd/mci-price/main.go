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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/internal/price"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/otelinit"
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
		slog.Bool("best_enabled", cfg.BestEnabled),
		slog.Duration("best_staleness", cfg.BestMaxStaleness),
		slog.Int("print", cfg.PrintFirstN),
	)

	srv := price.NewServer(cfg, logger)
	if cfg.BestEnabled {
		logger.Info("BestConsumer 활성 — 다중시장 best 호가 산정",
			slog.Duration("max_staleness", cfg.BestMaxStaleness),
		)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// OTel TracerProvider — Endpoint 비면 비활성 (PR 2/3, broker-tracing.md §8).
	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-price",
		cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
		logger); shutdown != nil {
		defer shutdown(ctx)
	}

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
		currencyMaster *pricing.CurrencyMaster
		pairMaster     *pricing.PairMaster
		crossCR        *price.CrossRateConsumer
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

		// Currency master watcher — fx-sync 가 wtg/currency/{code} 에 PUT 한 것 받음.
		currencyMaster = pricing.NewCurrencyMaster()
		currencyWatcher, err := pricing.NewEtcdCurrencyWatcher(ctx, pricing.EtcdCurrencyWatcherOptions{
			Client: etcdCli,
			Prefix: cfg.EtcdPrefix + "currency/",
			M:      currencyMaster,
			Logger: logger,
		})
		if err != nil {
			logger.Warn("CurrencyMaster watcher 시작 실패 — /v1/currency 미노출",
				slog.Any("error", err))
		} else {
			defer currencyWatcher.Close()
			srv.AttachCurrency(currencyMaster)
		}

		// Pair master watcher — fx-sync 가 wtg/pair/{id} 에 PUT 한 것.
		// + CrossRateConsumer 활성: PairMaster 의 cross 산식이 변경될 때마다
		//   ReplaceFormulas 자동 호출 (OnChange callback).
		pairMaster = pricing.NewPairMaster()
		crossCR = price.NewCrossRateConsumer(price.CrossRateOptions{
			Symbols:        symbols,
			Pairs:          pairMaster,
			Logger:         logger,
			MaxStaleness:   cfg.CrossMaxStaleness,
			DebounceWindow: cfg.CrossDebounceWindow,
		})
		srv.AttachCross(crossCR)
		pairWatcher, err := pricing.NewEtcdPairWatcher(ctx, pricing.EtcdPairWatcherOptions{
			Client: etcdCli,
			Prefix: cfg.EtcdPrefix + "pair/",
			M:      pairMaster,
			Logger: logger,
			OnChange: func(m *pricing.PairMaster) {
				// CrossRateConsumer 의 cross 산식 자동 wire.
				crossCR.ReplaceFormulas(m.CrossFormulas())
				// SymbolMap 을 PairMaster 의 derived view 로 — 기존 모든 consumer
				// (Aggregator / PricingConsumer / BestConsumer) 가 direct + cross
				// 모든 pair 의 Symbol→Pair lookup 가능.
				symbols.Replace(m.ToSymbolEntries())
			},
		})
		if err != nil {
			logger.Warn("PairMaster watcher 시작 실패 — /v1/pair 미노출 + cross 합성 비활성",
				slog.Any("error", err))
		} else {
			defer pairWatcher.Close()
			srv.AttachPair(pairMaster)
		}
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

	// Phase A (algo stream) — SubscribeAlgo 를 위한 별 서버. BestConsumer
	// downstream 으로 등록 → 매 tick 을 심볼별 monotonic seq + per-symbol ring
	// 에 저장 후 subscriber 에게 fan-out. Phase B 에서 from_seq > 0 backfill.
	var algoSrv *price.AlgoStreamServer
	if grpcSrv != nil && cfg.AlgoStreamEnabled {
		algoSrv = price.NewAlgoStreamServer(logger, price.AlgoStreamOptions{
			RingSize:          cfg.AlgoRingSize,
			ClientBufferSize:  cfg.AlgoClientBufferSize,
			SlowClientTimeout: cfg.AlgoSlowClientTimeout,
		})
		srv.AddConsumer(algoSrv)    // BEST/CROSS 합성 tick 수신 (BEST 모드 구독자용)
		srv.AddRawConsumer(algoSrv) // raw 원천 tick(SMB/KMB) 수신 (per-source 구독자용, mds excode)
		grpcSrv.AttachAlgo(algoSrv)
		logger.Info("AlgoStream 활성 — SubscribeAlgo",
			slog.Int("ring_size", cfg.AlgoRingSize),
			slog.Int("client_buffer", cfg.AlgoClientBufferSize),
			slog.Duration("slow_timeout", cfg.AlgoSlowClientTimeout))
	}

	// QuoteID stack (pkg/quoteid) — Generator + Registry. cfg.QuoteIDInstance
	// 가 비어 있으면 비활성 (양쪽 nil 반환 → PricingConsumer 가 fallback).
	quoteIDGen, quoteIDReg, quoteIDCloser := wireQuoteID(cfg, srv.Metrics(), logger)
	defer quoteIDCloser()
	if quoteIDReg != nil {
		validator := price.NewQuoteValidationServer(quoteIDReg, logger)
		validator.SetMetrics(srv.Metrics())
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
	// SymbolMap 비어있음 = Aggregator 가 silent drop — INFO 로는 함정.
	// 운영 alert path 에 잡히도록 WARN 으로 올린다.
	if symbols.Size() == 0 {
		mode := "정적 파일 (--symbols)"
		hint := "etc/symbols.json 같은 파일 경로 확인"
		if len(cfg.EtcdEndpoints) > 0 {
			mode = "etcd watch"
			hint = "fx-sync --table=pair --source-dir=./etc/db-mirror 또는 mci-admin UI 로 wtg/pair/ 시드"
		}
		logger.Warn("SymbolMap 비어있음 — Aggregator 가 모든 tick silent drop, quote_bars INSERT 0건",
			slog.String("mode", mode),
			slog.String("조치", hint),
		)
	}

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
		// ProfileSource 비어있음 = PricingConsumer 가 fan-out 대상 0 → 모든 quote silent drop.
		// SymbolMap WARN 과 같은 silent 함정. 운영 alert path 에 잡히도록 WARN.
		if len(profSrc.ActiveProfiles()) == 0 {
			logger.Warn("ProfileSource 비어있음 — PricingConsumer 가 fan-out 대상 0, 모든 마진 적용 quote silent drop",
				slog.String("prefix", cfg.EtcdPrefix+"price/profiles/"),
				slog.String("조치", "etcdctl put 또는 mci-admin UI 로 wtg/price/profiles/ 시드 (channel/site/tier 조합)"),
			)
		}
		// PricingTable 비어있음 = SwapPoint/HQMargin/SiteMargin 모두 0 → 마진 0 으로 raw 가격
		// 그대로 publish. silent drop 은 아니지만 운영 실수 (margin 미반영 quote) 가능.
		if tbl := pcInst.Store().Load(); tbl == nil || (len(tbl.SwapPoint) == 0 && len(tbl.HQMargin) == 0 && len(tbl.SiteMargin) == 0) {
			logger.Warn("PricingTable 비어있음 — 모든 마진 0 으로 raw 가격 publish (고객 quote 와 시장 best 동일)",
				slog.String("key", cfg.EtcdPrefix+"pricing/table"),
				slog.String("조치", "fx-sync --table=hq_margin,site_margin,swap 으로 시드 또는 mci-admin UI"),
			)
		}
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
	// forward-snapshot endpoint 활성화 — Store 공유로 hot reload 자동 반영.
	if pc != nil {
		srv.AttachPricing(pc.Store())
	}
	// 수동 스왑포인트 등록 endpoint (POST /v1/pricing/swap) — etcd 가 있을 때만.
	// 거래 경로 기능이라 admin 이 아닌 price 상주 (딜러/trn 발 cside/wtgswap).
	if etcdCli != nil {
		srv.AttachPricingSwapWriter(&price.EtcdDocStore{
			Cli: etcdCli,
			Key: cfg.EtcdPrefix + "pricing/table",
		})
	}
	// forward/lock endpoint 활성화 — quoteid Generator/Registry 가 있을 때만.
	if quoteIDGen != nil && quoteIDReg != nil {
		srv.AttachQuoteID(quoteIDGen, quoteIDReg, cfg.QuoteIDValidity)
	}
	// S3-b — swap/lock endpoint. EnableSwapLock + Registry 가 SwapIndex 도
	// 구현할 때만 활성. MemoryRegistry 는 둘 다 구현하므로 dev 에서 자동 동작.
	// Redis (S3-c) 도입 후엔 별도 SwapIndex impl 을 주입.
	if cfg.EnableSwapLock && quoteIDReg != nil {
		if idx, ok := quoteIDReg.(quoteid.SwapIndex); ok {
			srv.AttachSwapIndex(idx)
		} else {
			logger.Warn("EnableSwapLock=true 인데 Registry 가 SwapIndex 미구현 — swap endpoint 비활성",
				slog.String("hint", "MemoryRegistry 또는 S3-c Redis SwapIndex 필요"))
		}
	}
	if etcdTblWatch != nil {
		defer etcdTblWatch.Close()
	}
	if etcdProfileSrc != nil {
		defer etcdProfileSrc.Close()
	}

	// P6 운영 메트릭 — Prometheus /metrics 에 cross / pricing / master 노출.
	if err := price.RegisterP6Metrics(srv.Metrics(), price.P6MetricsOpts{
		Cross:    crossCR,
		Pricing:  pc,
		Currency: currencyMaster,
		Pair:     pairMaster,
		Best:     srv.Best(),
		Algo:     algoSrv,
	}); err != nil {
		logger.Warn("P6 메트릭 등록 실패", slog.Any("error", err))
	} else {
		logger.Info("P6 metrics 등록 — wtg_cross_*, wtg_pricing_*, wtg_master_*, wtg_best_*, wtg_algo_*")
	}

	// S3 swap 메트릭 — swap_lock 발급 + ValidateSwap/ConsumeSwap RPC + partial-race.
	// 미주입 컴포넌트는 nil 로 자동 skip.
	if err := price.RegisterSwapMetrics(srv.Metrics(), srv.SwapLockMetrics(), srv.QuoteValidator()); err != nil {
		logger.Warn("Swap 메트릭 등록 실패", slog.Any("error", err))
	} else if srv.SwapLockMetrics() != nil || srv.QuoteValidator() != nil {
		logger.Info("Swap metrics 등록 — wtg_swap_lock_*, wtg_swap_validate_total, wtg_swap_consume_total, wtg_consume_swap_partial_race_total")
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

	// Customer quote publisher 구성 — broker 부하 분리 옵션.
	//   QuotePublishBroker=true  : broker + gRPC (legacy)
	//   QuotePublishBroker=false : gRPC 만 (broker 시세 부하 0 — broker SIGABRT 회피)
	var publishers []price.QuotePublisher
	if cfg.QuotePublishBroker {
		publishers = append(publishers, price.NewMymqQuotePublisher(srv))
	}
	if grpcSrv != nil {
		publishers = append(publishers, grpcSrv)
	}
	opts := price.PricingConsumerOptions{
		Store:                store,
		Symbols:              symbols,
		Decoder:              price.JSONCookerDecoder(),
		Publisher:            price.NewMultiQuotePublisher(publishers...),
		Profiles:             profSrc,
		Logger:               logger,
		QuoteIDGen:           quoteIDGen,
		QuoteIDRegistry:      quoteIDReg,
		QuoteValidity:        cfg.QuoteIDValidity,
		QuoteRegistryTimeout: cfg.QuoteIDRegistryTimeout,
		TickBufferSize:       cfg.PricingBufferSize,
	}
	// Phase 4b — gRPC 활성 시 customer fan-out path 동시 연결.
	if grpcSrv != nil {
		opts.CustomerRegistry = grpcSrv.CustomerRegistry()
		opts.CustomerPub = grpcSrv
	}
	pc := price.NewPricingConsumer(opts)
	logger.Info("PricingConsumer (etcd watch) 활성",
		slog.String("table_key", cfg.EtcdPrefix+"pricing/table"),
		slog.String("profile_prefix", cfg.EtcdPrefix+"price/profiles/"),
		slog.Bool("grpc_publish", grpcSrv != nil),
		slog.Bool("customer_fanout", grpcSrv != nil),
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
	// Customer quote publisher 구성 — broker 부하 분리 옵션.
	//   QuotePublishBroker=true  : broker + gRPC (legacy)
	//   QuotePublishBroker=false : gRPC 만 (broker 시세 부하 0 — broker SIGABRT 회피)
	var publishers []price.QuotePublisher
	if cfg.QuotePublishBroker {
		publishers = append(publishers, price.NewMymqQuotePublisher(srv))
	}
	if grpcSrv != nil {
		publishers = append(publishers, grpcSrv)
	}

	opts := price.PricingConsumerOptions{
		Store:                store,
		Symbols:              symbols,
		Decoder:              price.JSONCookerDecoder(),
		Publisher:            price.NewMultiQuotePublisher(publishers...),
		Profiles:             &price.StaticProfileSource{Profiles: profiles},
		Logger:               logger,
		QuoteIDGen:           quoteIDGen,
		QuoteIDRegistry:      quoteIDReg,
		QuoteValidity:        cfg.QuoteIDValidity,
		QuoteRegistryTimeout: cfg.QuoteIDRegistryTimeout,
		TickBufferSize:       cfg.PricingBufferSize,
	}
	// Phase 4b — gRPC 활성 시 customer fan-out path 동시 연결.
	if grpcSrv != nil {
		opts.CustomerRegistry = grpcSrv.CustomerRegistry()
		opts.CustomerPub = grpcSrv
	}
	pc := price.NewPricingConsumer(opts)
	logger.Info("PricingConsumer 활성",
		slog.String("pricing", cfg.PricingFile),
		slog.String("profiles", cfg.ProfilesFile),
		slog.Int64("pricing_version", tbl.Version),
		slog.Int("profile_count", len(profiles)),
		slog.Bool("grpc_publish", grpcSrv != nil),
		slog.Bool("customer_fanout", grpcSrv != nil),
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
func wireQuoteID(cfg price.Config, mreg *metrics.Registry, logger *slog.Logger) (*quoteid.Generator, quoteid.Registry, func()) {
	// DevMode 자동 활성 — instance 가 비어있어도 "dev" 로 fallback.
	// admin UI 의 [QuoteID Validate 통계] 페이지가 미리 동작하도록 친절.
	// 운영은 명시 instance (예: A/B) 만 사용하므로 영향 X.
	if cfg.QuoteIDInstance == "" && cfg.DevMode {
		cfg.QuoteIDInstance = "dev"
		logger.Info("DevMode — QuoteID instance 자동 활성",
			slog.String("instance", "dev"),
			slog.String("note", "운영은 --quoteid-instance=A/B 등 명시"))
	}
	if cfg.QuoteIDInstance == "" {
		logger.Warn("QuoteID 비활성 — --quoteid-instance 미설정 → /v1/quoteid/* 모두 404",
			slog.String("hint", "운영: --quoteid-instance=A 또는 B. dev: --dev 자동 fallback"))
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
	var reg quoteid.Registry = quoteid.NewRedisRegistry(rdb, quoteid.RedisRegistryOptions{
		Prefix: cfg.QuoteIDRedisPrefix,
		Grace:  cfg.QuoteIDGrace,
	})

	closeAsync := func(_ context.Context) error { return nil }
	// AsyncRegistry — Redis 모드에서만 의미 (Memory 는 RTT 없음).
	if cfg.QuoteIDAsyncQueue > 0 {
		async := quoteid.NewAsyncRegistry(reg, quoteid.AsyncRegistryOptions{
			QueueSize:     cfg.QuoteIDAsyncQueue,
			FlushInterval: cfg.QuoteIDAsyncFlush,
			BatchMax:      cfg.QuoteIDAsyncBatchMax,
			PutTimeout:    cfg.QuoteIDAsyncTimeout,
			Logger:        logger,
		})
		// Prometheus hook — async 카운터 + queue_len gauge.
		if mreg != nil {
			async.SetMetricsHook(quoteid.AsyncMetricsHook{
				Enqueued: func(n uint64) { mreg.IncQuoteIDAsync("mci-price", "enqueued", n) },
				Dropped:  func(n uint64) { mreg.IncQuoteIDAsync("mci-price", "dropped", n) },
				Written:  func(n uint64) { mreg.IncQuoteIDAsync("mci-price", "written", n) },
				Failed:   func(n uint64) { mreg.IncQuoteIDAsync("mci-price", "failed", n) },
			})
			if err := mreg.RegisterAsyncQueueGauge("mci-price", func() float64 {
				return float64(async.QueueLen())
			}); err != nil {
				logger.Warn("async queue gauge 등록 실패", slog.Any("error", err))
			}
		}
		reg = async
		closeAsync = async.Close
		logger.Info("QuoteID Async wrapper 활성 (Put 비동기 batch)",
			slog.Int("queue", cfg.QuoteIDAsyncQueue),
			slog.Duration("flush", cfg.QuoteIDAsyncFlush),
			slog.Int("batch_max", cfg.QuoteIDAsyncBatchMax),
			slog.Duration("put_timeout", cfg.QuoteIDAsyncTimeout))
	}

	logger.Info("QuoteID 활성 (RedisRegistry)",
		slog.String("instance", cfg.QuoteIDInstance),
		slog.String("redis_mode", mode),
		slog.Any("redis_addrs", addrs),
		slog.String("redis_master", cfg.QuoteIDRedisMaster),
		slog.Duration("validity", cfg.QuoteIDValidity),
		slog.Duration("grace", cfg.QuoteIDGrace),
		slog.Bool("async", cfg.QuoteIDAsyncQueue > 0))
	return gen, reg, func() {
		// graceful drain — 1초 안에 남은 batch flush, 못 비우면 손실.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = closeAsync(ctx)
		_ = rdb.Close()
	}
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
