package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Server 는 mci-edge-api 의 HTTP 프록시 서버.
//
// httputil.ReverseProxy 기반으로 외부 요청을 Internal mci-api 로 forward.
// 인증/sanitization/audit/rate-limit/metrics 만 책임지고, 응답 본문은 그대로
// 통과 (passthrough).
type Server struct {
	cfg         Config
	logger      *slog.Logger
	metrics     *metrics.Registry
	ipLimiter   *ratelimit.Limiter
	jwtVer      *auth.Verifier
	tlsReloader *tlsutil.Reloader
	http        *http.Server
}

// SetJWTVerifier 는 access JWT 검증기를 주입한다.
//
// auth.md §6 의 RS256 권장 — DMZ edge 는 public key 만 보유하면 되며 secret
// 키는 Internal 의 Issuer 만 갖는다. 호출하지 않으면 1차 호환 모드 (DevMode 또는
// raw session_id) 로 동작.
func (s *Server) SetJWTVerifier(v *auth.Verifier) {
	s.jwtVer = v
}

// NewServer 는 Server 를 구성한다.
func NewServer(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.NewRegistry(),
	}
	if cfg.IPRatePerSec > 0 {
		s.ipLimiter = ratelimit.NewLimiter(ratelimit.Config{
			RatePerSec:     cfg.IPRatePerSec,
			Burst:          cfg.IPBurst,
			IdleEviction:   5 * time.Minute,
			EvictionPeriod: 1 * time.Minute,
		})
	}
	return s
}

// BuildHandler 는 운영 미들웨어 체인이 적용된 http.Handler 를 반환한다.
// Start() 가 내부적으로 사용하며, 테스트도 직접 호출해서 httptest.Server 에
// 등록 가능하다.
func (s *Server) BuildHandler() (http.Handler, error) {
	upstream, err := url.Parse(s.cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("upstream URL 파싱: %w", err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, errors.New("upstream URL 은 scheme + host 필요 (예: http://host:8080)")
	}

	proxy := s.buildProxy(upstream)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.pingHandler())
	// Prometheus /metrics — 인증 우회 (사내 네트워크 제한 가정).
	mux.Handle("GET /metrics", s.metrics.Handler())
	// 그 외 모든 /v1/* 은 upstream 으로 프록시.
	mux.HandleFunc("/", s.proxyHandler(proxy))

	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:     s.cfg.DevMode,
		JWTVerifier: s.jwtVer,
		Logger:      s.logger,
	})
	metricsMW := metrics.HTTPMiddleware(s.metrics, "mci-edge-api")

	// IP rate limit (옵션).
	mws := []middleware.Middleware{
		authMW,
		metricsMW,
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	}
	if s.cfg.IPRatePerSec > 0 {
		mws = append(mws, ratelimit.Middleware(s.ipLimiter, ratelimit.IPKey))
	}
	// IP allowlist — Chain semantics 상 slice 마지막이 *가장 바깥*. ratelimit
	// 도 거치기 전 즉시 거부되어 token 소모를 막고, auth/proxy 자원도 절약.
	if len(s.cfg.AllowCIDRs) > 0 {
		mws = append(mws, netutil.IPAllowList(s.cfg.AllowCIDRs, s.logger))
	}

	chain := middleware.Chain(mux, mws...)
	return s.maxBytesWrapper(chain), nil
}

// Start 는 listen + 블로킹.
func (s *Server) Start(ctx context.Context) error {
	handler, err := s.BuildHandler()
	if err != nil {
		return err
	}

	s.http = &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      handler,
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

	s.logger.Info("HTTP listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("upstream", s.cfg.UpstreamURL),
		slog.Bool("dev_mode", s.cfg.DevMode),
		slog.Bool("tls", tlsEnabled),
		slog.Bool("mtls", tlsEnabled && s.cfg.TLSClientCAFile != ""),
		slog.Bool("upstream_mtls", s.cfg.TLSUpstreamCertFile != ""),
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

// buildProxy 는 ReverseProxy 인스턴스를 구성한다.
func (s *Server) buildProxy(upstream *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	// Director: 외부에서 받은 요청을 upstream 용으로 변형.
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		// 외부에서 들어온 X-WTG-* 헤더는 위변조 방지를 위해 무조건 먼저 제거.
		// 그 후 edge 가 검증한 Principal 만 새로 주입한다.
		stripIngressHeaders(r.Header)
		if p := middleware.PrincipalFromContext(r.Context()); p != nil {
			r.Header.Set(middleware.HeaderEdgeUser, p.Usid)
			r.Header.Set(middleware.HeaderEdgeChannel, p.Channel)
			if p.SessionID != "" {
				r.Header.Set(middleware.HeaderEdgeSID, p.SessionID)
			}
		}
		// request id 전파.
		if rid := middleware.RequestIDFromContext(r.Context()); rid != "" {
			r.Header.Set("X-Request-ID", rid)
		}
		// upstream 측이 알아야 하는 원본 정보.
		r.Header.Set("X-Forwarded-Host", r.Host)
	}

	// 응답 보정: 외부에 노출하면 안 되는 헤더 제거.
	proxy.ModifyResponse = func(resp *http.Response) error {
		stripEgressHeaders(resp.Header)
		return nil
	}

	// 에러 핸들러 — upstream 끊김 / timeout 시 502.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.logger.WarnContext(r.Context(), "upstream 호출 실패",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
		)
		writeJSONError(w, http.StatusBadGateway, "upstream_unavailable", err.Error())
	}

	// 기본 transport — upstream mTLS 가 구성되었으면 적용.
	baseRT := http.DefaultTransport
	if s.cfg.TLSUpstreamCertFile != "" || s.cfg.TLSUpstreamCAFile != "" {
		tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
			CertFile:     s.cfg.TLSUpstreamCertFile,
			KeyFile:      s.cfg.TLSUpstreamKeyFile,
			ServerCAFile: s.cfg.TLSUpstreamCAFile,
			ServerName:   s.cfg.TLSUpstreamServerName,
		})
		if err != nil {
			s.logger.Error("upstream TLS 구성 실패 — DefaultTransport 로 fallback",
				slog.Any("error", err))
		} else {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = tlsCfg
			baseRT = tr
		}
	}

	// 전체 round-trip timeout (옵션).
	if s.cfg.UpstreamTimeout > 0 {
		proxy.Transport = &timeoutTransport{
			rt:      baseRT,
			timeout: s.cfg.UpstreamTimeout,
		}
	} else {
		proxy.Transport = baseRT
	}
	return proxy
}

