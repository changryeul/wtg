package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// Server 는 mci-edge-push 의 핵심.
//
// 흐름:
//
//	gRPC.Subscribe stream (Internal mci-push)
//	  → PushMessage 수신 → JSON envelope → Registry.FanoutToUser(logon_id)
//
//	Web client → GET /v1/subscribe → ws upgrade → Registry.Add(usid)
//	  → ws read/write goroutine
type Server struct {
	cfg    Config
	logger *slog.Logger

	registry         *Registry
	upstream         *grpc.ClientConn
	metrics          *metrics.Registry
	rateLimit        *ratelimit.RuleSet
	rateLimitWatcher *ratelimit.EtcdWatcher
	rateLimitEtcdCli *clientv3.Client
	rateLimitRedis   *redis.Client
	jwtVer           *auth.Verifier
	tlsReloader      *tlsutil.Reloader

	totalRecv      atomic.Uint64
	totalDelivered atomic.Uint64
	totalDropped   atomic.Uint64

	http *http.Server
}

// SetJWTVerifier — DMZ 가 보유하는 public key 기반 access JWT 검증기.
// 호출하지 않으면 DevMode (X-WTG-User 헤더) 만 동작.
func (s *Server) SetJWTVerifier(v *auth.Verifier) { s.jwtVer = v }

// NewServer.
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		registry: NewRegistry(logger),
		metrics:  metrics.NewRegistry(),
	}
	rules := cfg.RateLimitRules
	if rules == nil {
		rules = DefaultRateLimitRules()
	}
	if cfg.IPRatePerSec > 0 || len(rules) > 0 {
		var fallback *ratelimit.Config
		if cfg.IPRatePerSec > 0 {
			fallback = &ratelimit.Config{
				RatePerSec:     cfg.IPRatePerSec,
				Burst:          cfg.IPBurst,
				IdleEviction:   5 * time.Minute,
				EvictionPeriod: 1 * time.Minute,
			}
		}
		var ruleFactory ratelimit.LimiterFactory
		var fallbackFactory func(*ratelimit.Config) ratelimit.AllowLimiter
		if cfg.RateLimitRedisAddr != "" {
			s.rateLimitRedis = redis.NewClient(&redis.Options{
				Addr: cfg.RateLimitRedisAddr, Password: cfg.RateLimitRedisPassword, DB: cfg.RateLimitRedisDB,
			})
			onFail := func() { s.metrics.IncRateLimitRedisFail("mci-edge-push") }
			ruleFactory, fallbackFactory = ratelimit.MakeRedisFactoriesWithOnFail(s.rateLimitRedis, "edge-push", logger, onFail)
			logger.Info("rate limit Redis backend 활성", slog.String("addr", cfg.RateLimitRedisAddr))
		}
		rs, err := ratelimit.NewRuleSetWithFactory(rules, fallback, ruleFactory, fallbackFactory)
		if err != nil {
			logger.Error("rate limit 룰셋 빌드 실패", slog.Any("error", err))
		} else {
			s.rateLimit = rs
		}
	}
	return s
}

// Start.
func (s *Server) Start(ctx context.Context) error {
	creds, err := s.upstreamCreds()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(s.cfg.UpstreamGRPC, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("gRPC NewClient %s: %w", s.cfg.UpstreamGRPC, err)
	}
	s.upstream = conn

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	go s.subscribeLoop(streamCtx)

	if err := s.startRateLimitWatcher(ctx); err != nil {
		s.logger.Warn("ratelimit etcd watcher 시작 실패 — 정적 룰", slog.Any("error", err))
	}

	return s.startHTTP(ctx)
}

// upstreamCreds 는 Internal mci-push 호출용 gRPC TransportCredentials.
//
// 인증서 경로가 있으면 mTLS, 없으면 insecure (1차 호환). 운영 환경에서는
// 반드시 인증서 경로를 채워야 한다.
func (s *Server) upstreamCreds() (credentials.TransportCredentials, error) {
	if s.cfg.GRPCTLSCertFile == "" && s.cfg.GRPCTLSCAFile == "" {
		return insecure.NewCredentials(), nil
	}
	tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
		CertFile:     s.cfg.GRPCTLSCertFile,
		KeyFile:      s.cfg.GRPCTLSKeyFile,
		ServerCAFile: s.cfg.GRPCTLSCAFile,
		ServerName:   s.cfg.GRPCTLSServerName,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream TLS 구성: %w", err)
	}
	s.logger.Info("Upstream gRPC mTLS 활성화",
		slog.String("upstream", s.cfg.UpstreamGRPC),
		slog.String("sni", s.cfg.GRPCTLSServerName),
	)
	return credentials.NewTLS(tlsCfg), nil
}

