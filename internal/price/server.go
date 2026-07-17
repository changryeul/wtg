package price

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Subscriber 는 mymq.Client 의 unsolicited 채널 추상화 (테스트 mock 가능).
type Subscriber interface {
	Subscribe() <-chan *mymq.Unsolicited
}

// TickConsumer 는 정규화된 Tick 을 받아가는 다운스트림 인터페이스.
//
// Phase 4 1차 prototype: stdout dump / stats / 단위 테스트용 collector.
// Phase 5 (DMZ edge) 에서 gRPC stream 구현체로 대체.
type TickConsumer interface {
	OnTick(*Tick)
}

// TickConsumerFunc 는 함수를 TickConsumer 로 어댑트.
type TickConsumerFunc func(*Tick)

func (f TickConsumerFunc) OnTick(t *Tick) { f(t) }

// Server 는 mci-price 의 핵심 컴포넌트.
//
// 흐름:
//
//	mymq Subscribe → 필터 (Func == FCCast/FCPush, Exchange == cfg.Exchange)
//	  → DecodePushData → Conflation.Update → consumer.OnTick
//	  → 통계 카운터
//
// HTTP 서버는 헬스체크 + stats 만 제공 (별도 fanout 인터페이스는 consumer 로
// 추상화).
type Server struct {
	cfg    Config
	logger *slog.Logger

	mq         *mymq.Client
	conflation *Conflation
	consumers  []TickConsumer
	best       *BestConsumer // cfg.BestEnabled 이면 NewServer 가 생성, AddConsumer
	// 가 best.downstream 에 라우팅. nil 이면 raw fan-out (이전 동작).
	grpcSrv *GRPCServer // 외부 주입 가능 (AttachGRPC); nil 이면 Start 가 자동 생성.
	metrics *metrics.Registry

	// quoteValidator — HTTP gateway (POST /v1/quoteid/*) 로 외부 노출하기 위한
	// 옵션 attach. 동일 인스턴스를 gRPC (GRPCServer.AttachValidator) 와
	// HTTP (이 필드) 양쪽에 attach 가능 — 두 transport 가 같은 카운터 / 같은
	// Registry 를 공유.
	quoteValidator *QuoteValidationServer

	// pricingStore — forward-snapshot endpoint 등 외부 노출용. AttachPricing 으로
	// 주입. nil 이면 forward-snapshot 라우트 미등록.
	pricingStore *pricing.Store

	// currencyMaster — fx-sync 가 미러링한 통화 마스터. AttachCurrency 로 주입.
	// nil 이면 /v1/currency 미노출.
	currencyMaster *pricing.CurrencyMaster

	// pairMaster — fx-sync 가 미러링한 통화쌍 마스터. AttachPair 로 주입.
	// nil 이면 /v1/pair 미노출.
	pairMaster *pricing.PairMaster

	// crossConsumer — direct leg → cross 자동 합성. AttachCross 로 주입.
	// nil 이면 cross fan-out 비활성 (기존 동작). BestConsumer 의 downstream 으로
	// 자동 등록 + AddConsumer 가 cross.AddDownstream 도 자동 호출.
	crossConsumer *CrossRateConsumer

	// QuoteID 발급/등록 — forward/lock endpoint 용. AttachQuoteID 로 주입.
	// 모두 nil 이면 lock endpoint 미등록.
	quoteIDGen      *quoteid.Generator
	quoteIDReg      quoteid.Registry
	quoteIDValidity time.Duration

	// S3-b — swap/lock endpoint 용. AttachSwapIndex 로 주입. nil 이면 swap
	// endpoint 미등록 (forward/lock 은 영향 X).
	swapIndex      quoteid.SwapIndex
	swapMetrics    *AtomicSwapLockMetrics
	swapPointStore DocStore   // POST /v1/pricing/swap 의 etcd 저장소 (nil=503)
	swapStore      *SwapStore // 로이터 수신 + 운영자 delta swap (AttachSwapStore, nil=미노출)

	totalRecv  atomic.Uint64
	totalMatch atomic.Uint64 // exchange 필터 통과 건수
	totalDrop  atomic.Uint64 // 디코딩 실패 등
	totalTicks atomic.Uint64 // 실제 처리된 envelope (tick) 수.

	// e2e latency — envelope.ts (cooker 측 시각) vs IngestEnvelopes 진입 시각.
	// 운영 가시화 — broker vs grpc path 의 정량적 비교, P99 추세 모니터링.
	latency LatencyTracker
	// batch 1개 broker message 가 N envelope 을 담을 수 있으므로
	// totalRecv (broker message 수) 와 분리. delivery% 측정 시 sent UDP
	// 와 비교할 대상은 totalTicks 쪽.

	http *http.Server
}

