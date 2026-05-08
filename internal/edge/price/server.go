package price

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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
	"github.com/winwaysystems/wtg/pkg/tlsutil"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// Server 는 mci-edge-price 의 핵심.
//
// 흐름:
//
//	gRPC.Subscribe stream (Internal mci-price)
//	  → Tick 수신 → JSON 직렬화 → Registry.Broadcast (모든 ws 에 fan-out)
//
//	Web client → GET /v1/subscribe → ws upgrade → Registry.Add
//	  → ws read/write goroutine
type Server struct {
	cfg    Config
	logger *slog.Logger

	registry    *Registry
	upstream    *grpc.ClientConn
	metrics     *metrics.Registry
	ipLimiter   *ratelimit.Limiter
	jwtVer      *auth.Verifier
	tlsReloader *tlsutil.Reloader

	totalRecv atomic.Uint64

	http *http.Server
}

// SetJWTVerifier — DMZ public key 기반 access JWT 검증기.
// 호출하지 않으면 DevMode 만 동작.
func (s *Server) SetJWTVerifier(v *auth.Verifier) { s.jwtVer = v }

// NewServer 는 Server 를 구성한다 (gRPC 미연결 상태).
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

// Start 는 gRPC dial + Subscribe stream + HTTP 서버 가동 (블로킹).
func (s *Server) Start(ctx context.Context) error {
	// TLS 옵션이 있으면 mTLS, 없으면 insecure (1차 호환).
	// NewClient 는 lazy connect — 실제 연결은 첫 RPC 시점.
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

	return s.startHTTP(ctx)
}

// upstreamCreds 는 Internal mci-price 호출용 gRPC TransportCredentials.
//
// 인증서 경로가 있으면 mTLS, 없으면 insecure — 1차 호환 모드.
// 운영에서는 반드시 인증서 경로를 채워야 한다.
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

// subscribeLoop 는 gRPC Subscribe stream 을 (재)시작하며 끊기면 자동 재시도.
func (s *Server) subscribeLoop(ctx context.Context) {
	client := wtgpb.NewPriceServiceClient(s.upstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("Subscribe stream 끊김 — 재시도",
			slog.Any("error", err),
			slog.Duration("backoff", backoff),
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// exponential backoff up to 10s.
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// consumeOnce 는 단일 Subscribe stream 의 lifecycle.
func (s *Server) consumeOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	req := &wtgpb.SubscribeRequest{
		SubscriberId: s.cfg.SubscriberID,
	}
	stream, err := client.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	s.logger.Info("PriceService Subscribe 시작", slog.String("subscriber_id", s.cfg.SubscriberID))

	for {
		tick, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream EOF")
		}
		if err != nil {
			return err
		}
		s.totalRecv.Add(1)
		payload, err := encodeTickJSON(tick)
		if err != nil {
			s.logger.Warn("tick JSON 직렬화 실패", slog.Any("error", err))
			continue
		}
		s.registry.Broadcast(payload)
	}
}

// encodeTickJSON 은 proto Tick → 클라이언트 JSON envelope.
//
// data(body) 는 cooker 페이로드 raw bytes 그대로. JSON 이면 RawMessage,
// 아니면 string-wrap.
func encodeTickJSON(t *wtgpb.Tick) ([]byte, error) {
	type wsTick struct {
		MarketID         uint64          `json:"market_id"`
		Symbol           string          `json:"symbol"`
		SeqNum           uint32          `json:"seq_num"`
		Mask             uint32          `json:"mask,omitempty"`
		Type             uint32          `json:"type,omitempty"`
		Flag             uint32          `json:"flag,omitempty"`
		Data             json.RawMessage `json:"data,omitempty"`
		ReceivedUnixNano int64           `json:"received_unix_nano,omitempty"`
	}
	out := wsTick{
		MarketID:         t.GetMarketId(),
		Symbol:           t.GetSymbol(),
		SeqNum:           t.GetSeqNum(),
		Mask:             t.GetMask(),
		Type:             t.GetType(),
		Flag:             t.GetFlag(),
		ReceivedUnixNano: t.GetReceivedUnixNano(),
	}
	body := t.GetBody()
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

// startHTTP 는 ws + 모니터링 endpoint 를 가동한다.
func (s *Server) startHTTP(ctx context.Context) error {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     nil, // 운영 환경에서 화이트리스트 함수 주입.
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-edge-price",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/edge-stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"received": s.totalRecv.Load(),
			"registry": s.registry.Stats(),
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
		// ws 클라이언트 호환 — query 의 access_token 을 헤더로 변환.
		middleware.BearerFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-edge-price"),
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
	chain := middleware.Chain(mux, mws...)

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

// subscribeHandler 는 GET /v1/subscribe — ws upgrade + Registry 등록.
func (s *Server) subscribeHandler(upgrader *websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 인증된 사용자만 (Auth 미들웨어 통과 가정 — 시세는 전체 broadcast 라
		// 사용자 식별이 필수는 아니지만 운영 정책상 인증은 강제).
		if p := middleware.PrincipalFromContext(r.Context()); p == nil || p.Usid == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "인증 필요")
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			s.logger.Warn("ws upgrade 실패", slog.Any("error", err))
			return
		}
		sub := NewSubscriber(ws, SubscriberOptions{
			SendQueueSize: s.cfg.SendQueueSize,
			Logger:        s.logger,
			OnClose: func(sb *Subscriber) {
				s.registry.Remove(sb)
			},
		})
		s.registry.Add(sub)

		// write/read goroutine 시작.
		go s.writeLoop(sub)
		go s.readLoop(sub)
	}
}

// writeLoop 은 send queue → ws.WriteMessage 직렬 송신 + 주기 ping.
func (s *Server) writeLoop(sub *Subscriber) {
	ticker := time.NewTicker(s.cfg.WsPingInterval)
	defer ticker.Stop()
	defer sub.Close()
	for {
		select {
		case <-sub.closeC:
			return
		case payload, ok := <-sub.send:
			if !ok {
				return
			}
			sub.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := sub.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			sub.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := sub.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readLoop 는 ws 클라이언트 측 메시지 소비 (시세는 단방향이라 ping/close 만).
func (s *Server) readLoop(sub *Subscriber) {
	defer sub.Close()
	sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
	sub.conn.SetPongHandler(func(string) error {
		sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
		return nil
	})
	for {
		if _, _, err := sub.conn.ReadMessage(); err != nil {
			return
		}
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
