package chart

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"crypto/rsa"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// Server 는 mci-edge-chart 의 reverse proxy + 보안 layer.
//
// 흐름:
//
//	브라우저 → [Recover → AccessLog → RequestID → IPAllowList → RateLimit
//	            → (인증 — /v1/* 만)] → ReverseProxy → mci-chart
//
// /healthz 와 / (UI SPA) 는 인증 우회. /v1/chart, /v1/chart/stream 만 보호.
// WebSocket 은 httputil.ReverseProxy 가 자동으로 Upgrade 처리.
type Server struct {
	cfg    Config
	logger *slog.Logger

	metrics     *metrics.Registry
	ipLimiter   *ratelimit.Limiter
	jwtVer      *auth.Verifier
	tlsReloader *tlsutil.Reloader

	totalRequests atomic.Uint64
	totalProxied  atomic.Uint64
	totalErrors   atomic.Uint64

	http *http.Server
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

// SetJWTVerifier — 외부에서 JWT 검증기 주입 (테스트 / 운영 wiring).
func (s *Server) SetJWTVerifier(v *auth.Verifier) { s.jwtVer = v }

// BuildHandler — 미들웨어 chain 까지 적용된 최종 http.Handler 를 반환한다.
// 테스트 (httptest.NewServer) 와 Start 가 같은 chain 을 공유하도록 분리.
//
// 호출자 책임: Start 이전 시점에 호출. JWT 검증기 자동 로드는 Start 에서 처리.
func (s *Server) BuildHandler() (http.Handler, error) {
	upstream, err := url.Parse(s.cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("UpstreamURL 파싱: %w", err)
	}
	proxy := s.buildProxy(upstream)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthzHandler())
	mux.Handle("/v1/", s.requireAuth(proxy))
	mux.Handle("/", proxy)

	mws := []middleware.Middleware{
		middleware.BearerFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-edge-chart"),
		middleware.AccessLog(s.logger),
		middleware.RequestID(),
		middleware.Recover(s.logger),
	}
	if s.ipLimiter != nil {
		mws = append(mws, ratelimit.Middleware(s.ipLimiter, ratelimit.IPKey))
	}
	if len(s.cfg.AllowCIDRs) > 0 {
		mws = append(mws, netutil.IPAllowList(s.cfg.AllowCIDRs, s.logger))
	}
	return middleware.Chain(mux, mws...), nil
}

// Start 는 listen + serve (블로킹).
func (s *Server) Start(ctx context.Context) error {
	// JWT 검증기 — cfg 의 pub key 파일 로드.
	if s.cfg.JWTPubFile != "" && s.jwtVer == nil {
		v, err := loadVerifierFromFile(s.cfg.JWTPubFile)
		if err != nil {
			return fmt.Errorf("JWT 검증기 로드: %w", err)
		}
		s.jwtVer = v
	}

	chain, err := s.BuildHandler()
	if err != nil {
		return err
	}

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

	s.logger.Info("mci-edge-chart listen 시작",
		slog.String("addr", s.cfg.ListenAddr),
		slog.String("upstream", s.cfg.UpstreamURL),
		slog.Bool("dev", s.cfg.DevMode),
		slog.Bool("tls", tlsEnabled),
		slog.Bool("mtls", tlsEnabled && s.cfg.TLSClientCAFile != ""),
		slog.Bool("jwt", s.jwtVer != nil),
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

// Shutdown 은 그레이스풀 종료.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	if s.ipLimiter != nil {
		s.ipLimiter.Stop()
	}
	if s.tlsReloader != nil {
		s.tlsReloader.Stop()
	}
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			first = err
		}
	}
	return first
}

// requireAuth 는 JWTVerifier 또는 DevMode 가 활성일 때만 인증 통과 요구.
// 비활성 시 (둘 다 nil/false) 그대로 통과 — 사내망 dev 환경 호환.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	if !s.cfg.DevMode && s.jwtVer == nil {
		// 인증 미설정 — 그대로 통과 (사내망 가정).
		return next
	}
	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:     s.cfg.DevMode,
		JWTVerifier: s.jwtVer,
		Logger:      s.logger,
	})
	// query 의 access_token 도 처리 (브라우저 ws 헤더 미지원 대응).
	return middleware.BearerFromQuery()(authMW(next))
}