// NewServer 는 Server 를 구성 (broker 미접속 상태).
//
// cfg.BestEnabled 이면 BestConsumer 가 단일 hot-path consumer 로 자리잡고,
// 이후 AddConsumer 호출은 모두 BestConsumer.downstream 으로 라우팅된다.
// 그래야 raw 다중시장 tick 들이 BestConsumer 에서 흡수된 뒤 합성 BEST 만
// Aggregator/PricingConsumer/gRPC 에 전달된다.
func NewServer(cfg Config, logger *slog.Logger, consumers ...TickConsumer) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:        cfg,
		logger:     logger,
		conflation: NewConflation(),
		metrics:    metrics.NewRegistry(),
	}
	if cfg.BestEnabled {
		s.best = NewBestConsumer(BestOptions{
			MaxStaleness: cfg.BestMaxStaleness,
			Logger:       logger,
			Dedup: DedupOptions{
				Enabled:            cfg.BestDedupEnabled,
				TickSizeMultiplier: cfg.BestDedupTickSizeMultiplier,
			},
		}, consumers...)
		s.consumers = []TickConsumer{s.best}
	} else {
		s.consumers = consumers
	}
	return s
}

// AddConsumer 는 Tick 다운스트림 소비자를 추가한다 (Start 전 호출).
//
// BestConsumer 활성 시 등록 대상은 BestConsumer.downstream — Aggregator 등은
// raw 가 아닌 합성 BEST Tick 만 받는다. 비활성 시 (테스트/회귀 호환) 기존
// 동작과 동일하게 hot path consumer 리스트에 직접 추가.
//
// CrossRateConsumer 활성 시 (AttachCross 호출됨), 추가되는 consumer 는 cross
// 합성 결과도 받아야 하므로 crossConsumer.AddDownstream 도 동시에 호출.
// 결과: Aggregator/PricingConsumer/gRPC 모두 direct BEST + cross 합성 둘 다
// 단일 hot path 로 수신.
func (s *Server) AddConsumer(c TickConsumer) {
	if s.best != nil {
		s.best.AddDownstream(c)
		if s.crossConsumer != nil {
			s.crossConsumer.AddDownstream(c)
		}
		return
	}
	s.consumers = append(s.consumers, c)
}

// AddRawConsumer 는 BestConsumer *이전* 의 raw hot-path 에 consumer 를 추가한다 —
// Source=SMB/KMB 등 원천별 tick 을 그대로 받는다 (BestConsumer 가 흡수하기 전).
// AlgoStream 의 per-source 모드(mds excode 대응)처럼 원천 구분이 필요한 소비자용.
// best 비활성 시엔 AddConsumer 와 동일 (이미 raw path).
func (s *Server) AddRawConsumer(c TickConsumer) {
	s.consumers = append(s.consumers, c)
}

// AttachGRPC 는 외부에서 생성한 GRPCServer 를 주입한다.
//
// 사용 패턴: main.go 가 GRPCServer 를 미리 만들어두면 PricingConsumer 등이
// 그 인스턴스를 QuotePublisher 로 직접 참조할 수 있다. Server 는 Start 시
// 이 인스턴스를 TickConsumer 로 등록하고 cfg.GRPCAddr 가 있으면 Serve 한다.
//
// 호출하지 않으면 Start 가 자동으로 GRPCServer 를 생성한다 (back-compat).
// 중복 호출은 무시.
func (s *Server) AttachGRPC(g *GRPCServer) {
	if s.grpcSrv != nil || g == nil {
		return
	}
	s.grpcSrv = g
	s.AddConsumer(g)
	g.AttachServer(s) // PublishTick handler 가 hot path 에 dispatch 할 수 있도록.
}

// AttachQuoteValidator — QuoteValidationServer 를 HTTP gateway 로도 노출.
// nil 이면 무시. Start 전에 호출.
// AttachPricing — forward-snapshot endpoint 가 사용할 PricingTable Store 주입.
// cmd/mci-price 의 bootstrap 에서 호출. 미주입이면 forward-snapshot 미노출 (404).
func (s *Server) AttachPricing(store *pricing.Store) {
	s.pricingStore = store
}

// AttachCurrency — /v1/currency REST endpoint 가 사용할 master 주입.
// cmd/mci-price 가 fx-sync 의 etcd watcher 와 같은 인스턴스 주입.
func (s *Server) AttachCurrency(m *pricing.CurrencyMaster) {
	s.currencyMaster = m
}

// AttachPair — /v1/pair REST endpoint 가 사용할 master 주입.
func (s *Server) AttachPair(m *pricing.PairMaster) {
	s.pairMaster = m
}

// AttachCross — CrossRateConsumer 를 hot path 에 끼움.
//
// 동작:
//  1. BestConsumer 의 downstream 에 cross consumer 등록 → direct BEST tick
//     이 cross consumer 의 leg cache 로 흐름.
//  2. AddConsumer 가 향후 추가되는 consumer 를 cross consumer 의 downstream
//     에도 등록 → cross 합성 tick 도 Aggregator/PricingConsumer/gRPC 로 흐름.
//
// AttachCross 는 Start 전, AddConsumer 호출 전에 불러야 한다 — 이후 등록되는
// consumer 들이 cross 도 받게 됨.
func (s *Server) AttachCross(c *CrossRateConsumer) {
	if c == nil || s.crossConsumer != nil {
		return
	}
	s.crossConsumer = c
	if s.best != nil {
		s.best.AddDownstream(c)
	}
}

