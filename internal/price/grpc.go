package price

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// GRPCServer 는 mci-price 가 노출하는 PriceService gRPC 서버.
//
// 동작:
//   - DMZ 의 mci-edge-price 가 PriceService.Subscribe 호출 → reverse stream 시작
//   - mci-price 의 TickConsumer 로 등록 — Server.subscribeLoop 가 fan-out 해줌
//   - 다수 edge 가 동시 구독 가능 (각각 독립 stream)
//   - slow consumer 격리: 구독자별 buffered channel, 가득 차면 stream 종료
//
// 보안: 1차 prototype 은 plain gRPC. Phase 8 (운영화) 시점에 mTLS 적용.
type GRPCServer struct {
	wtgpb.UnimplementedPriceServiceServer

	logger *slog.Logger
	bufSz  int

	mu          sync.RWMutex
	subscribers map[uint64]*subscriber

	qmu              sync.RWMutex
	quoteSubscribers map[uint64]*quoteSubscriber

	bmu            sync.RWMutex
	barSubscribers map[uint64]*barSubscriber

	nextSubID atomic.Uint64

	// 선택적 QuoteValidationService — nil 이면 Serve 가 register 안 함.
	// 같은 gRPC 서버 / 같은 listen 포트에 합쳐서 노출 (RFC §3 결정).
	validator *QuoteValidationServer

	// serv 는 PublishTick handler 가 받은 tick 을 hot path 에 dispatch 하기 위한
	// Server 참조. AttachServer 로 주입. nil 이면 PublishTick 비활성 (Unimplemented).
	serv *Server

	// PublishTick 통계.
	publishAccepted atomic.Uint64
	publishDropped  atomic.Uint64
}

// AttachServer — PublishTick handler 가 dispatch 할 Server 주입.
// Server.AttachGRPC 의 대칭 — Server 가 GRPCServer 를 consumer 로 등록할 때 같이 호출.
func (g *GRPCServer) AttachServer(s *Server) {
	g.serv = s
}

// AttachValidator — Serve 호출 전에 옵션 등록.
func (g *GRPCServer) AttachValidator(v *QuoteValidationServer) {
	g.validator = v
}

// subscriber 는 단일 stream 의 상태.
type subscriber struct {
	id      uint64
	symbols map[string]struct{} // 빈 set 이면 모두 통과
	out     chan *wtgpb.Tick    // server-side 큐
	srvID   string              // 디버깅
}

// barSubscriber 는 단일 SubscribeBar stream 의 상태.
type barSubscriber struct {
	id    uint64
	tfs   map[string]struct{} // 빈 set = 모두 통과
	pairs map[string]struct{}
	out   chan *wtgpb.Bar
	srvID string
}

// quoteSubscriber 는 단일 SubscribeQuote stream 의 상태.
//
// 필터:
//   - profiles : profile key set (예: "WEB.BRANCH.VIP"). 빈 set 이면 모두 통과.
//   - pairs    : pair set (예: "USD/KRW"). 빈 set 이면 모두 통과.
//
// edge-price 는 보통 자기 담당 Profile 들만 받기 위해 profile_keys 를 명시한다.
type quoteSubscriber struct {
	id       uint64
	profiles map[string]struct{}
	pairs    map[string]struct{}
	out      chan *wtgpb.CustomerQuote
	srvID    string
}

// NewGRPCServer 는 GRPCServer 를 생성.
// bufSize 는 구독자별 큐 크기 (기본 1024).
func NewGRPCServer(logger *slog.Logger, bufSize int) *GRPCServer {
	if logger == nil {
		logger = slog.Default()
	}
	if bufSize <= 0 {
		bufSize = 1024
	}
	return &GRPCServer{
		logger:           logger,
		bufSz:            bufSize,
		subscribers:      make(map[uint64]*subscriber),
		quoteSubscribers: make(map[uint64]*quoteSubscriber),
		barSubscribers:   make(map[uint64]*barSubscriber),
	}
}

