package push

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Server 는 mci-push HTTP/WebSocket 서버 + MyMQ unsolicited 구독자.
type Server struct {
	cfg        Config
	logger     *slog.Logger
	mq         *mymq.Client
	registry   *Registry
	dispatcher *Dispatcher
	metrics    *metrics.Registry
	http       *http.Server
}

// NewServer 는 Server 를 구성한다 (broker 미접속 상태).
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.NewRegistry(),
	}
}

// Start 는 broker 연결 + Dispatcher 시작 + HTTP 서버 가동 (블로킹).
//
// NoBroker=true 면 broker 연결 skip — dispatcher 는 HTTP inject 만 소비.
// Phase 2.7 사전 옵션 / dev / test / 장애 대응.
func (s *Server) Start(ctx context.Context) error {
	var mq *mymq.Client
	if !s.cfg.NoBroker {
		brokerTLS, err := loadBrokerTLS(&s.cfg)
		if err != nil {
			return fmt.Errorf("broker TLS 구성: %w", err)
		}
		mq, err = mymq.Open(ctx, s.cfg.BrokerHost, s.cfg.BrokerPort, mymq.Options{
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
				// QfUnsolRep 가 핵심: broker 가 이 client 를 `대표 unsolicited 수신자`
				// (representative receiver) 로 등록해서 user 매칭 없이 모든 publish 를
				// 흘려준다. broker 의 publish.c 가 _REPRESENTATIVE_UNSOL_RECVER_ flag 를
				// 보고 user 매칭을 skip 하기 때문 (umap_chk 우회). mci-push 는 그 다음에
				// broadcast prefix.LogonID 로 ws Registry fan-out 한다.
				Flags: mymq.QfUnsolMsg | mymq.QfUnsolHdr | mymq.QfUnsolRep,
			},
			Reconnect: &mymq.ReconnectOptions{
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     30 * time.Second,
				BackoffFactor:  2.0,
			},
			Metrics: mymq.MetricsHook{
				OnDisconnect:       func(_ error) { s.metrics.IncBrokerDisconnect("mci-push") },
				OnReconnect:        func(_ int, d time.Duration) { s.metrics.IncBrokerReconnect("mci-push", d) },
				OnInflightAborted:  func(n int) { s.metrics.IncBrokerInflightAborted("mci-push", n) },
				OnHeartbeatTimeout: func() { s.metrics.IncBrokerHeartbeatTimeout("mci-push") },
			},
		})
		if err != nil {
			return fmt.Errorf("mymq.Open: %w", err)
		}
		s.mq = mq
	} else {
		s.logger.Warn("mci-push: broker 비활성 모드 (--no-broker) — HTTP push only")
	}

	s.registry = NewRegistry(s.logger)
	// Sub == nil 이면 Dispatcher.Run 이 자동으로 broker case 비활성 (nil-channel select).
	dispOpts := DispatcherOptions{
		Registry: s.registry,
		Logger:   s.logger,
	}
	if mq != nil {
		dispOpts.Sub = mq
	}
	s.dispatcher = NewDispatcher(dispOpts)

	dispCtx, dispCancel := context.WithCancel(ctx)
	defer dispCancel()

	// gRPC PushService — DMZ edge 구독자 fan-out.
	if s.cfg.GRPCAddr != "" {
		grpcSrv := NewGRPCServer(s.logger, s.cfg.GRPCBufSize)
		s.dispatcher.AddHook(grpcSrv)

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
			if err := grpcSrv.Serve(dispCtx, s.cfg.GRPCAddr, grpcOpts...); err != nil {
				s.logger.Error("PushService gRPC 서버 종료", slog.Any("error", err))
			}
		}()
	}

	// Dispatcher 가동.
	go s.dispatcher.Run(dispCtx)

	// HTTP 라우팅.
	deps := &HandlerDeps{
		Registry:      s.registry,
		Dispatcher:    s.dispatcher,
		Logger:        s.logger,
		SendQueueSize: s.cfg.SendQueueSize,
		PingInterval:  s.cfg.WsPingInterval,
		PongTimeout:   s.cfg.WsPongTimeout,
		StartedAt:     time.Now(),
	}
	// dispatcher 의 누적 카운터를 Prometheus 게이지로 노출 — push-stats 와 별도로
	// /metrics 에서 mci_push_dispatcher_* 라벨로 제공된다 (forwarder 와 같은 패턴).
	registerDispatcherMetrics(s.metrics, s.dispatcher)
	// CheckOrigin: 운영에서는 도메인 화이트리스트로 교체. DevMode 에서는 mci-admin UI
	// (다른 origin: 9090) 에서 8081 로 ws connect 하는 것을 허용해야 하므로 모두 통과.
	if s.cfg.DevMode {
		deps.CheckOrigin = func(r *http.Request) bool { return true }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", PingHandler())
	mux.HandleFunc("GET /v1/push-stats", StatsHandler(deps))
	mux.HandleFunc("GET /v1/subscribe", SubscribeHandler(deps))
	// Phase-1 PoC — broker 없이 unsolicited 주입 받는 internal endpoint.
	// Phase 2.4 — mTLS audit log + secret 이중 검증.
	// Phase 2.5 — CN 별 inject counter (mci_push_http_inject_total{cn, result}).
	// 운영 svc 가 점차 broker 의존 줄이도록. mTLS (HTTPTLSClientCAFile) 또는 PushSecret
	// (X-Push-Secret 헤더) 또는 둘 다 활성 — 둘 다 비면 wide-open (dev 전용, warn log).
	if s.cfg.PushSecret == "" && s.cfg.HTTPTLSClientCAFile == "" {
		s.logger.Warn("POST /v1/internal/push 인증 wide-open — dev 전용. 운영은 --push-secret 또는 --http-tls-client-ca 설정 권장")
	}
	httpInjectCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mci_push_http_inject_total",
			Help: "POST /v1/internal/push 처리 결과 (cn = mTLS client CN 또는 'anonymous', " +
				"result = ok|unauthorized|bad_json|inject_full). Phase 2.5",
		},
		[]string{"cn", "result"},
	)
	if err := s.metrics.Register(httpInjectCounter); err != nil {
		s.logger.Warn("mci_push_http_inject_total 등록 실패", slog.Any("error", err))
	}
	mux.HandleFunc("POST /v1/internal/push", HTTPPushHandlerDeps(HTTPPushDeps{
		Dispatcher: s.dispatcher,
		Secret:     s.cfg.PushSecret,
		Logger:     s.logger,
		OnInject: func(cn, result string) {
			httpInjectCounter.WithLabelValues(cn, result).Inc()
		},
	}))
	mux.Handle("GET /metrics", s.metrics.Handler())

	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode: s.cfg.DevMode,
		Logger:  s.logger,
	})
	chain := middleware.Chain(
		mux,
		authMW,
		// 브라우저 WebSocket 은 사용자 정의 헤더를 못 보내므로 query → 헤더 변환:
		//   access_token (운영 JWT) → Authorization,  x_wtg_user (DevMode) → X-WTG-User.
		middleware.BearerFromQuery(),
		middleware.UserFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-push"),
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	)

	// HTTP TLS (Phase 2.4) — HTTPTLSCertFile/KeyFile 있으면 HTTPS, ClientCAFile 도 있으면 mTLS.
	httpTLSCfg, err := loadHTTPTLS(&s.cfg)
	if err != nil {
		return fmt.Errorf("HTTP TLS 구성: %w", err)
	}

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      chain,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		TLSConfig:    httpTLSCfg,
	}

	httpsActive := httpTLSCfg != nil
	mtlsActive := httpsActive && s.cfg.HTTPTLSClientCAFile != ""
	s.logger.Info("HTTP/WS listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", s.cfg.BrokerHost, s.cfg.BrokerPort)),
		slog.String("queue", s.cfg.QueueName),
		slog.Bool("dev_mode", s.cfg.DevMode),
		slog.Bool("https", httpsActive),
		slog.Bool("mtls", mtlsActive),
	)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if httpsActive {
			// cert/key 는 TLSConfig 에 이미 로딩됨 — 빈 인자 OK.
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

// Shutdown 은 HTTP + 모든 WS connection + MyMQ connection 을 정리한다.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.registry != nil {
		s.registry.CloseAll()
	}
	if s.mq != nil {
		if err := s.mq.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
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

// loadHTTPTLS — Config 의 HTTP TLS 옵션이 있으면 *tls.Config (서버), 아니면 nil.
// CertFile/KeyFile 필수, ClientCAFile 추가 시 mTLS (client cert 검증).
// Phase 2.4 — POST /v1/internal/push 의 운영 svc 인증 (CN/SAN 기반).
func loadHTTPTLS(cfg *Config) (*tls.Config, error) {
	if cfg.HTTPTLSCertFile == "" && cfg.HTTPTLSKeyFile == "" {
		return nil, nil
	}
	return tlsutil.LoadServer(tlsutil.ServerOptions{
		CertFile:     cfg.HTTPTLSCertFile,
		KeyFile:      cfg.HTTPTLSKeyFile,
		ClientCAFile: cfg.HTTPTLSClientCAFile,
	})
}