// CrossConsumer — 외부에서 stats 조회 등에 사용. nil 가능.
func (s *Server) CrossConsumer() *CrossRateConsumer { return s.crossConsumer }

// AttachQuoteID — forward/lock endpoint 의 QuoteID 발급/등록자 주입. validity 가
// 0 이면 500ms default. gen+reg 둘 다 있어야 endpoint 활성.
func (s *Server) AttachQuoteID(gen *quoteid.Generator, reg quoteid.Registry, validity time.Duration) {
	s.quoteIDGen = gen
	s.quoteIDReg = reg
	if validity <= 0 {
		validity = 500 * time.Millisecond
	}
	s.quoteIDValidity = validity
}

func (s *Server) AttachQuoteValidator(v *QuoteValidationServer) {
	s.quoteValidator = v
}

// AttachPricingSwapWriter — POST /v1/pricing/swap 의 etcd 저장소 주입.
// nil 이면 endpoint 는 등록되지만 503 (etcd 미구성) 응답.
func (s *Server) AttachPricingSwapWriter(store DocStore) {
	s.swapPointStore = store
}

// AttachSwapStore — swap store(received+delta) 주입. 수신/조정/조회 endpoint 노출
// + AlgoStream SwapProvider 의 effective 원천.
func (s *Server) AttachSwapStore(store *SwapStore) {
	s.swapStore = store
}

// registerSwapPointRoutes — 수동 스왑포인트 등록 (딜러/trn 발, cside/wtgswap).
// 거래 경로 기능이라 admin 이 아닌 mci-price 상주 (admin 은 비필수 콘솔).
func (s *Server) registerSwapPointRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/pricing/swap",
		SwapPointHandler(SwapPointDeps{Store: s.swapPointStore, Logger: s.logger}, s.cfg.DevMode))
	// swap store — 로이터 수신 주입 / 운영자 delta 설정 / 병합 조회 (admin 화면).
	mux.HandleFunc("POST /v1/pricing/swap-received", SwapReceivedInjectHandler(s.swapStore))
	mux.HandleFunc("POST /v1/pricing/swap-delta", SwapDeltaHandler(s.swapStore))
	mux.HandleFunc("GET /v1/pricing/swap", SwapViewHandler(s.swapStore))
	var statsFn func() BestStats
	if s.best != nil {
		statsFn = s.best.Stats
	}
	mux.HandleFunc("GET /v1/pricing/market-status", MarketStatusHandler(statsFn, s.cfg.DevMode))
}

// registerSwapLockRoutes — S3-b. swap/lock endpoint + stats 등록.
// 별도 메서드로 분리해 단위 테스트가 mux 직접 검증 가능. EnableSwapLock 와
// 모든 deps 가 채워졌을 때만 라우트 등록 — 그 외엔 no-op.
func (s *Server) registerSwapLockRoutes(mux *http.ServeMux) {
	if !s.cfg.EnableSwapLock {
		return
	}
	if s.pricingStore == nil || s.best == nil ||
		s.quoteIDGen == nil || s.quoteIDReg == nil || s.swapIndex == nil {
		return
	}
	if s.swapMetrics == nil {
		s.swapMetrics = &AtomicSwapLockMetrics{}
	}
	mux.HandleFunc("POST /v1/quote/swap/lock",
		SwapLockHandler(SwapLockDeps{
			Store:      s.pricingStore,
			Best:       s.best,
			Cross:      s.crossConsumer,
			Gen:        s.quoteIDGen,
			Reg:        s.quoteIDReg,
			Idx:        s.swapIndex,
			Validity:   s.quoteIDValidity,
			PutTimeout: s.cfg.QuoteIDRegistryTimeout,
			SpotDays:   2, // S3 1차: T+2 고정. pair 별 컨벤션은 후속.
			Metrics:    s.swapMetrics,
		}, s.cfg.DevMode))
	mux.HandleFunc("GET /v1/quote/swap/stats", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		writeJSON(w, http.StatusOK, s.swapMetrics.Snapshot())
	})
	s.logger.Info("Swap quote-lock endpoint 활성 — POST /v1/quote/swap/lock + GET /v1/quote/swap/stats",
		slog.Duration("validity", s.quoteIDValidity))
}

// AttachSwapIndex — S3-b. swap/lock endpoint 의 swap_id 인덱스 store 주입.
// nil 또는 EnableSwapLock=false 면 endpoint 미등록 — forward/lock 은 영향 X.
// MemoryRegistry 는 Registry + SwapIndex 둘 다 구현하므로 같은 인스턴스를
// AttachQuoteID + AttachSwapIndex 양쪽에 주입 가능 (운영 = Redis SwapIndex
// 도입 S3-c 후 분리).
func (s *Server) AttachSwapIndex(idx quoteid.SwapIndex) {
	s.swapIndex = idx
	if s.swapMetrics == nil {
		s.swapMetrics = &AtomicSwapLockMetrics{}
	}
	// QuoteValidationServer 가 주입돼 있으면 swap RPC 활성화도 같이.
	if s.quoteValidator != nil {
		s.quoteValidator.SetSwapIndex(idx)
	}
}