// OnTick 은 Server (price.Server) 의 TickConsumer 인터페이스 구현.
// 모든 활성 구독자에게 fan-out 한다 (필터링 + non-blocking enqueue).
func (g *GRPCServer) OnTick(t *Tick) {
	if t == nil {
		return
	}
	pb := tickToProto(t)

	g.mu.RLock()
	subs := make([]*subscriber, 0, len(g.subscribers))
	for _, s := range g.subscribers {
		subs = append(subs, s)
	}
	g.mu.RUnlock()

	for _, s := range subs {
		// 심볼 필터링 (빈 set 이면 모두 통과).
		if len(s.symbols) > 0 {
			if _, ok := s.symbols[t.Symbol]; !ok {
				continue
			}
		}
		// non-blocking enqueue — slow consumer 는 stream 종료로 격리.
		select {
		case s.out <- pb:
		default:
			g.logger.Warn("gRPC subscriber slow — stream 종료",
				slog.Uint64("sub_id", s.id),
				slog.String("subscriber_id", s.srvID),
			)
			// 큐 종료 신호 — Subscribe RPC 안에서 감지.
			close(s.out)
			g.removeSubscriber(s.id)
		}
	}
}

// Subscribe 는 PriceService.Subscribe RPC 구현.
// 클라이언트가 호출하면 신규 구독자로 등록 후, 채널이 닫힐 때까지 stream 송신.
func (g *GRPCServer) Subscribe(req *wtgpb.SubscribeRequest, stream wtgpb.PriceService_SubscribeServer) error {
	sub := &subscriber{
		id:      g.nextSubID.Add(1),
		symbols: make(map[string]struct{}, len(req.GetSymbols())),
		out:     make(chan *wtgpb.Tick, g.bufSz),
		srvID:   req.GetSubscriberId(),
	}
	for _, s := range req.GetSymbols() {
		sub.symbols[s] = struct{}{}
	}

	g.mu.Lock()
	g.subscribers[sub.id] = sub
	g.mu.Unlock()

	g.logger.Info("gRPC 구독 시작",
		slog.Uint64("sub_id", sub.id),
		slog.String("subscriber_id", sub.srvID),
		slog.Int("symbols", len(sub.symbols)),
	)

	defer func() {
		g.removeSubscriber(sub.id)
		g.logger.Info("gRPC 구독 종료",
			slog.Uint64("sub_id", sub.id),
			slog.String("subscriber_id", sub.srvID),
		)
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case tick, ok := <-sub.out:
			if !ok {
				// slow consumer 격리로 close 됨.
				return errors.New("price: slow consumer")
			}
			if err := stream.Send(tick); err != nil {
				return err
			}
		}
	}
}

// removeSubscriber 는 구독자 맵에서 제거 (idempotent).
func (g *GRPCServer) removeSubscriber(id uint64) {
	g.mu.Lock()
	delete(g.subscribers, id)
	g.mu.Unlock()
}

// SubscriberCount 는 현재 활성 Tick 구독자 수.
func (g *GRPCServer) SubscriberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.subscribers)
}

// ─── Quote stream ──────────────────────────────────────────────────────────

// PublishQuote 는 PricingConsumer.QuotePublisher 인터페이스 구현.
// 활성 quote subscriber 중 filter 통과 대상에게 fan-out (non-blocking).
//
// 항상 nil 반환 — 개별 subscriber 실패는 로컬 격리 (PricingConsumer 의 전체
// publish 를 실패시키지 않는다).
func (g *GRPCServer) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	pb := customerQuoteToProto(profile, cq)

	g.qmu.RLock()
	subs := make([]*quoteSubscriber, 0, len(g.quoteSubscribers))
	for _, s := range g.quoteSubscribers {
		subs = append(subs, s)
	}
	g.qmu.RUnlock()

	profKey := profile.Key()
	pairStr := string(cq.Pair)
	for _, s := range subs {
		if len(s.profiles) > 0 {
			if _, ok := s.profiles[profKey]; !ok {
				continue
			}
		}
		if len(s.pairs) > 0 {
			if _, ok := s.pairs[pairStr]; !ok {
				continue
			}
		}
		select {
		case s.out <- pb:
		default:
			g.logger.Warn("gRPC quote subscriber slow — stream 종료",
				slog.Uint64("sub_id", s.id),
				slog.String("subscriber_id", s.srvID),
			)
			close(s.out)
			g.removeQuoteSubscriber(s.id)
		}
	}
	return nil
}