// subscribeLoop 는 PushService.Subscribe 를 (재)시작.
func (s *Server) subscribeLoop(ctx context.Context) {
	client := wtgpb.NewPushServiceClient(s.upstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("PushService stream 끊김 — 재시도",
			slog.Any("error", err),
			slog.Duration("backoff", backoff),
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// consumeOnce 는 단일 stream lifecycle.
func (s *Server) consumeOnce(ctx context.Context, client wtgpb.PushServiceClient) error {
	req := &wtgpb.PushSubscribeRequest{
		SubscriberId: s.cfg.SubscriberID,
	}
	stream, err := client.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	s.logger.Info("PushService Subscribe 시작", slog.String("subscriber_id", s.cfg.SubscriberID))

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream EOF")
		}
		if err != nil {
			return err
		}
		s.totalRecv.Add(1)
		s.dispatch(msg)
	}
}

// dispatch 는 단일 PushMessage 를 logon_id 기반으로 fan-out.
func (s *Server) dispatch(msg *wtgpb.PushMessage) {
	payload, err := encodePushJSON(msg)
	if err != nil {
		s.logger.Warn("PushMessage 직렬화 실패", slog.Any("error", err))
		return
	}
	if msg.GetLogonId() != "" {
		sent, _ := s.registry.FanoutToUser(msg.GetLogonId(), payload)
		if sent > 0 {
			s.totalDelivered.Add(uint64(sent))
		} else {
			s.totalDropped.Add(1)
		}
		return
	}
	// logon_id 비어있음 → broadcast (시스템 알림 등).
	sent, _ := s.registry.FanoutBroadcast(payload)
	if sent > 0 {
		s.totalDelivered.Add(uint64(sent))
	} else {
		s.totalDropped.Add(1)
	}
}

// encodePushJSON 은 proto PushMessage → 클라이언트 JSON envelope.
func encodePushJSON(m *wtgpb.PushMessage) ([]byte, error) {
	type wsPush struct {
		Func             uint32          `json:"func"`
		Subc             uint32          `json:"subc"`
		Exchange         string          `json:"exchange,omitempty"`
		Channel          string          `json:"channel,omitempty"`
		LogonID          string          `json:"logon_id,omitempty"`
		Data             json.RawMessage `json:"data,omitempty"`
		ReceivedUnixNano int64           `json:"received_unix_nano,omitempty"`
	}
	out := wsPush{
		Func:             m.GetFunc(),
		Subc:             m.GetSubc(),
		Exchange:         m.GetExchange(),
		Channel:          m.GetChannel(),
		LogonID:          m.GetLogonId(),
		ReceivedUnixNano: m.GetReceivedUnixNano(),
	}
	body := m.GetData()
	if len(body) > 0 {
		if json.Valid(body) {
			out.Data = json.RawMessage(body)
		} else {
			b, err := json.Marshal(string(body))
			if err != nil {
				return nil, err
			}
			out.Data = b
		}
	}
	return json.Marshal(out)
}

// BuildHandler — 미들웨어 chain 까지 적용된 최종 http.Handler 반환. 테스트
// (httptest.NewServer) 와 startHTTP 가 동일 chain 을 공유하도록 분리.
func (s *Server) BuildHandler() http.Handler {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 운영: nil = gorilla default (same-origin). DevMode: cross-origin 허용 (admin UI wsmon 등).
	}
	if s.cfg.DevMode {
		upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-edge-push",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/edge-stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"received":  s.totalRecv.Load(),
			"delivered": s.totalDelivered.Load(),
			"dropped":   s.totalDropped.Load(),
			"users":     s.registry.UserCount(),
			"conns":     s.registry.Count(),
		})
	})
	mux.HandleFunc("GET /v1/subscribe", s.subscribeHandler(upgrader))
	mux.Handle("GET /metrics", s.metrics.Handler())

	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:     s.cfg.DevMode,
		JWTVerifier: s.jwtVer,
		Logger:      s.logger,
	})
	mws := []middleware.Middleware{
		authMW,
		// ws 클라이언트는 Authorization 헤더 보내기 어려우므로 query 의 access_token / x_wtg_user 를
		// 헤더로 변환. Auth 보다 안쪽 (실행 순서상 먼저) 에 위치.
		// 운영(JWT): BearerFromQuery. DevMode: UserFromQuery.
		middleware.BearerFromQuery(),
		middleware.UserFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-edge-push"),
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	}
	if s.rateLimit != nil {
		mws = append(mws, ratelimit.MiddlewareRules(
			s.rateLimit,
			ratelimit.UserOrIPKey(middleware.HeaderEdgeUser),
			ratelimit.MetricsHook{
				OnAllowed: func(rule, kind string) {
					s.metrics.IncRateLimit("mci-edge-push", kind, rule, true)
				},
				OnDenied: func(rule, kind string) {
					s.metrics.IncRateLimit("mci-edge-push", kind, rule, false)
				},
			},
		))
	}
	if len(s.cfg.AllowCIDRs) > 0 {
		mws = append(mws, netutil.IPAllowList(s.cfg.AllowCIDRs, s.logger))
	}
	return middleware.Chain(mux, mws...)
}