// SwapLockMetrics — admin/Prometheus exporter 가 stats 조회. nil 가능.
func (s *Server) SwapLockMetrics() *AtomicSwapLockMetrics { return s.swapMetrics }

// QuoteValidator — 현재 attached 된 QuoteValidationServer. nil 가능 — Prometheus
// 메트릭 등록 등 외부 wire 용.
func (s *Server) QuoteValidator() *QuoteValidationServer { return s.quoteValidator }

// Metrics — Prometheus Registry 노출. main.go 가 validator 에 주입 시 사용.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// Best — BestConsumer 노출 (P6Metrics 등록용). BestEnabled=false 면 nil.
func (s *Server) Best() *BestConsumer { return s.best }

// GRPCServer 는 현재 attached 된 GRPCServer 를 반환 (Start 이후 자동생성 포함).
// nil 이면 활성화되지 않은 상태.
func (s *Server) GRPCServer() *GRPCServer { return s.grpcSrv }

// Conflation 은 외부에서 latest 시세 조회용 (모니터링/디버깅).
func (s *Server) Conflation() *Conflation { return s.conflation }

// Send 는 underlying mymq.Client 를 통해 frame 송신.
// MymqQuotePublisher 등이 Server 를 publisher 로 사용할 수 있게 한다.
// Server.Start 가 broker 와 connect 한 이후에만 동작한다 — 호출 시점 보장은 호출자 책임
// (보통 broker subscribe 가 시작된 후 hot path 에서 호출됨).
func (s *Server) Send(in *mymq.FrameInput) error {
	if s.mq == nil {
		return errors.New("price: Server.mq 미초기화 — Start 이전 호출 금지")
	}
	return s.mq.Send(in)
}

// Start 는 broker 연결 + 구독 + HTTP 서버를 가동한다 (블로킹).
func (s *Server) Start(ctx context.Context) error {
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	if !s.cfg.NoBroker {
		brokerTLS, err := loadBrokerTLS(&s.cfg)
		if err != nil {
			return fmt.Errorf("broker TLS 구성: %w", err)
		}
		mq, err := mymq.Open(ctx, s.cfg.BrokerHost, s.cfg.BrokerPort, mymq.Options{
			ApplName:         s.cfg.ApplName,
			Instance:         s.cfg.Instance,
			Channel:          mymq.ChannelWeb,
			DialTimeout:      s.cfg.DialTimeout,
			HandshakeTimeout: s.cfg.HandshakeTimeout,
			Logger:           s.logger,
			TLS:              brokerTLS,
			// SubBufferSize — broker → mci-price unsolicited 채널 깊이. 0 이면
			// pkg/mymq default 256. 부하 테스트 (cmd/load-gen) 에서 broker→client
			// drop 이 보이면 이 값을 올리고 SubDrops() 가 0 으로 떨어지는지 재측정.
			SubBufferSize: s.cfg.SubBufferSize,
			Queue: &mymq.QueueOptions{
				Name: s.cfg.QueueName,
				Attr: mymq.QtClient,
				// QfUnsolRep 가 핵심 — broker 의 representative receiver 로 등록되어
				// LogonID 매칭 없이 모든 broadcast (forwarder/cooker 의 raw 시세) 를 수신.
				// 빠지면 broker 가 broadcast 를 보내주지 않아 recv 가 0 으로 막힌다.
				// mci-push 와 동일 패턴 (internal/push/server.go).
				Flags: mymq.QfUnsolMsg | mymq.QfUnsolHdr | mymq.QfUnsolRep,
				// Exchange declare — broker 의 publish_packet 이 TO_EXCH 매칭 시
				// client->xchg 와 strcasecmp 한다 (publish.c:223). 빈값이면 publish
				// 마다 0/N 으로 skip 되어 recv 가 영원히 0. cfg.ExchangeName 을
				// declare 해서 그 exchange 의 broadcast 만 받게 한다 (FANOUT).
				ExchangeName: s.cfg.ExchangeName,
				ExchangeType: mymq.ExchangeFanout,
			},
			Reconnect: &mymq.ReconnectOptions{
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     30 * time.Second,
				BackoffFactor:  2.0,
			},
			Metrics: mymq.MetricsHook{
				OnDisconnect:       func(_ error) { s.metrics.IncBrokerDisconnect("mci-price") },
				OnReconnect:        func(_ int, d time.Duration) { s.metrics.IncBrokerReconnect("mci-price", d) },
				OnInflightAborted:  func(n int) { s.metrics.IncBrokerInflightAborted("mci-price", n) },
				OnHeartbeatTimeout: func() { s.metrics.IncBrokerHeartbeatTimeout("mci-price") },
			},
		})
		if err != nil {
			return fmt.Errorf("mymq.Open: %w", err)
		}
		s.mq = mq

		// 구독 goroutine — broker subscribe path.
		go s.subscribeLoop(subCtx, mq)
	} else {
		s.logger.Warn("mci-price: broker 비활성 모드 (--no-broker) — 입력 source = gRPC PublishTick + HTTP DevTick only")
	}

	// 통계 출력 goroutine.
	if s.cfg.StatsInterval > 0 {
		go s.statsLoop(subCtx)
	}

	// gRPC PriceService — TickConsumer 로 등록 후 Subscribe/SubscribeQuote stream 노출.
	if s.cfg.GRPCAddr != "" {
		if s.grpcSrv == nil {
			// 외부에서 AttachGRPC 호출 없으면 auto-create (back-compat).
			s.grpcSrv = NewGRPCServer(s.logger, s.cfg.GRPCBufSize)
			s.AddConsumer(s.grpcSrv)
			s.grpcSrv.AttachServer(s) // PublishTick handler dispatch path.
		}

		// OTel — 모든 gRPC 호출에 server-side trace + metrics. otelinit 으로
		// TracerProvider 등록 안 됐으면 no-op. grpc/otelgrpc v0.59 의 stats handler
		// 가 unary / streaming 모두 자동 처리 — interceptor 보다 권장 패턴.
		grpcOpts := []grpc.ServerOption{
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
		}
		if s.cfg.GRPCTLSCertFile != "" && s.cfg.GRPCTLSKeyFile != "" {
			tlsCfg, err := tlsutil.LoadServer(tlsutil.ServerOptions{
				CertFile:     s.cfg.GRPCTLSCertFile,
				KeyFile:      s.cfg.GRPCTLSKeyFile,
				ClientCAFile: s.cfg.GRPCTLSClientCAFile,
			})
			if err != nil {
				return fmt.Errorf("gRPC TLS 구성: %w", err)
			}
			grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
			s.logger.Info("gRPC TLS 활성화",
				slog.String("addr", s.cfg.GRPCAddr),
				slog.Bool("mtls", s.cfg.GRPCTLSClientCAFile != ""),
			)
		}

		go func() {
			if err := s.grpcSrv.Serve(subCtx, s.cfg.GRPCAddr, grpcOpts...); err != nil {
				s.logger.Error("gRPC 서버 종료", slog.Any("error", err))
			}
		}()
	}

	// HTTP 서버.
	if err := s.startHTTP(ctx); err != nil {
		return err
	}
	return nil
}