// SubscribeQuote 는 PriceService.SubscribeQuote RPC 구현.
func (g *GRPCServer) SubscribeQuote(req *wtgpb.QuoteSubscribeRequest, stream wtgpb.PriceService_SubscribeQuoteServer) error {
	sub := &quoteSubscriber{
		id:       g.nextSubID.Add(1),
		profiles: make(map[string]struct{}, len(req.GetProfileKeys())),
		pairs:    make(map[string]struct{}, len(req.GetPairs())),
		out:      make(chan *wtgpb.CustomerQuote, g.bufSz),
		srvID:    req.GetSubscriberId(),
	}
	for _, k := range req.GetProfileKeys() {
		sub.profiles[k] = struct{}{}
	}
	for _, p := range req.GetPairs() {
		sub.pairs[p] = struct{}{}
	}

	g.qmu.Lock()
	g.quoteSubscribers[sub.id] = sub
	g.qmu.Unlock()

	g.logger.Info("gRPC quote 구독 시작",
		slog.Uint64("sub_id", sub.id),
		slog.String("subscriber_id", sub.srvID),
		slog.Int("profile_filter", len(sub.profiles)),
		slog.Int("pair_filter", len(sub.pairs)),
	)

	defer func() {
		g.removeQuoteSubscriber(sub.id)
		g.logger.Info("gRPC quote 구독 종료",
			slog.Uint64("sub_id", sub.id),
			slog.String("subscriber_id", sub.srvID),
		)
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case cq, ok := <-sub.out:
			if !ok {
				return errors.New("price: slow quote consumer")
			}
			if err := stream.Send(cq); err != nil {
				return err
			}
		}
	}
}

func (g *GRPCServer) removeQuoteSubscriber(id uint64) {
	g.qmu.Lock()
	delete(g.quoteSubscribers, id)
	g.qmu.Unlock()
}

// QuoteSubscriberCount 는 현재 활성 Quote 구독자 수.
func (g *GRPCServer) QuoteSubscriberCount() int {
	g.qmu.RLock()
	defer g.qmu.RUnlock()
	return len(g.quoteSubscribers)
}

// HasQuoteSubscribers — Phase 3 QuoteSubscriberSink 인터페이스 구현.
// 현재 활성 quote subscriber 중 그 Profile 을 관심 대상에 두는 게 1+ 있나.
//
// 매칭 규칙은 PublishQuote 의 필터와 동일:
//   - s.profiles 가 빈 set 이면 모든 profile 매칭 (=구독자 1)
//   - 비어있지 않으면 profileKey 일치 시만
//
// 매 tick 호출되므로 lock 안에서 빠르게. RLock + map lookup 만 — 충분히 가벼움.
func (g *GRPCServer) HasQuoteSubscribers(profileKey string) bool {
	g.qmu.RLock()
	defer g.qmu.RUnlock()
	for _, s := range g.quoteSubscribers {
		if len(s.profiles) == 0 {
			return true // 무필터 = 모두 받음
		}
		if _, ok := s.profiles[profileKey]; ok {
			return true
		}
	}
	return false
}

// ─── Bar stream ────────────────────────────────────────────────────────────

// PublishBar 는 BarCloseHandler 시그니처 — Aggregator 의 onClose 콜백으로 등록 가능.
// 활성 bar subscriber 중 filter 통과 대상에게 fan-out (non-blocking, slow 격리).
func (g *GRPCServer) PublishBar(b *quote.Bar) {
	if b == nil {
		return
	}
	pb := barToProto(b)

	g.bmu.RLock()
	subs := make([]*barSubscriber, 0, len(g.barSubscribers))
	for _, s := range g.barSubscribers {
		subs = append(subs, s)
	}
	g.bmu.RUnlock()

	tfStr := string(b.TF)
	pairStr := string(b.Pair)
	for _, s := range subs {
		if len(s.tfs) > 0 {
			if _, ok := s.tfs[tfStr]; !ok {
				continue
			}
		}
		if len(s.pairs) > 0 {
			if _, ok := s.pairs[pairStr]; !ok {
				continue
			}
		}
		select {
		case s.out <- pb:
		default:
			g.logger.Warn("gRPC bar subscriber slow — stream 종료",
				slog.Uint64("sub_id", s.id),
				slog.String("subscriber_id", s.srvID),
			)
			close(s.out)
			g.removeBarSubscriber(s.id)
		}
	}
}

