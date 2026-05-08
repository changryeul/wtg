package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/internal/api/handlers"
	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/routing"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Server 는 mci-api 의 HTTP 서버.
//
// 라이프사이클:
//
//  1. NewServer(cfg, logger) — Config + slog 으로 인스턴스 구성
//  2. s.Start(ctx)            — broker 연결 + HTTP listen, 블로킹
//  3. s.Shutdown(ctx)          — 그레이스풀 종료
type Server struct {
	cfg        Config
	logger     *slog.Logger
	mq         *mymq.Client
	metrics    *metrics.Registry
	sessions   auth.Store
	refresh    auth.RefreshStore
	jwtIss     *auth.Issuer
	jwtVer     *auth.Verifier
	routes      routing.Registry
	policy      *policy.Engine
	policySync  *policy.EtcdSync
	tlsReloader *tlsutil.Reloader
	http        *http.Server
}

// SetPolicyEngine — mci-admin 과 공유 시 외부 주입.
func (s *Server) SetPolicyEngine(e *policy.Engine) { s.policy = e }

// SetJWT 는 Issuer/Verifier 를 함께 주입한다 (운영 키 페어).
//
// 호출하지 않으면 1차 호환 모드 (raw session_id 만 발급) 로 동작.
// 인자가 nil 이면 무시 — 부분 주입 가능.
func (s *Server) SetJWT(iss *auth.Issuer, ver *auth.Verifier) {
	if iss != nil {
		s.jwtIss = iss
	}
	if ver != nil {
		s.jwtVer = ver
	}
}

// SetRefreshStore 는 refresh 토큰 저장소를 주입한다.
// 호출하지 않으면 Start 시점에 in-memory 가 자동 생성된다 (DevMode 외).
func (s *Server) SetRefreshStore(store auth.RefreshStore) {
	s.refresh = store
}

// SetRoutingRegistry 는 외부에서 라우팅 룰 저장소를 주입한다 (테스트 / etcd
// 차환 / mci-admin 과 공유하는 외부 backed 구현 등).
//
// 호출하지 않으면 Start 시점에 빈 InMemoryRegistry 가 자동 생성된다.
// alias 가 미등록이면 envelope.Alias 사용은 거부되지만, alias 없는 raw
// passthrough 는 Registry 와 무관하게 동작.
func (s *Server) SetRoutingRegistry(reg routing.Registry) {
	s.routes = reg
}

// NewServer 는 Server 를 구성한다 (broker 미접속 상태).
// 실제 broker 연결은 Start() 에서 수행.
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