// subscribeLoop 는 mymq Subscribe 를 소비해서 conflation + consumer 호출.
func (s *Server) subscribeLoop(ctx context.Context, sub Subscriber) {
	ch := sub.Subscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				s.logger.Info("Subscribe 채널 종료 — subscribeLoop 종료")
				return
			}
			s.totalRecv.Add(1)
			s.handleUnsolicited(msg)
		}
	}
}

// handleUnsolicited 는 단일 unsolicited 메시지를 처리.
func (s *Server) handleUnsolicited(msg *mymq.Unsolicited) {
	// FC_CAST / FC_PUSH 만 처리.
	switch msg.Header.Func {
	case mymq.FCCast, mymq.FCPush:
	default:
		return
	}

	// Exchange 필터 (cfg.ExchangeName 비어있으면 모두 통과).
	if s.cfg.ExchangeName != "" {
		if msg.Prefix == nil || msg.Prefix.ExchangeString() != s.cfg.ExchangeName {
			return
		}
	}
	s.totalMatch.Add(1)

	tick, err := DecodePushData(msg.Body)
	if err != nil {
		s.totalDrop.Add(1)
		s.logger.Debug("pushdata 디코딩 실패",
			slog.Any("error", err),
			slog.Int("body_len", len(msg.Body)),
		)
		return
	}
	s.IngestEnvelopes(tick.Body, tick)
}