// SubscribeBar 는 PriceService.SubscribeBar RPC 구현.
func (g *GRPCServer) SubscribeBar(req *wtgpb.BarSubscribeRequest, stream wtgpb.PriceService_SubscribeBarServer) error {
	sub := &barSubscriber{
		id:    g.nextSubID.Add(1),
		tfs:   make(map[string]struct{}, len(req.GetTimeframes())),
		pairs: make(map[string]struct{}, len(req.GetPairs())),
		out:   make(chan *wtgpb.Bar, g.bufSz),
		srvID: req.GetSubscriberId(),
	}
	for _, t := range req.GetTimeframes() {
		sub.tfs[t] = struct{}{}
	}
	for _, p := range req.GetPairs() {
		sub.pairs[p] = struct{}{}
	}

	g.bmu.Lock()
	g.barSubscribers[sub.id] = sub
	g.bmu.Unlock()

	g.logger.Info("gRPC bar 구독 시작",
		slog.Uint64("sub_id", sub.id),
		slog.String("subscriber_id", sub.srvID),
		slog.Int("tf_filter", len(sub.tfs)),
		slog.Int("pair_filter", len(sub.pairs)),
	)

	defer func() {
		g.removeBarSubscriber(sub.id)
		g.logger.Info("gRPC bar 구독 종료",
			slog.Uint64("sub_id", sub.id),
			slog.String("subscriber_id", sub.srvID),
		)
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-sub.out:
			if !ok {
				return errors.New("price: slow bar consumer")
			}
			if err := stream.Send(b); err != nil {
				return err
			}
		}
	}
}

func (g *GRPCServer) removeBarSubscriber(id uint64) {
	g.bmu.Lock()
	delete(g.barSubscribers, id)
	g.bmu.Unlock()
}

// BarSubscriberCount 는 현재 활성 Bar 구독자 수.
func (g *GRPCServer) BarSubscriberCount() int {
	g.bmu.RLock()
	defer g.bmu.RUnlock()
	return len(g.barSubscribers)
}