// proxyHandler 는 인증 통과한 요청을 ReverseProxy 로 위임.
func (s *Server) proxyHandler(proxy *httputil.ReverseProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 인증된 Principal 이 없으면 forward 하지 않음 (Auth 미들웨어 통과 안된 경우).
		// /v1/ping 은 별도 핸들러로 분리되어 여기 도달 안 함.
		if p := middleware.PrincipalFromContext(r.Context()); p == nil || p.Usid == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "인증 필요")
			return
		}
		proxy.ServeHTTP(w, r)
	}
}

// pingHandler — DMZ 자체 헬스체크 (upstream 호출 안 함).
func (s *Server) pingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-edge-api",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// maxBytesWrapper 는 요청 본문 크기 제한 미들웨어.
func (s *Server) maxBytesWrapper(h http.Handler) http.Handler {
	if s.cfg.MaxRequestBody <= 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBody)
		h.ServeHTTP(w, r)
	})
}

// Shutdown 은 그레이스풀 종료.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.ipLimiter != nil {
		s.ipLimiter.Stop()
	}
	if s.tlsReloader != nil {
		s.tlsReloader.Stop()
	}
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// timeoutTransport 는 round-trip 타임아웃을 적용한다.
type timeoutTransport struct {
	rt      http.RoundTripper
	timeout time.Duration
}

func (t *timeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	r2 := r.Clone(ctx)
	resp, err := t.rt.RoundTrip(r2)
	if err != nil {
		cancel()
		return nil, err
	}
	// cancel 은 응답 본문이 다 소비된 후 호출되어야 하므로, body wrap.
	resp.Body = &cancelOnClose{rc: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	rc interface {
		Read([]byte) (int, error)
		Close() error
	}
	cancel context.CancelFunc
}

func (c *cancelOnClose) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *cancelOnClose) Close() error {
	err := c.rc.Close()
	c.cancel()
	return err
}

// stripIngressHeaders 는 외부에서 들어온 sensitive 헤더를 제거한다.
//
// 핵심: X-WTG-* 헤더를 외부에서 받지 않도록 차단 — mci-api 의
// TrustEdgeHeaders 모드에서 이 헤더로 인증을 결정하므로, 클라이언트가 직접
// 보내면 인증 우회 가능.
func stripIngressHeaders(h http.Header) {
	for _, name := range []string{
		"X-Forwarded-For", // edge 가 다시 채움
		"X-Real-IP",
		"X-Forwarded-Proto",
		// 인증 관련은 edge 가 검증 후 헤더로 변환하므로 원본 제거.
		"Authorization",
		"Cookie",
		// 신뢰 헤더 — 외부에서는 절대 받으면 안 됨.
		middleware.HeaderEdgeUser,
		middleware.HeaderEdgeChannel,
		middleware.HeaderEdgeSID,
	} {
		h.Del(name)
	}
}

// stripEgressHeaders 는 응답에서 외부 노출 부적절한 헤더 제거.
func stripEgressHeaders(h http.Header) {
	for _, name := range []string{
		"Server",       // 버전 노출 차단
		"X-Powered-By", // 프레임워크 정보
	} {
		h.Del(name)
	}
}

// 내부 응답 헬퍼.
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

// 사용 안 하는 import 제거용 (strings 는 일부 미들웨어 코드 패턴에 활용 가능).
var _ = strings.Repeat
