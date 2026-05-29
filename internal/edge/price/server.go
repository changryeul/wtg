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
	if s.cfg.EnableQuoteStream {
		go s.subscribeQuoteLoop(streamCtx)
	}

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

// subscribeQuoteLoop 는 SubscribeQuote stream 을 (재)시작 + 자동 재시도.
// EnableQuoteStream=true 인 경우에만 호출됨.
func (s *Server) subscribeQuoteLoop(ctx context.Context) {
	client := wtgpb.NewPriceServiceClient(s.upstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeQuoteOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("SubscribeQuote stream 끊김 — 재시도",
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

// consumeQuoteOnce 는 단일 SubscribeQuote stream 의 lifecycle.
func (s *Server) consumeQuoteOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	req := &wtgpb.QuoteSubscribeRequest{
		SubscriberId: s.cfg.SubscriberID,
		ProfileKeys:  s.cfg.QuoteProfileKeys,
	}
	stream, err := client.SubscribeQuote(ctx, req)
	if err != nil {
		return err
	}
	s.logger.Info("SubscribeQuote 시작",
		slog.String("subscriber_id", s.cfg.SubscriberID),
		slog.Any("profile_filter", s.cfg.QuoteProfileKeys),
	)

	for {
		cq, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream quote EOF")
		}
		if err != nil {
			return err
		}
		payload, err := encodeCustomerQuoteJSON(cq)
		if err != nil {
			s.logger.Warn("customerQuote JSON 직렬화 실패", slog.Any("error", err))
			continue
		}
		profKey := cq.GetChannel() + "." + cq.GetSite() + "." + cq.GetTier()
		s.registry.SendByProfile(profKey, cq.GetPair(), payload)
	}
}

// encodeCustomerQuoteJSON 은 proto CustomerQuote → 클라이언트 JSON.
// QuoteID / ValidUntilUnixNano 는 mci-price 가 quoteid 활성일 때만 채워져
// 오므로 omitempty 로 처리.
func encodeCustomerQuoteJSON(cq *wtgpb.CustomerQuote) ([]byte, error) {
	out := struct {
		Type               string  `json:"type"`
		Pair               string  `json:"pair"`
		Channel            string  `json:"chan"`
		Site               string  `json:"site"`
		Tier               string  `json:"tier"`
		Tenor              string  `json:"tenor"`
		Bid                float64 `json:"bid"`
		Ask                float64 `json:"ask"`
		TSUnixNano         int64   `json:"ts_unix_nano"`
		RawBid             float64 `json:"raw_bid,omitempty"`
		RawAsk             float64 `json:"raw_ask,omitempty"`
		TableVersion       int64   `json:"v"`
		QuoteID            string  `json:"quote_id,omitempty"`
		ValidUntilUnixNano int64   `json:"valid_until_unix_nano,omitempty"`
	}{
		Type:               "quote",
		Pair:               cq.GetPair(),
		Channel:            cq.GetChannel(),
		Site:               cq.GetSite(),
		Tier:               cq.GetTier(),
		Tenor:              cq.GetTenor(),
		Bid:                cq.GetBid(),
		Ask:                cq.GetAsk(),
		TSUnixNano:         cq.GetTsUnixNano(),
		RawBid:             cq.GetRawBid(),
		RawAsk:             cq.GetRawAsk(),
		TableVersion:       cq.GetTableVersion(),
		QuoteID:            cq.GetQuoteId(),
		ValidUntilUnixNano: cq.GetValidUntilUnixNano(),
	}
	return json.Marshal(out)
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

// BuildHandler — 미들웨어 chain 까지 적용된 최종 http.Handler 반환. 테스트
// 와 startHTTP 가 동일 chain 공유.
func (s *Server) BuildHandler() http.Handler {
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
	return middleware.Chain(mux, mws...)
}

// startHTTP 는 ws + 모니터링 endpoint 를 가동한다.
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

// subscribeHandler 는 GET /v1/subscribe — ws upgrade + Registry 등록.
//
// Profile 결정 우선순위 (quote stream 활성 시):
//  1. Principal.ProfileKey() — JWT claim (Chan/Site/Tier) 또는 Session 에서 (운영 경로)
//  2. ?profile= 쿼리 파라미터 — dev 도구용 fallback
//  3. 빈값 — quote 미수신 (raw broadcast 만)
func (s *Server) subscribeHandler(upgrader *websocket.Upgrader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil || p.Usid == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "인증 필요")
			return
		}

		// 1순위: Principal 의 Profile (JWT claim / Session 출처) — 위변조 불가.
		// 2순위: ?profile= 쿼리 — DevMode 검증 도구용. 운영에서는 사용 X.
		profileKey := p.ProfileKey()
		if profileKey == "" {
			profileKey = r.URL.Query().Get("profile")
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			s.logger.Warn("ws upgrade 실패", slog.Any("error", err))
			return
		}
		sub := NewSubscriber(ws, SubscriberOptions{
			SendQueueSize: s.cfg.SendQueueSize,
			Logger:        s.logger,
			ProfileKey:    profileKey,
			OnClose: func(sb *Subscriber) {
				s.registry.Remove(sb)
			},
		})
		s.registry.Add(sub)

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

// readLoop 는 ws 클라이언트 측 메시지 소비.
//
// Phase 1: 시세는 여전히 단방향이지만 control message 양방향 추가:
//
//	{"type":"subscribe",   "pairs":["USD/KRW","EUR/USD"]}  → 필터 추가
//	{"type":"unsubscribe", "pairs":["EUR/USD"]}           → 필터 제거 (빈셋=all 복귀)
//
// 서버는 처리 후 현재 셋 상태를 echo:
//
//	{"type":"subscribed",  "pairs":["USD/KRW", ...]}      // 필터 활성
//	{"type":"subscribed",  "pairs":null}                  // all 모드 (필터 없음)
//
// invalid JSON / 알 수 없는 type 은 error frame:
//
//	{"type":"error", "code":"bad_request", "message":"..."}
//
// ws 연결은 끊지 않음 — 잘못된 메시지 1건이 세션 전체를 끊지 않게.
func (s *Server) readLoop(sub *Subscriber) {
	defer sub.Close()
	sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
	sub.conn.SetPongHandler(func(string) error {
		sub.conn.SetReadDeadline(time.Now().Add(s.cfg.WsPongTimeout))
		return nil
	})
	for {
		_, data, err := sub.conn.ReadMessage()
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		s.handleControlMessage(sub, data)
	}
}

// controlRequest — ws 클라이언트가 보내는 control message 모양.
type controlRequest struct {
	Type  string   `json:"type"`
	Pairs []string `json:"pairs"`
}

// handleControlMessage — 단일 control message 파싱 + 처리 + echo.
func (s *Server) handleControlMessage(sub *Subscriber, data []byte) {
	var req controlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		s.sendControlError(sub, "bad_request", "JSON parse 실패: "+err.Error())
		return
	}
	switch req.Type {
	case "subscribe":
		sub.SubscribePairs(req.Pairs)
	case "unsubscribe":
		sub.UnsubscribePairs(req.Pairs)
	default:
		s.sendControlError(sub, "unknown_type", "지원하지 않는 type: "+req.Type)
		return
	}
	s.sendControlEcho(sub)
}

// sendControlEcho — 현재 필터 셋 상태 echo. Pairs=nil 이면 all 모드.
func (s *Server) sendControlEcho(sub *Subscriber) {
	pairs := sub.SubscribedPairs()
	payload, err := json.Marshal(map[string]any{
		"type":  "subscribed",
		"pairs": pairs, // nil → JSON null (의도)
	})
	if err != nil {
		return
	}
	_ = sub.Send(payload)
}

func (s *Server) sendControlError(sub *Subscriber, code, msg string) {
	payload, err := json.Marshal(map[string]any{
		"type":    "error",
		"code":    code,
		"message": msg,
	})
	if err != nil {
		return
	}
	_ = sub.Send(payload)
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
