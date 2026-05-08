package price

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

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

	nextSubID atomic.Uint64
}

// subscriber 는 단일 stream 의 상태.
type subscriber struct {
	id      uint64
	symbols map[string]struct{} // 빈 set 이면 모두 통과
	out     chan *wtgpb.Tick    // server-side 큐
	srvID   string              // 디버깅
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
		logger:      logger,
		bufSz:       bufSize,
		subscribers: make(map[uint64]*subscriber),
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

// SubscriberCount 는 현재 활성 구독자 수.
func (g *GRPCServer) SubscriberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.subscribers)
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
