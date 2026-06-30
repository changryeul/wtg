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
	"net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
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

	registry         *Registry
	upstream         *grpc.ClientConn
	metrics          *metrics.Registry
	rateLimit        *ratelimit.RuleSet
	rateLimitWatcher *ratelimit.EtcdWatcher
	rateLimitEtcdCli *clientv3.Client
	rateLimitRedis   *redis.Client
	jwtVer           *auth.Verifier
	tlsReloader      *tlsutil.Reloader

	// pairValidator — Phase 2. nil 이면 모든 pair 허용 (backward compat).
	// MemoryPairValidator 가 기본 구현. operator seed (config) + passive
	// learning (consumeQuoteOnce 가 도착 quote.Pair 추가) 결합.
	pairValidator PairValidator

	// customerPairs — 고객별 ws 구독 허용 pair allowlist. nil 이면 글로벌
	// 정책만 (backward compat). etcd watch 로 mci-admin 의 변경 즉시 반영.
	customerPairs CustomerPairPolicy

	// customerPairsWatcher — etcd lifecycle 보유. Stop 시 cancel.
	customerPairsWatcher *EtcdCustomerPairWatcher
	customerPairsEtcdCli *clientv3.Client

	// Phase 4c — customer-specific quote 경로. cfg.EnableCustomerStream 활성 시
	// Start 에서 가동. ws upgrade 시 customerRegMgr.Register 호출,
	// subscribeCustomerQuoteLoop 가 customer-tagged quote 를 Registry 로 전달.
	customerRegMgr *customerRegManager

	// Phase 4 liveness — pair 별 last_ts 추적 + stale 알림. nil 이면 비활성.
	liveness *pairLiveness

	// 부정 카운터 — admin / metric 노출용.
	totalRejectedPairs atomic.Uint64
	totalStaleSent     atomic.Uint64 // stale 알림 누적 발송 수
	totalFreshSent     atomic.Uint64 // 회복 알림 누적

	totalRecv atomic.Uint64

	http *http.Server
}

// SetJWTVerifier — DMZ public key 기반 access JWT 검증기.
// 호출하지 않으면 DevMode 만 동작.
func (s *Server) SetJWTVerifier(v *auth.Verifier) { s.jwtVer = v }

