package price

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/winwaysystems/wtg/pkg/pricing"
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

	nextSubID atomic.Uint64
}

// subscriber 는 단일 stream 의 상태.
type subscriber struct {
	id      uint64
	symbols map[string]struct{} // 빈 set 이면 모두 통과
	out     chan *wtgpb.Tick    // server-side 큐
	srvID   string              // 디버깅
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

// customerQuoteToProto 는 pricing.CustomerQuote 를 proto CustomerQuote 로 매핑.
func customerQuoteToProto(profile session.Profile, cq pricing.CustomerQuote) *wtgpb.CustomerQuote {
	return &wtgpb.CustomerQuote{
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
	}
}