// IngestEnvelopes 는 raw envelope body (pushdata.msgb 또는 grpc Tick.body) 를
// 받아 v1 envelope 들로 파싱한 뒤 hot path consumer (BestConsumer) 에 dispatch.
//
// 두 진입점이 공유:
//  1. broker subscribe (handleUnsolicited) — pushdata 디코딩 후 호출
//  2. PublishTick gRPC (GRPCServer.PublishTick) — forwarder 가 broker 우회로 직접 push
//
// baseTick 은 메타데이터 (MarketID/SeqNum/Mask/Type/Flag) 만 사용 — Body 는
// 본 함수가 envelope 별로 새로 인코딩하므로 무시.
func (s *Server) IngestEnvelopes(body []byte, baseTick *Tick) {
	envs, err := ParseEnvelopes(body)
	if err != nil {
		s.totalDrop.Add(1)
		s.logger.Debug("envelope 파싱 실패",
			slog.Any("error", err),
			slog.Int("body_len", len(body)),
		)
		return
	}

	ingressTS := time.Now()
	for _, env := range envs {
		// e2e latency — cooker 가 매긴 ts vs 본 함수 진입 시각.
		s.latency.Observe(env.TS, ingressTS)
		// envelope 별 sub-tick — Symbol/Source 를 envelope 으로 덮어쓴다.
		// (batch 일 때 baseTick.Symbol 은 첫 envelope 만 반영하므로 per-envelope
		// sym 으로 SymbolMap lookup 해야 정확.)
		encBody, mErr := quote.EncodeJSONEnvelope(env)
		if mErr != nil {
			s.totalDrop.Add(1)
			continue
		}
		// envelope 의 seq 를 우선 사용 — cooker/forwarder 가 매긴 cooker-side seq.
		// baseTick.SeqNum 은 broker 측 batch 첫 envelope 만 반영하므로 batch 의
		// 두 번째 envelope 부터 부정확. grpc PublishTick path 에선 0 으로 들어옴.
		seqNum := uint32(env.Seq)
		if seqNum == 0 {
			seqNum = baseTick.SeqNum
		}
		sub := &Tick{
			MarketID: baseTick.MarketID,
			Symbol:   env.Sym,
			SeqNum:   seqNum,
			Mask:     baseTick.Mask,
			Type:     baseTick.Type,
			Flag:     baseTick.Flag,
			Body:     encBody,
			Source:   env.Src,
			Received: time.Now(),
		}
		s.totalTicks.Add(1)
		s.conflation.Update(sub)
		for _, c := range s.consumers {
			c.OnTick(sub)
		}
	}
}

// statsLoop 는 주기적으로 통계 로그를 출력한다.
func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.StatsInterval)
	defer t.Stop()
	var prevRecv uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			recv := s.totalRecv.Load()
			match := s.totalMatch.Load()
			drop := s.totalDrop.Load()
			cstats := s.conflation.Stats()
			rate := float64(recv-prevRecv) / s.cfg.StatsInterval.Seconds()
			prevRecv = recv

			s.logger.Info("price stats",
				slog.Time("ts", now),
				slog.Uint64("recv", recv),
				slog.Uint64("match", match),
				slog.Uint64("drop", drop),
				slog.Float64("rate_per_sec", rate),
				slog.Uint64("symbols", cstats.Symbols),
				slog.Uint64("conflation_swaps", cstats.Swaps),
			)
		}
	}
}

// Stats 는 Server 의 누적 카운터 + conflation 스냅샷.
type Stats struct {
	Received uint64          `json:"received"` // broker message 수
	Matched  uint64          `json:"matched"`  // exchange 필터 통과
	Dropped  uint64          `json:"dropped"`  // 디코딩 실패
	Ticks    uint64          `json:"ticks"`    // 실제 envelope (tick) 수 — batch 펼친 후
	Conf     ConflationStats `json:"conflation"`

	// SubDrops — pkg/mymq.Client.subCh 가 가득 차서 broker 쪽에서 drop 된
	// 누적 메시지 수. backpressure 진단용. 0 이 정상, 증가하면 SubBufferSize
	// 를 늘리거나 subscribeLoop 처리 속도 개선 필요.
	SubDrops      uint64 `json:"sub_drops"`
	SubBufferSize int    `json:"sub_buffer_size"`

	// Latency — envelope.ts → IngestEnvelopes 진입 시각 e2e 지연.
	// broker vs grpc path 의 정량적 비교 지표.
	Latency LatencySnapshot `json:"latency"`
}

// Stats 는 외부 노출용 카운터.
func (s *Server) Stats() Stats {
	var subDrops uint64
	var subBuf int
	if s.mq != nil {
		subDrops = s.mq.SubDrops()
		subBuf = s.mq.SubBufferCapacity()
	}
	return Stats{
		Received:      s.totalRecv.Load(),
		Matched:       s.totalMatch.Load(),
		Dropped:       s.totalDrop.Load(),
		Ticks:         s.totalTicks.Load(),
		Conf:          s.conflation.Stats(),
		SubDrops:      subDrops,
		SubBufferSize: subBuf,
		Latency:       s.latency.Snapshot(),
	}
}