// healthzHandler — edge 자체 헬스 (upstream 호출 안 함).
func (s *Server) healthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-edge-chart",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// buildProxy — Internal mci-chart 로 forward 하는 ReverseProxy.
// Upgrade 헤더 (WebSocket) 는 표준 라이브러리가 자동 tunneling.
func (s *Server) buildProxy(upstream *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		// 외부에서 들어온 X-WTG-* 헤더 위변조 차단 — 무조건 strip.
		stripIngressHeaders(r.Header)
		// 검증된 Principal 만 새로 주입.
		if p := middleware.PrincipalFromContext(r.Context()); p != nil {
			r.Header.Set(middleware.HeaderEdgeUser, p.Usid)
			r.Header.Set(middleware.HeaderEdgeChannel, p.Channel)
			if p.SessionID != "" {
				r.Header.Set(middleware.HeaderEdgeSID, p.SessionID)
			}
			if p.Site != "" {
				r.Header.Set(middleware.HeaderEdgeSite, p.Site)
			}
			if p.Tier != "" {
				r.Header.Set(middleware.HeaderEdgeTier, p.Tier)
			}
		}
		if rid := middleware.RequestIDFromContext(r.Context()); rid != "" {
			r.Header.Set("X-Request-ID", rid)
		}
		r.Header.Set("X-Forwarded-Host", r.Host)
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.totalErrors.Add(1)
		s.logger.WarnContext(r.Context(), "upstream 호출 실패",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeJSONError(w, http.StatusBadGateway, "upstream_unavailable", err.Error())
	}

	baseRT := http.DefaultTransport
	if s.cfg.TLSUpstreamCertFile != "" || s.cfg.TLSUpstreamCAFile != "" {
		tlsCfg, err := tlsutil.LoadClient(tlsutil.ClientOptions{
			CertFile:     s.cfg.TLSUpstreamCertFile,
			KeyFile:      s.cfg.TLSUpstreamKeyFile,
			ServerCAFile: s.cfg.TLSUpstreamCAFile,
			ServerName:   s.cfg.TLSUpstreamServerName,
		})
		if err != nil {
			s.logger.Error("upstream TLS 구성 실패", slog.Any("error", err))
		} else {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = tlsCfg
			baseRT = tr
		}
	}
	proxy.Transport = baseRT

	// 응답 헤더 sanitization — Internal 의 디버그 헤더 외부 노출 차단.
	proxy.ModifyResponse = func(resp *http.Response) error {
		stripEgressHeaders(resp.Header)
		s.totalProxied.Add(1)
		return nil
	}
	return proxy
}

// 외부에서 받은 신뢰 헤더 strip — 외부 클라이언트의 spoof 차단.
func stripIngressHeaders(h http.Header) {
	h.Del(middleware.HeaderEdgeSID)
	h.Del(middleware.HeaderEdgeUser)
	h.Del(middleware.HeaderEdgeChannel)
	h.Del(middleware.HeaderEdgeSite)
	h.Del(middleware.HeaderEdgeTier)
}

// 외부로 응답 시 빼야 할 헤더 (Internal 디버그용 헤더 등).
func stripEgressHeaders(h http.Header) {
	// 운영 시 Internal-only 헤더가 추가되면 여기에서 제거.
	h.Del("X-WTG-Debug")
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

// loadVerifierFromFile — PEM 파일에서 RSA public key 읽어 Verifier 생성.
func loadVerifierFromFile(path string) (*auth.Verifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("PEM 형식 아님")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		// PKCS1 시도.
		rsaPub, err2 := x509.ParsePKCS1PublicKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("public key 파싱: %w / %w", err, err2)
		}
		pub = rsaPub
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key 가 RSA 가 아님")
	}
	return auth.NewVerifier(auth.VerifierOptions{
		Keys: auth.SingleKey{Key: rsaPub},
	})
}

// 사용하지 않는 import warning 방지 (strings 가 import 되어 있지만 직접 사용은 안 함).
var _ = strings.HasPrefix