// Serve 는 별도 listener 에서 gRPC 서버를 가동한다.
//
// 일반 사용:
//
//	lis, _ := net.Listen("tcp", ":50051")
//	gs := grpc.NewServer()
//	wtgpb.RegisterPriceServiceServer(gs, mygRPC)
//	gs.Serve(lis)
//
// 본 헬퍼는 그 boilerplate 를 묶었다.
func (g *GRPCServer) Serve(ctx context.Context, addr string, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := grpc.NewServer(opts...)
	wtgpb.RegisterPriceServiceServer(srv, g)
	if g.validator != nil {
		wtgpb.RegisterQuoteValidationServiceServer(srv, g.validator)
		g.logger.Info("QuoteValidationService 등록")
	}

	g.logger.Info("PriceService gRPC listen 시작", slog.String("addr", addr))

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(lis)
	}()
	select {
	case <-ctx.Done():
		srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

// tickToProto 는 internal Tick 을 proto Tick 으로 매핑.
func tickToProto(t *Tick) *wtgpb.Tick {
	return &wtgpb.Tick{
		MarketId:         t.MarketID,
		Symbol:           t.Symbol,
		SeqNum:           t.SeqNum,
		Mask:             t.Mask,
		Type:             uint32(t.Type),
		Flag:             uint32(t.Flag),
		Body:             append([]byte(nil), t.Body...),
		ReceivedUnixNano: t.Received.UnixNano(),
	}
}

// barToProto 는 quote.Bar 를 proto Bar 로 매핑.
func barToProto(b *quote.Bar) *wtgpb.Bar {
	return &wtgpb.Bar{
		Pair:           string(b.Pair),
		Tf:             string(b.TF),
		OpenedUnixNano: b.OpenedAt.UnixNano(),
		ClosedUnixNano: b.ClosedAt.UnixNano(),
		OpenBid:        b.OpenBid,
		OpenAsk:        b.OpenAsk,
		HighBid:        b.HighBid,
		HighAsk:        b.HighAsk,
		LowBid:         b.LowBid,
		LowAsk:         b.LowAsk,
		CloseBid:       b.CloseBid,
		CloseAsk:       b.CloseAsk,
		TickCount:      int32(b.TickCount),
	}
}

// customerQuoteToProto 는 pricing.CustomerQuote 를 proto CustomerQuote 로 매핑.
// PublishTick — quote-forwarder 가 broker 우회로 시세를 mci-price 에 직접 push 하는
// 양방향 stream. broker 의 PRICE exchange fan-out 부하를 분리 — broker 가 매매
// transaction RPC 에만 집중하도록 한다.
//
// 흐름:
//   1. forwarder 가 Tick (body=raw envelope JSON) 을 stream 으로 push
//   2. Server.IngestEnvelopes 로 broker subscribe path 와 동일 hot path 진입
//   3. ack 는 매 100건 또는 1초마다 한 번 (저빈도 — 통계/끊김 진단용)
//
// AttachServer 가 호출 안 됐으면 Unimplemented 반환.
func (g *GRPCServer) PublishTick(stream wtgpb.PriceService_PublishTickServer) error {
	if g.serv == nil {
		return status.Error(codes.Unimplemented, "PublishTick: Server 미주입 (AttachServer 필요)")
	}
	g.logger.Info("PublishTick 시작")
	defer g.logger.Info("PublishTick 종료",
		slog.Uint64("accepted_total", g.publishAccepted.Load()),
		slog.Uint64("dropped_total", g.publishDropped.Load()),
	)

	var localAccepted, localDropped uint64
	lastAck := time.Now()
	const ackEvery = 100
	const ackInterval = 1 * time.Second

	for {
		tick, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(tick.GetBody()) == 0 {
			localDropped++
			g.publishDropped.Add(1)
			continue
		}
		base := &Tick{
			MarketID: tick.GetMarketId(),
			Symbol:   tick.GetSymbol(),
			SeqNum:   tick.GetSeqNum(),
			Mask:     tick.GetMask(),
			Type:     uint8(tick.GetType()),
			Flag:     uint8(tick.GetFlag()),
		}
		g.serv.IngestEnvelopes(tick.GetBody(), base)
		localAccepted++
		g.publishAccepted.Add(1)

		// ack 주기 — N tick 또는 T 시간 (먼저 도달한 쪽).
		if localAccepted%ackEvery == 0 || time.Since(lastAck) >= ackInterval {
			ack := &wtgpb.PublishAck{
				Accepted:       localAccepted,
				Dropped:        localDropped,
				ServerUnixNano: time.Now().UnixNano(),
			}
			if err := stream.Send(ack); err != nil {
				return err
			}
			lastAck = time.Now()
		}
	}
}

func customerQuoteToProto(profile session.Profile, cq pricing.CustomerQuote) *wtgpb.CustomerQuote {
	pb := &wtgpb.CustomerQuote{
		Pair:         string(cq.Pair),
		Channel:      string(profile.Channel),
		Site:         string(profile.Site),
		Tier:         string(profile.Tier),
		Tenor:        string(cq.Tenor),
		Bid:          cq.Bid,
		Ask:          cq.Ask,
		TsUnixNano:   cq.TS.UnixNano(),
		RawBid:       cq.RawBid,
		RawAsk:       cq.RawAsk,
		TableVersion: cq.TableVersion,
		QuoteId:      cq.QuoteID,
	}
	if !cq.ValidUntil.IsZero() {
		pb.ValidUntilUnixNano = cq.ValidUntil.UnixNano()
	}
	return pb
}