// startHTTP 는 헬스체크 + stats endpoint 만 제공.
func (s *Server) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-price",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/price-stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats())
	})
	// BestConsumer 활성 시 per-symbol best snapshot 노출 (디버그 / 운영 가시성).
	if s.best != nil {
		mux.HandleFunc("GET /v1/best-stats", func(w http.ResponseWriter, r *http.Request) {
			// DevMode 한정 CORS 허용 — 같은 호스트의 다른 포트 (mci-admin 9090)
			// 가 자기 origin 으로 직접 fetch 할 수 있게. 운영은 reverse-proxy 단일
			// origin 권장 (Access-Control-* 헤더 노출 X).
			if s.cfg.DevMode {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			writeJSON(w, http.StatusOK, s.best.Stats())
		})
	}

	// Currency master 노출 — fx-sync 가 미러링한 통화 카탈로그.
	// etcd 미연결 시 master 가 nil 이지만 endpoint 자체는 항상 노출 — UI 가
	// "0개" 라는 정확한 상태를 표시하도록 (404 가 아니라 빈 결과).
	mux.HandleFunc("GET /v1/currency", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if s.currencyMaster == nil {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "currencies": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"count":      s.currencyMaster.Size(),
			"currencies": s.currencyMaster.List(),
		})
	})
	mux.HandleFunc("GET /v1/currency/{code}", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if s.currencyMaster == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		c, ok := s.currencyMaster.Get(r.PathValue("code"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, c)
	})

	// Pair master 노출 — etcd 미연결 시도 endpoint 노출 (빈 결과).
	mux.HandleFunc("GET /v1/pair", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if s.pairMaster == nil {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "pairs": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"count": s.pairMaster.Size(),
			"pairs": s.pairMaster.List(),
		})
	})
	mux.HandleFunc("GET /v1/pair/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if s.pairMaster == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		p, ok := s.pairMaster.Get(r.PathValue("id"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, p)
	})

	// Cross-rate consumer stats — etcd 미연결 시도 endpoint 노출 (빈 stats).
	mux.HandleFunc("GET /v1/cross-stats", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.DevMode {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if s.crossConsumer == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, s.crossConsumer.Stats())
	})

	// Forward 시세 snapshot — pricingStore 가 주입돼 있고 best 가 활성일 때만 노출.
	if s.pricingStore != nil && s.best != nil {
		mux.HandleFunc("GET /v1/quote/forward-snapshot",
			ForwardSnapshotHandler(ForwardSnapshotDeps{
				Store: s.pricingStore, Best: s.best, Cross: s.crossConsumer, Swap: s.swapStore,
			}, s.cfg.DevMode))
		// S2. Spot-only lite endpoint — 매칭 엔진의 spot 거래 hot path 용.
		// forward tenor 루프 skip → latency 절감. bulk pair 지원.
		mux.HandleFunc("GET /v1/quote/spot",
			SpotSnapshotHandler(ForwardSnapshotDeps{
				Store: s.pricingStore, Best: s.best, Cross: s.crossConsumer,
			}, s.cfg.DevMode))
		s.logger.Info("Forward snapshot endpoint 활성 — GET /v1/quote/forward-snapshot, GET /v1/quote/spot")
	}
	// Forward quote lock — pricingStore + best + quoteID gen/reg 모두 있을 때.
	if s.pricingStore != nil && s.best != nil && s.quoteIDGen != nil && s.quoteIDReg != nil {
		mux.HandleFunc("POST /v1/quote/forward/lock",
			ForwardLockHandler(ForwardLockDeps{
				Store:    s.pricingStore,
				Best:     s.best,
				Gen:      s.quoteIDGen,
				Reg:      s.quoteIDReg,
				Validity: s.quoteIDValidity,
			}, s.cfg.DevMode))
		s.logger.Info("Forward quote-lock endpoint 활성 — POST /v1/quote/forward/lock",
			slog.Duration("validity", s.quoteIDValidity))
	}
	s.registerSwapLockRoutes(mux)
	s.registerSwapPointRoutes(mux)
	// 운영 진단 — gRPC stream 카탈로그 (누가 구독 중인가). grpcSrv 가 주입된
	// 경우에만 노출. 큐 깊이 = backpressure 가시화. operator 가 "지금 edge-A 가
	// VIP profile 받고 있나" 같은 질문에 즉시 답하기 위한 endpoint.
	if s.grpcSrv != nil {
		mux.HandleFunc("GET /v1/subscribers", func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.DevMode {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			writeJSON(w, http.StatusOK, s.grpcSrv.SubscribersSnapshot())
		})
		// CustomerRegistry digest — count + Profile 별 breakdown + 옵션 sample.
		// 수만 customer 일 수도 있어 default 는 by-profile 분포만, ?include_sample=true
		// 면 처음 limit (default 100) 명의 detail 도 포함.
		mux.HandleFunc("GET /v1/customers", func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.DevMode {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			reg := s.grpcSrv.CustomerRegistry()
			if reg == nil {
				writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
				return
			}
			snap := reg.Snapshot()
			byProfile := map[string]int{}
			for _, e := range snap {
				byProfile[e.Profile.Key()]++
			}
			resp := map[string]any{
				"count":      len(snap),
				"by_profile": byProfile,
			}
			if r.URL.Query().Get("include_sample") == "true" {
				limit := 100
				if v := r.URL.Query().Get("limit"); v != "" {
					if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
						limit = n
					}
				}
				if len(snap) > limit {
					snap = snap[:limit]
				}
				resp["sample"] = snap
				resp["sample_limit"] = limit
			}
			writeJSON(w, http.StatusOK, resp)
		})
		// 단일 customer 검색 — support 시나리오: customer_id 받으면 즉시 등록
		// 상태 + Profile 확인. 미등록이면 404.
		mux.HandleFunc("GET /v1/customers/{customerID}", func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.DevMode {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			reg := s.grpcSrv.CustomerRegistry()
			if reg == nil {
				writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
				return
			}
			id := r.PathValue("customerID")
			prof, ok := reg.Lookup(id)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error":       "not_registered",
					"customer_id": id,
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"customer_id": id,
				"profile":     prof,
				"profile_key": prof.Key(),
				"registered":  true,
			})
		})
		// N7. backpressure history — checkBackpressure 가 WARN 발생 시 ring buffer 에
		// 기록. 카운터만 보는 게 아니라 "최근 누가 어떤 큐로 정체 시점에 어디까지
		// 찼었는가" 까지 운영 alert 후 즉시 진단 가능.
		mux.HandleFunc("GET /v1/backpressure", func(w http.ResponseWriter, r *http.Request) {
			if s.cfg.DevMode {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			writeJSON(w, http.StatusOK, SnapshotBackpressureStats())
		})
		s.logger.Info("운영 진단 endpoint 활성 — GET /v1/subscribers, GET /v1/customers, GET /v1/customers/{customerID}, GET /v1/backpressure")
	}

	mux.Handle("GET /metrics", s.metrics.Handler())

	// DevMode 우회 tick 주입 — broker broadcast 를 못 받는 dev 환경에서
	// Aggregator/Consumer chain 단독 검증용. 운영에선 라우트 미등록.
	if s.cfg.DevMode {
		mux.HandleFunc("POST /v1/dev/tick", s.DevTickHandler())
		s.logger.Info("DevMode tick 주입 endpoint 활성 — POST /v1/dev/tick")

		// pprof — DevMode 한정 (부하 진단용). 운영 노출 금지.
		// /debug/pprof/{profile,heap,goroutine,mutex,block,...}
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		mux.Handle("GET /debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("GET /debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("GET /debug/pprof/mutex", pprof.Handler("mutex"))
		mux.Handle("GET /debug/pprof/block", pprof.Handler("block"))
		mux.Handle("GET /debug/pprof/allocs", pprof.Handler("allocs"))
		// mutex/block profiler 는 활성화하지 않으면 항상 0. 1=모든 contention 기록.
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		s.logger.Info("pprof 활성 (DevMode) — /debug/pprof/{profile,heap,mutex,block,goroutine}")
	}

	// QuoteID HTTP gateway — gRPC 와 동일한 핸들러, JSON wire.
	if s.quoteValidator != nil {
		RegisterQuoteValidationHTTP(mux, s.quoteValidator, s.logger)
		s.logger.Info("QuoteValidation HTTP gateway 등록",
			slog.String("base", "/v1/quoteid/"))
	}

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      metrics.HTTPMiddleware(s.metrics, "mci-price")(mux),
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	// HTTP TLS — cert/key 가 채워지면 https. Reloader 가 SIGHUP / 파일 watch
	// 로 핫리로드 — 운영 cert 회전 시 무중단.
	useTLS := s.cfg.HTTPTLSCertFile != "" && s.cfg.HTTPTLSKeyFile != ""
	if useTLS {
		rl, err := tlsutil.NewReloader(tlsutil.ReloaderOptions{
			CertFile:     s.cfg.HTTPTLSCertFile,
			KeyFile:      s.cfg.HTTPTLSKeyFile,
			ClientCAFile: s.cfg.HTTPTLSClientCAFile,
			Logger:       s.logger,
		})
		if err != nil {
			return fmt.Errorf("HTTP TLS reloader: %w", err)
		}
		s.http.TLSConfig = rl.ServerConfig()
		s.logger.Info("HTTP TLS 활성화",
			slog.String("addr", s.cfg.ListenAddr),
			slog.Bool("mtls", s.cfg.HTTPTLSClientCAFile != ""),
		)
	} else {
		s.logger.Info("HTTP listen 시작",
			slog.String("addr", s.cfg.ListenAddr),
			slog.String("broker", fmt.Sprintf("%s:%d", s.cfg.BrokerHost, s.cfg.BrokerPort)),
			slog.String("queue", s.cfg.QueueName),
			slog.String("exchange", s.cfg.ExchangeName),
		)
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if useTLS {
			// ListenAndServeTLS 는 cert/key 파일 인자가 빈 문자열이면 TLSConfig
			// 의 GetCertificate 를 사용 — Reloader 가 그 hook 점.
			err = s.http.ListenAndServeTLS("", "")
		} else {
			err = s.http.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown 은 HTTP + MyMQ 정리.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.mq != nil {
		if err := s.mq.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// loadBrokerTLS — Config 의 broker TLS 옵션이 있으면 *tls.Config, 아니면 nil.
func loadBrokerTLS(cfg *Config) (*tls.Config, error) {
	if cfg.BrokerTLSCertFile == "" && cfg.BrokerTLSCAFile == "" {
		return nil, nil
	}
	return tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     cfg.BrokerTLSCertFile,
		KeyFile:      cfg.BrokerTLSKeyFile,
		ServerCAFile: cfg.BrokerTLSCAFile,
		ServerName:   cfg.BrokerTLSSNI,
	})
}