// SetPairValidator — 외부에서 PairValidator 주입 (테스트 / 운영 wiring).
// nil 로 설정하면 모든 pair 허용 모드로 회귀.
func (s *Server) SetPairValidator(v PairValidator) { s.pairValidator = v }

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
			onFail := func() { s.metrics.IncRateLimitRedisFail("mci-edge-price") }
			ruleFactory, fallbackFactory = ratelimit.MakeRedisFactoriesWithOnFail(s.rateLimitRedis, "edge-price", logger, onFail)
			logger.Info("rate limit Redis backend 활성", slog.String("addr", cfg.RateLimitRedisAddr))
		}
		rs, err := ratelimit.NewRuleSetWithFactory(rules, fallback, ruleFactory, fallbackFactory)
		if err != nil {
			logger.Error("rate limit 룰셋 빌드 실패", slog.Any("error", err))
		} else {
			s.rateLimit = rs
		}
	}
	// Phase 2 권한 가드 — seed 가 있거나 quote-stream 활성이면 MemoryPairValidator
	// 자동 wire. 운영자가 SetPairValidator 로 명시 주입한 경우엔 그쪽 우선.
	if len(cfg.QuoteSeedPairs) > 0 {
		v := NewMemoryPairValidator()
		v.Add(cfg.QuoteSeedPairs...)
		s.pairValidator = v
	}
	// Phase 4 liveness — StaleThreshold>0 일 때만 활성. scanner 는 Start 에서 가동.
	if cfg.StaleThreshold > 0 {
		s.liveness = newPairLiveness()
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
	conn, err := grpc.NewClient(s.cfg.UpstreamGRPC,
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
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
	// Phase 4c — customer-specific quote 경로 활성.
	if s.cfg.EnableCustomerStream {
		s.customerRegMgr = newCustomerRegManager(s.upstream, s.cfg.SubscriberID, s.logger)
		s.customerRegMgr.Start(streamCtx)
		go s.subscribeCustomerQuoteLoop(streamCtx)
		s.logger.Info("Customer-specific quote 경로 활성")
	}
	// Phase 4 stale scanner — liveness 가 nil 이면 함수 자체가 early return.
	go s.runStaleScanner(streamCtx)

	if err := s.startRateLimitWatcher(ctx); err != nil {
		s.logger.Warn("ratelimit etcd watcher 시작 실패 — 정적 룰", slog.Any("error", err))
	}

	if err := s.startCustomerPairsWatcher(ctx); err != nil {
		s.logger.Warn("customer-pair etcd watcher 시작 실패 — 글로벌 정책만", slog.Any("error", err))
	}

	return s.startHTTP(ctx)
}

// startCustomerPairsWatcher — EtcdEndpoints + EtcdCustomerPairsPrefix 둘 다
// 채워지면 활성. MemoryCustomerPairPolicy 생성 후 etcd watcher 가동.
// 빈값이면 no-op (글로벌 정책만 — backward compat).
func (s *Server) startCustomerPairsWatcher(ctx context.Context) error {
	if s.cfg.EtcdEndpoints == "" || s.cfg.EtcdCustomerPairsPrefix == "" {
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
	cp := NewMemoryCustomerPairPolicy()
	w := NewEtcdCustomerPairWatcher(cli, s.cfg.EtcdCustomerPairsPrefix, cp, s.logger)
	if err := w.Start(ctx); err != nil {
		_ = cli.Close()
		return fmt.Errorf("customer-pair watcher Start: %w", err)
	}
	s.customerPairs = cp
	s.customerPairsWatcher = w
	s.customerPairsEtcdCli = cli
	s.logger.Info("CustomerPairPolicy etcd watcher 활성",
		slog.String("prefix", s.cfg.EtcdCustomerPairsPrefix))
	return nil
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

// subscribeCustomerQuoteLoop — Phase 4c. SubscribeCustomerQuote stream 유지 +
// 끊김 시 재연결.
func (s *Server) subscribeCustomerQuoteLoop(ctx context.Context) {
	client := wtgpb.NewPriceServiceClient(s.upstream)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.consumeCustomerQuoteOnce(ctx, client)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		s.logger.Warn("SubscribeCustomerQuote stream 끊김 — 재시도",
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

// consumeCustomerQuoteOnce — 단일 SubscribeCustomerQuote stream 의 lifecycle.
//
// edge 는 본 stream 으로 customer-tag 된 quote 를 받아 매칭 ws subscriber 에게
// fan-out (Registry.SendByCustomerID). filter 는 빈 customer_ids = 모두 — 단일
// edge 인스턴스가 자기가 등록한 customer 들의 quote 를 모두 받는 모델.
//
// 운영 최적화 (subscriber_id 기반 자동 ownership 매칭) 는 후속 단계.
func (s *Server) consumeCustomerQuoteOnce(ctx context.Context, client wtgpb.PriceServiceClient) error {
	req := &wtgpb.CustomerQuoteSubscribeRequest{
		SubscriberId: s.cfg.SubscriberID,
		// CustomerIds 비움 — 모두 수신. 서버측은 edge 가 자기 등록 customer 만
		// 받을지 명시 customer_ids 로 좁힐 수 있도록 P4c 후속에서 옵션화 예정.
	}
	stream, err := client.SubscribeCustomerQuote(ctx, req)
	if err != nil {
		return err
	}
	s.logger.Info("SubscribeCustomerQuote 시작",
		slog.String("subscriber_id", s.cfg.SubscriberID),
	)

	for {
		cq, err := stream.Recv()
		if err == io.EOF {
			return errors.New("upstream customer-quote EOF")
		}
		if err != nil {
			return err
		}
		payload, err := encodeCustomerQuoteJSON(cq)
		if err != nil {
			s.logger.Warn("customerQuote JSON 직렬화 실패 (customer)", slog.Any("error", err))
			continue
		}
		// Phase 2 passive learning — customer 경로 pair 도 valid set 에 등록.
		if mv, ok := s.pairValidator.(*MemoryPairValidator); ok && mv != nil {
			mv.Add(cq.GetPair())
		}
		// liveness — customer 경로도 freshness 마킹.
		if s.liveness != nil {
			if became := s.liveness.Update(cq.GetPair(), time.Now()); became {
				s.sendFreshNotification(cq.GetPair())
			}
		}
		s.registry.SendByCustomerID(cq.GetCustomerId(), cq.GetPair(), payload)
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
		// Phase 2 passive learning — 도착한 quote 의 pair 를 validator 에 등록.
		// upstream PricingTable 이 실제로 발행하는 pair 만 점진적으로 허용 set
		// 에 들어가 — 운영자가 seed 안 줘도 첫 quote 도착 후 subscribe 가능.
		if mv, ok := s.pairValidator.(*MemoryPairValidator); ok && mv != nil {
			mv.Add(cq.GetPair())
		}
		// Phase 4 liveness — quote 도착 마킹. stale 상태였다가 회복되면 fresh
		// 알림 발송 (그 pair 매칭하는 모든 subscriber 에게).
		if s.liveness != nil {
			if became := s.liveness.Update(cq.GetPair(), time.Now()); became {
				s.sendFreshNotification(cq.GetPair())
			}
		}
		s.registry.SendByProfile(profKey, cq.GetPair(), payload)
	}
}

// encodeCustomerQuoteJSON 은 proto CustomerQuote → 클라이언트 JSON.
// QuoteID / ValidUntilUnixNano / CustomerID 는 비활성 또는 무관 경로에선 빈
// 값이므로 omitempty.
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
		CustomerID         string  `json:"customer_id,omitempty"` // P4c
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
		CustomerID:         cq.GetCustomerId(),
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
		// format 분기 — "legacy" 면 forwarder/broker subscribe 호환 형식,
		// 그 외 default "best".
		var payload []byte
		if s.cfg.EnvelopeFormat == "legacy" {
			payload, err = encodeTickLegacyJSON(tick)
		} else {
			payload, err = encodeTickJSON(tick)
		}
		if err != nil {
			s.logger.Warn("tick JSON 직렬화 실패",
				slog.String("format", s.cfg.EnvelopeFormat),
				slog.Any("error", err))
			continue
		}
		s.registry.Broadcast(payload)
	}
}

// encodeTickJSON 은 proto Tick → 클라이언트 JSON envelope (default "best" 형식).
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

// encodeTickLegacyJSON — legacy cs framework 호환 envelope.
//
// cs framework 가 mymqd broker subscribe 시 받던 형식 (quote-forwarder 의 출력):
//
//	{
//	  "ts":      "<RFC3339 nano>",
//	  "feed":    "BEST",            // 다중시장 산정 후 합성 source
//	  "seq":     <seq_num>,
//	  "msgtype": "incremental",
//	  "symbol":  "USDKRW",
//	  "entries": [
//	    {"type":"bid", "px":<bid>, "qty":0},
//	    {"type":"ask", "px":<ask>, "qty":0}
//	  ]
//	}
//
// best tick (proto Tick.body) 의 평면 {sym, bid, ask, src, seq, ts} 를 위 형식으로
// 변환. 단방향 (best → legacy). 한쪽 호가만 있으면 그것만 entry 추가.
func encodeTickLegacyJSON(t *wtgpb.Tick) ([]byte, error) {
	body := t.GetBody()
	if len(body) == 0 || !json.Valid(body) {
		// best tick 이 아닌 raw 페이로드 — passthrough 불가. drop 신호로 빈 결과.
		return nil, fmt.Errorf("legacy 변환 불가: body 가 JSON 아님")
	}
	var probe struct {
		Sym string  `json:"sym"`
		Bid float64 `json:"bid"`
		Ask float64 `json:"ask"`
		Src string  `json:"src"`
		Seq uint32  `json:"seq"`
		TS  string  `json:"ts"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("legacy 변환 unmarshal: %w", err)
	}
	type mdEntry struct {
		Type string  `json:"type"`
		Px   float64 `json:"px"`
		Qty  float64 `json:"qty"`
	}
	type legacyEnvelope struct {
		TS      string    `json:"ts,omitempty"`
		Feed    string    `json:"feed,omitempty"`
		Seq     uint32    `json:"seq,omitempty"`
		MsgType string    `json:"msgtype"`
		Symbol  string    `json:"symbol,omitempty"`
		Entries []mdEntry `json:"entries"`
	}
	out := legacyEnvelope{
		TS:      probe.TS,
		Feed:    probe.Src, // "BEST"
		Seq:     probe.Seq,
		MsgType: "incremental",
		Symbol:  probe.Sym,
		Entries: make([]mdEntry, 0, 2),
	}
	if probe.Bid != 0 {
		out.Entries = append(out.Entries, mdEntry{Type: "bid", Px: probe.Bid})
	}
	if probe.Ask != 0 {
		out.Entries = append(out.Entries, mdEntry{Type: "ask", Px: probe.Ask})
	}
	return json.Marshal(out)
}

// BuildHandler — 미들웨어 chain 까지 적용된 최종 http.Handler 반환. 테스트
// 와 startHTTP 가 동일 chain 공유.
func (s *Server) BuildHandler() http.Handler {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// CheckOrigin: 운영은 nil = gorilla default (same-origin). DevMode 는 모두 허용 —
		// admin UI(:9090) → edge-price(:8083) wsmon 같은 cross-origin 진단 가능.
	}
	if s.cfg.DevMode {
		upgrader.CheckOrigin = func(r *http.Request) bool { return true }
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
		out := map[string]any{
			"received":             s.totalRecv.Load(),
			"registry":             s.registry.Stats(),
			"rejected_pairs_total": s.totalRejectedPairs.Load(),
		}
		if s.pairValidator != nil {
			out["allowed_pairs"] = s.pairValidator.AllowedSnapshot()
		}
		if s.liveness != nil {
			out["liveness"] = s.liveness.Snapshot()
			out["stale_sent_total"] = s.totalStaleSent.Load()
			out["fresh_sent_total"] = s.totalFreshSent.Load()
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/subscribe", s.subscribeHandler(upgrader))
	// 운영 진단 — 현재 연결된 모든 ws 클라이언트의 detail. mci-price /v1/subscribers
	// 가 internal gRPC layer (어느 edge 가 어느 Profile 받는지) 를 노출한 것과
	// 짝 — 본 endpoint 는 external ws layer (어떤 customer / remote_addr 가 본
	// edge instance 에 붙어있는지) 노출. 양 layer 합쳐서 end-to-end 가시화.
	mux.HandleFunc("GET /v1/connections", func(w http.ResponseWriter, r *http.Request) {
		snap := s.registry.Snapshot()
		// by_profile 집계 (필터 적용 전 — 전체 분포).
		byProfile := map[string]int{}
		for _, sub := range snap {
			byProfile[sub.ProfileKey]++
		}
		// optional 필터 — 운영 시나리오:
		//   - ?customer_id=X : support 티켓의 customer 가 이 instance 에 붙어있나
		//   - ?profile=X    : 특정 Profile 만 분리해서 보기
		if cid := r.URL.Query().Get("customer_id"); cid != "" {
			filtered := snap[:0:0]
			for _, sub := range snap {
				if sub.CustomerID == cid {
					filtered = append(filtered, sub)
				}
			}
			snap = filtered
		}
		if prof := r.URL.Query().Get("profile"); prof != "" {
			filtered := snap[:0:0]
			for _, sub := range snap {
				if sub.ProfileKey == prof {
					filtered = append(filtered, sub)
				}
			}
			snap = filtered
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"count":       len(snap),
			"by_profile":  byProfile, // 필터 무관 전체 분포 — 컨텍스트 유지
			"connections": snap,
		})
	})
	// N7. backpressure history — checkBackpressure 가 WARN 발생 시 ring buffer.
	mux.HandleFunc("GET /v1/backpressure", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, SnapshotBackpressureStats())
	})
	mux.Handle("GET /metrics", s.metrics.Handler())

	// DevMode 한정 pprof — burst PoC 진단용. 운영 비활성.
	if s.cfg.DevMode {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		mux.Handle("GET /debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("GET /debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("GET /debug/pprof/block", pprof.Handler("block"))
		mux.Handle("GET /debug/pprof/mutex", pprof.Handler("mutex"))
	}

	// Phase 4b admin endpoint — 별도 IP allowlist 가드. cfg.AdminAllowCIDRs
	// 가 비어있으면 모든 admin 요청 거부 (default secure). 일반 ws AllowCIDRs
	// 와 별개 — 운영망에서만 좁게 허용 권장.
	mux.HandleFunc("POST /v1/admin/disallow-pair", s.guardAdmin(s.adminDisallowPair()))
	mux.HandleFunc("POST /v1/admin/allow-pair", s.guardAdmin(s.adminAllowPair()))

	authMW := middleware.Auth(middleware.AuthConfig{
		DevMode:     s.cfg.DevMode,
		JWTVerifier: s.jwtVer,
		Logger:      s.logger,
	})
	mws := []middleware.Middleware{
		authMW,
		// ws 클라이언트 호환 — query 의 access_token / x_wtg_user 를 헤더로 변환.
		// 브라우저 WebSocket API 가 사용자 정의 헤더 못 주는 환경 (admin UI WS 모니터 등) 대응.
		// 운영(JWT): BearerFromQuery → Authorization: Bearer. DevMode: UserFromQuery → X-WTG-User.
		middleware.BearerFromQuery(),
		middleware.UserFromQuery(),
		metrics.HTTPMiddleware(s.metrics, "mci-edge-price"),
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
					s.metrics.IncRateLimit("mci-edge-price", kind, rule, true)
				},
				OnDenied: func(rule, kind string) {
					s.metrics.IncRateLimit("mci-edge-price", kind, rule, false)
				},
			},
		))
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

		// Phase 4c — customer-specific quote 활성 시 ws 클라이언트의 customer
		// 식별자 결정. Principal.Usid 가 자연스러운 customer key. 또한 customer
		// 별 pair allowlist (CustomerPairPolicy) 가 활성이면 quote stream 비활성
		// 이라도 customer 식별 필요.
		var customerID string
		if profileKey != "" && p.Usid != "" {
			if (s.cfg.EnableCustomerStream && s.customerRegMgr != nil) || s.customerPairs != nil {
				customerID = p.Usid
			}
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
			CustomerID:    customerID,
			OnClose: func(sb *Subscriber) {
				s.registry.Remove(sb)
				// Phase 4c — ws disconnect 시 customer 등록 해제.
				if sb.customerID != "" && s.customerRegMgr != nil {
					s.customerRegMgr.Unregister(sb.customerID)
				}
			},
		})
		s.registry.Add(sub)

		// Phase 4c — ws connect 시 customer 등록 (비블로킹 enqueue).
		if customerID != "" && s.customerRegMgr != nil {
			s.customerRegMgr.Register(customerID, profileKey)
		}

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
		accepted, rejected := s.gateSubscribe(sub, req.Pairs)
		sub.SubscribePairs(accepted)
		if len(rejected) > 0 {
			s.totalRejectedPairs.Add(uint64(len(rejected)))
			s.sendControlPairReject(sub, rejected)
			// echo 도 함께 — 클라이언트가 현재 활성 set 확인할 수 있게.
		}
	case "unsubscribe":
		sub.UnsubscribePairs(req.Pairs)
	default:
		s.sendControlError(sub, "unknown_type", "지원하지 않는 type: "+req.Type)
		return
	}
	s.sendControlEcho(sub)
}

// gateSubscribe — ws subscribe 권한 가드. 두 정책 결합:
//
//  1. 글로벌 pairValidator (Phase 2) — nil 이면 모두 통과 (backward compat).
//  2. customerPairs (PoC) — customer 미등록이면 무관 (unrestricted),
//     등록되면 글로벌 ∩ customer 허용 set 만 accept.
//
// 글로벌 disallow 가 항상 우선 (emergency cut). 빈 string 은 무조건 reject.
func (s *Server) gateSubscribe(sub *Subscriber, pairs []string) (accepted, rejected []string) {
	// customer policy 사전 조회 (등록된 경우만 적용).
	var customerAllowed map[string]struct{}
	customerRegistered := false
	if s.customerPairs != nil && sub != nil && sub.CustomerID() != "" {
		allowed, ok := s.customerPairs.AllowedFor(sub.CustomerID())
		if ok {
			customerRegistered = true
			customerAllowed = make(map[string]struct{}, len(allowed))
			for _, p := range allowed {
				customerAllowed[p] = struct{}{}
			}
		}
	}

	accepted = make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			rejected = append(rejected, p)
			continue
		}
		// 글로벌 정책 — 우선.
		if s.pairValidator != nil && !s.pairValidator.IsAllowed(p) {
			rejected = append(rejected, p)
			continue
		}
		// customer 정책 — 등록된 경우만 추가 필터.
		if customerRegistered {
			if _, ok := customerAllowed[p]; !ok {
				rejected = append(rejected, p)
				continue
			}
		}
		accepted = append(accepted, p)
	}
	return accepted, rejected
}

// sendControlPairReject — 거절된 pair 목록 알림. ws 끊지 않음.
//
//	{"type":"error","code":"forbidden_pair","rejected":["..."],"message":"..."}
func (s *Server) sendControlPairReject(sub *Subscriber, rejected []string) {
	payload, err := json.Marshal(map[string]any{
		"type":     "error",
		"code":     "forbidden_pair",
		"rejected": rejected,
		"message":  "허용되지 않은 pair (PricingTable 미등록 또는 미허가)",
	})
	if err != nil {
		return
	}
	_ = sub.Send(payload)
}

// sendStaleNotification — pair 의 stale 진입 알림 (그 pair 매칭 sub 들에게).
//
//	{"type":"stale","pair":"USD/KRW","threshold_sec":30}
func (s *Server) sendStaleNotification(pair string, threshold time.Duration) {
	payload, err := json.Marshal(map[string]any{
		"type":          "stale",
		"pair":          pair,
		"threshold_sec": int(threshold.Seconds()),
	})
	if err != nil {
		return
	}
	sent, _ := s.registry.BroadcastForPair(pair, payload)
	s.totalStaleSent.Add(uint64(sent))
}

// sendFreshNotification — pair 의 회복 알림 (stale 였다가 quote 다시 옴).
//
//	{"type":"fresh","pair":"USD/KRW"}
func (s *Server) sendFreshNotification(pair string) {
	payload, err := json.Marshal(map[string]any{
		"type": "fresh",
		"pair": pair,
	})
	if err != nil {
		return
	}
	sent, _ := s.registry.BroadcastForPair(pair, payload)
	s.totalFreshSent.Add(uint64(sent))
}

// runStaleScanner — 백그라운드 goroutine 으로 pair liveness 주기 스캔.
// 신규 stale pair 에 대해 알림 발송. context cancel 시 종료.
func (s *Server) runStaleScanner(ctx context.Context) {
	if s.liveness == nil || s.cfg.StaleThreshold <= 0 {
		return
	}
	interval := s.cfg.StaleScanInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			newlyStale := s.liveness.ScanForStale(now, s.cfg.StaleThreshold)
			for _, pair := range newlyStale {
				s.logger.Warn("pair stale — 알림 발송",
					slog.String("pair", pair),
					slog.Duration("threshold", s.cfg.StaleThreshold),
				)
				s.sendStaleNotification(pair, s.cfg.StaleThreshold)
			}
		}
	}
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
	if s.customerPairsWatcher != nil {
		s.customerPairsWatcher.Stop()
	}
	if s.customerPairsEtcdCli != nil {
		_ = s.customerPairsEtcdCli.Close()
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