// Start 는 broker 에 연결하고 HTTP 서버를 가동한다 (블로킹).
// ctx 가 취소되거나 listen 에러 발생 시 반환.
func (s *Server) Start(ctx context.Context) error {
	// broker TLS 옵션이 있으면 *tls.Config 구성 (docs/broker-tls.md 참조).
	brokerTLS, err := loadBrokerTLS(&s.cfg)
	if err != nil {
		return fmt.Errorf("broker TLS 구성: %w", err)
	}
	// MyMQ broker 연결 — request/reply 만 필요하므로 DECLARE_SESSION 사용.
	mq, err := mymq.Open(ctx, s.cfg.BrokerHost, s.cfg.BrokerPort, mymq.Options{
		ApplName:         s.cfg.ApplName,
		Instance:         s.cfg.Instance,
		Channel:          mymq.ChannelWeb,
		DialTimeout:      s.cfg.DialTimeout,
		HandshakeTimeout: s.cfg.HandshakeTimeout,
		Logger:           s.logger,
		TLS:              brokerTLS,
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

	// 세션 저장소 — DevMode 가 아니면 in-memory 기본 (Phase 3 Redis 전 단계).
	// auth.md §7 — 운영은 RedisStore 로 차환 예정. 인터페이스 동일.
	if !s.cfg.DevMode {
		s.sessions = auth.NewMemoryStore(auth.MemoryStoreOptions{})
		if s.refresh == nil {
			s.refresh = auth.NewMemoryRefreshStore(auth.MemoryRefreshStoreOptions{})
		}
	}
	// 라우팅 룰 저장소 — etcd endpoint 가 있으면 EtcdRegistry 로 mci-admin 과 공유,
	// 없으면 InMemoryRegistry (단일 인스턴스 / dev).
	if s.routes == nil {
		reg, err := routing.New(ctx, routing.FactoryOptions{
			Endpoints: s.cfg.EtcdEndpoints,
			Prefix:    s.cfg.EtcdRoutesPath,
			Logger:    s.logger,
		})
		if err != nil {
			return fmt.Errorf("routing registry: %w", err)
		}
		s.routes = reg
	}
	// DevMode 시드 — etcd 미사용 시 mci-admin 과 별개의 in-memory registry 라
	// 같은 alias 시드를 같이 적용해야 alias 호출이 동작한다. cfg 파일 우선,
	// 없으면 hardcode default. 양쪽 인스턴스가 같은 cfg 파일을 읽으면 일관됨.
	// 정책 (additive | sync) 은 routing.SeedPolicy 주석 참조.
	if s.cfg.DevMode {
		policy, _ := routing.ParseSeedPolicy(s.cfg.DevRoutesPolicy)
		routing.SeedDevRoutesExPolicy(s.routes, s.logger, s.cfg.DevRoutesFile, policy)
		// hot reload — cfg 파일 mtime polling. mci-admin / mci-api 양쪽 다 watch
		// 라 사용자가 cfg 한 번 수정하면 두 인스턴스 모두 재시드.
		routing.WatchRoutesFilePolicy(ctx, s.routes, s.logger, s.cfg.DevRoutesFile, 2*time.Second, policy)
	}
	if s.policy == nil {
		s.policy = policy.NewEngine(nil)
	}
	// etcd 동기화 — admin 측 변경이 watch 로 도착해서 즉시 반영.
	if eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints); len(eps) > 0 && s.policySync == nil {
		ps, err := policy.StartEtcdSync(ctx, s.policy, policy.EtcdSyncOptions{
			Endpoints: eps,
			Key:       s.cfg.EtcdPolicyKey,
			Logger:    s.logger,
		})
		if err != nil {
			return fmt.Errorf("policy etcd sync: %w", err)
		}
		s.policySync = ps
	}
	// DevMode HTTP poll sync — etcd 가 없을 때만 의미. mci-admin 이 진실의 원천.
	// split-brain (admin 토글이 api 까지 안 닿음) 방지용 dev-only 우회.
	if u := policy.SanitizePollURL(s.cfg.DevPolicyURL); u != "" && s.cfg.DevMode {
		if err := policy.StartHTTPPoll(ctx, s.policy, policy.HTTPPollOptions{
			URL:      u,
			Interval: 2 * time.Second,
			Headers:  map[string]string{"X-WTG-User": "dev-policy-poller"},
			Logger:   s.logger,
		}); err != nil {
			return fmt.Errorf("policy http poll: %w", err)
		}
	}

	// 핸들러 dependencies.
	deps := &handlers.Deps{
		MQ:           mq,
		CallTimeout:  s.cfg.BrokerCallTimeout,
		Logger:       s.logger,
		Sessions:     s.sessions,
		Routes:       s.routes,
		Policy:       s.policy,
		JWTIssuer:    s.jwtIss,
		RefreshStore: s.refresh,
	}

	// 라우팅 — Go 1.22+ ServeMux (method+path 패턴 지원).
	//
	// 매매 transaction 은 모두 POST /v1/tx 로 generic passthrough.
	// /v1/login, /v1/logout 은 web 세션 라이프사이클이 부수효과로 들어가
	// 별도 핸들러로 분리 (auth.md §3, §5).
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", handlers.Ping(deps))
	mux.HandleFunc("POST /v1/tx", handlers.Transaction(deps))
	mux.HandleFunc("POST /v1/login", handlers.Login(deps))
	mux.HandleFunc("POST /v1/logout", handlers.Logout(deps))
	mux.HandleFunc("POST /v1/refresh", handlers.Refresh(deps))
	mux.Handle("GET /metrics", s.metrics.Handler())

	// 미들웨어 체인 — 바깥 → 안쪽 순서:
	//   recover  : panic → 500
	//   reqid    : X-Request-ID 발급/주입
	//   access   : access log
	//   auth     : DevMode (헤더) 또는 SessionMode (Bearer) 인증
	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:          s.cfg.DevMode,
		SessionStore:     s.sessions,
		JWTVerifier:      s.jwtVer,
		TrustEdgeHeaders: s.cfg.TrustEdgeHeaders,
		Logger:           s.logger,
	})
	chain := middleware.Chain(
		mux,
		authMW,
		metrics.HTTPMiddleware(s.metrics, "mci-api"),
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	)

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      chain,
		ReadTimeout:  s.cfg.ReadTimeout,
		WriteTimeout: s.cfg.WriteTimeout,
		IdleTimeout:  s.cfg.IdleTimeout,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

	tlsEnabled := s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != ""
	if tlsEnabled {
		rl, err := tlsutil.NewReloader(tlsutil.ReloaderOptions{
			CertFile:     s.cfg.TLSCertFile,
			KeyFile:      s.cfg.TLSKeyFile,
			ClientCAFile: s.cfg.TLSClientCAFile,
			Logger:       s.logger,
		})
		if err != nil {
			return fmt.Errorf("TLS 구성: %w", err)
		}
		// 운영 중 cert 갱신: SIGHUP 또는 mtime polling 둘 다 활성.
		rl.WatchSIGHUP()
		rl.WatchFile(30 * time.Second)
		s.tlsReloader = rl
		s.http.TLSConfig = rl.ServerConfig()
	}

	s.logger.Info("HTTP listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", s.cfg.BrokerHost, s.cfg.BrokerPort)),
		slog.Bool("dev_mode", s.cfg.DevMode),
		slog.Bool("tls", tlsEnabled),
		slog.Bool("mtls", tlsEnabled && s.cfg.TLSClientCAFile != ""),
	)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if tlsEnabled {
			// 인증서/키는 TLSConfig 에서 로딩 — ListenAndServeTLS 인자는 빈 값 OK.
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
		// 외부 종료 신호 — 그레이스풀 셧다운.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown 은 HTTP 서버와 MyMQ connection 을 그레이스풀하게 종료한다.
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
	if s.sessions != nil {
		if err := s.sessions.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.refresh != nil {
		if err := s.refresh.Close(); err != nil && first == nil {
			first = err
		}
	}
	if closer, ok := s.routes.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.policySync != nil {
		if err := s.policySync.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.tlsReloader != nil {
		s.tlsReloader.Stop()
	}
	return first
}

// loadBrokerTLS — Config 의 broker TLS 옵션이 있으면 *tls.Config, 아니면 nil.
//
// 모든 4개 인증서 파일 경로 또는 CA 만 채워진 경우 모두 허용:
//   - cert+key+ca: mTLS 클라이언트
//   - ca 만:        서버 인증서만 검증 (클라이언트 cert 없음)
//   - 전부 빈 경우: nil → plain TCP
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