// startHTTP.
func (s *Server) startHTTP(ctx context.Context) error {
	chain := s.BuildHandler()

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
		rl.WatchSIGHUP()
		rl.WatchFile(30 * time.Second)
		s.tlsReloader = rl
		s.http.TLSConfig = rl.ServerConfig()
	}

	s.logger.Info("HTTP/WS listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("upstream", s.cfg.UpstreamGRPC),
		slog.Bool("dev_mode", s.cfg.DevMode),
		slog.Bool("tls", tlsEnabled),
		slog.Bool("mtls", tlsEnabled && s.cfg.TLSClientCAFile != ""),
		slog.Bool("upstream_mtls", s.cfg.GRPCTLSCertFile != ""),
	)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if tlsEnabled {
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

// subscribeHandler 는 ws upgrade + Registry 등록.
func (s *Server) subscribeHandler(upgrader *websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil || p.Usid == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "인증 필요")
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			s.logger.Warn("ws upgrade 실패", slog.Any("error", err))
			return
		}
		conn := NewConnection(ws, ConnectionOptions{
			LogonID:       p.Usid,
			Channel:       p.Channel,
			SendQueueSize: s.cfg.SendQueueSize,
			Logger:        s.logger,
			OnClose: func(c *Connection) {
				s.registry.Remove(c)
			},
		})
		s.registry.Add(conn)
		go s.writeLoop(conn)
		go s.readLoop(conn)
	}
}

// writeLoop.
func (s *Server) writeLoop(c *Connection) {
	ticker := time.NewTicker(s.cfg.WsPingInterval)
	defer ticker.Stop()
	defer c.Close()
	for {
		select {
		case <-c.closeC:
			return
		case payload, ok := <-c.send:
			if !ok {
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readLoop — 클라이언트 측 메시지 무시 (단방향 push).
func (s *Server) readLoop(c *Connection) {
	defer c.Close()
	c.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// Shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.rateLimitWatcher != nil {
		_ = s.rateLimitWatcher.Close()
	}
	if s.rateLimitEtcdCli != nil {
		_ = s.rateLimitEtcdCli.Close()
	}
	if s.rateLimit != nil {
		s.rateLimit.Stop()
	}
	if s.rateLimitRedis != nil {
		_ = s.rateLimitRedis.Close()
	}
	if s.tlsReloader != nil {
		s.tlsReloader.Stop()
	}
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.registry != nil {
		s.registry.CloseAll()
	}
	if s.upstream != nil {
		if err := s.upstream.Close(); err != nil && first == nil {
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

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": msg,
	})
}

// startRateLimitWatcher — EtcdEndpoints 비면 no-op. dial + EtcdWatcher 시작.
func (s *Server) startRateLimitWatcher(ctx context.Context) error {
	if s.cfg.EtcdEndpoints == "" || s.rateLimit == nil {
		return nil
	}
	eps := policy.SplitEndpoints(s.cfg.EtcdEndpoints)
	if len(eps) == 0 {
		return nil
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("etcd dial: %w", err)
	}
	defaults := s.cfg.RateLimitRules
	if defaults == nil {
		defaults = DefaultRateLimitRules()
	}
	var fb *ratelimit.FallbackCfg
	if s.cfg.IPRatePerSec > 0 {
		fb = &ratelimit.FallbackCfg{Rate: s.cfg.IPRatePerSec, Burst: s.cfg.IPBurst}
	}
	w, err := ratelimit.NewEtcdWatcher(ctx, ratelimit.EtcdWatcherOptions{
		Client:   cli,
		Key:      s.cfg.EtcdRateLimitKey,
		RuleSet:  s.rateLimit,
		Defaults: defaults,
		Fallback: fb,
		Logger:   s.logger,
	})
	if err != nil {
		_ = cli.Close()
		return fmt.Errorf("ratelimit watcher: %w", err)
	}
	s.rateLimitEtcdCli = cli
	s.rateLimitWatcher = w
	s.logger.Info("ratelimit etcd watcher 활성",
		slog.String("key", s.cfg.EtcdRateLimitKey),
		slog.Int("endpoints", len(eps)))
	return nil
}
