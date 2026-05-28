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
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
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

	totalRecv  atomic.Uint64
	totalMatch atomic.Uint64 // exchange 필터 통과 건수
	totalDrop  atomic.Uint64 // 디코딩 실패 등

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
func (s *Server) AddConsumer(c TickConsumer) {
	if s.best != nil {
		s.best.AddDownstream(c)
		return
	}
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
}

// AttachQuoteValidator — QuoteValidationServer 를 HTTP gateway 로도 노출.
// nil 이면 무시. Start 전에 호출.
func (s *Server) AttachQuoteValidator(v *QuoteValidationServer) {
	s.quoteValidator = v
}

// Metrics — Prometheus Registry 노출. main.go 가 validator 에 주입 시 사용.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

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
	})
	if err != nil {
		return fmt.Errorf("mymq.Open: %w", err)
	}
	s.mq = mq

	// 구독 goroutine.
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	go s.subscribeLoop(subCtx, mq)

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
		}

		var grpcOpts []grpc.ServerOption
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

	// Tick.Source 채움 — v1 envelope 의 "src" 필드. BestConsumer 의 (Symbol,
	// Source) 캐시 키. raw body 가 v1 envelope 이 아니면 빈값 (BestConsumer 는
	// 빈 Source 를 drop, 비활성 시는 무관).
	tick.Source = peekSource(tick.Body)

	// Conflation 업데이트.
	s.conflation.Update(tick)

	// 다운스트림 consumer 호출.
	for _, c := range s.consumers {
		c.OnTick(tick)
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
	Received uint64          `json:"received"`
	Matched  uint64          `json:"matched"`
	Dropped  uint64          `json:"dropped"`
	Conf     ConflationStats `json:"conflation"`
}

// Stats 는 외부 노출용 카운터.
func (s *Server) Stats() Stats {
	return Stats{
		Received: s.totalRecv.Load(),
		Matched:  s.totalMatch.Load(),
		Dropped:  s.totalDrop.Load(),
		Conf:     s.conflation.Stats(),
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
			writeJSON(w, http.StatusOK, s.best.Stats())
		})
	}
	mux.Handle("GET /metrics", s.metrics.Handler())

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
