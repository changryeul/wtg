package push

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/winwaysystems/wtg/pkg/mymq"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// GRPCServer 는 mci-push 가 노출하는 PushService.
//
// 동작:
//   - DMZ 의 mci-edge-push 가 PushService.Subscribe 호출 → reverse stream
//   - Dispatcher 가 unsolicited 메시지를 받으면 (1) ws Registry, (2) gRPC
//     subscribers 양쪽으로 fan-out
//   - 다수 edge 동시 구독 OK
//   - slow consumer 격리: 구독자별 buffered chan, 가득 차면 stream 종료
type GRPCServer struct {
	wtgpb.UnimplementedPushServiceServer

	logger *slog.Logger
	bufSz  int

	mu          sync.RWMutex
	subscribers map[uint64]*pushSubscriber

	nextSubID atomic.Uint64
}

type pushSubscriber struct {
	id    uint64
	out   chan *wtgpb.PushMessage
	srvID string
}

// NewGRPCServer 는 GRPCServer 를 생성. bufSize 는 구독자별 큐 (기본 1024).
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
		subscribers: make(map[uint64]*pushSubscriber),
	}
}

// OnUnsolicited 는 Dispatcher 가 호출하는 fan-out 진입점.
// ws Registry 와 별도로 gRPC 구독자들에게도 메시지 전달.
//
// 메시지 종류:
//   - FCCast/FCPush/FCSignal 만 처리 (그 외 무시).
//   - logon_id, channel, exchange 등 broadcast prefix 정보 그대로 전달.
//   - body 는 raw bytes — edge 가 ws 클라이언트로 forward.
func (g *GRPCServer) OnUnsolicited(msg *mymq.Unsolicited) {
	if msg == nil {
		return
	}
	switch msg.Header.Func {
	case mymq.FCCast, mymq.FCPush, mymq.FCSignal:
	default:
		return
	}
	pb := unsolicitedToProto(msg)

	g.mu.RLock()
	subs := make([]*pushSubscriber, 0, len(g.subscribers))
	for _, s := range g.subscribers {
		subs = append(subs, s)
	}
	g.mu.RUnlock()

	for _, s := range subs {
		select {
		case s.out <- pb:
		default:
			g.logger.Warn("gRPC subscriber slow — stream 종료",
				slog.Uint64("sub_id", s.id),
				slog.String("subscriber_id", s.srvID),
			)
			close(s.out)
			g.removeSubscriber(s.id)
		}
	}
}

// Subscribe 는 PushService.Subscribe 구현 (server-streaming).
func (g *GRPCServer) Subscribe(req *wtgpb.PushSubscribeRequest, stream wtgpb.PushService_SubscribeServer) error {
	sub := &pushSubscriber{
		id:    g.nextSubID.Add(1),
		out:   make(chan *wtgpb.PushMessage, g.bufSz),
		srvID: req.GetSubscriberId(),
	}

	g.mu.Lock()
	g.subscribers[sub.id] = sub
	g.mu.Unlock()

	g.logger.Info("PushService 구독 시작",
		slog.Uint64("sub_id", sub.id),
		slog.String("subscriber_id", sub.srvID),
	)

	defer func() {
		g.removeSubscriber(sub.id)
		g.logger.Info("PushService 구독 종료",
			slog.Uint64("sub_id", sub.id),
			slog.String("subscriber_id", sub.srvID),
		)
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-sub.out:
			if !ok {
				return errors.New("push: slow consumer")
			}
			if err := stream.Send(m); err != nil {
				return err
			}
		}
	}
}

// removeSubscriber 는 idempotent 제거.
func (g *GRPCServer) removeSubscriber(id uint64) {
	g.mu.Lock()
	delete(g.subscribers, id)
	g.mu.Unlock()
}

// SubscriberCount 는 활성 구독자 수.
func (g *GRPCServer) SubscriberCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.subscribers)
}

// Serve 는 별도 listener 에서 gRPC 서버 가동.
func (g *GRPCServer) Serve(ctx context.Context, addr string, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// OTel — server-side gRPC trace + metrics. otelgrpc stats handler 가
	// unary/streaming 모두 자동 처리. TracerProvider 등록 안 된 환경은 no-op.
	opts = append([]grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	}, opts...)
	srv := grpc.NewServer(opts...)
	wtgpb.RegisterPushServiceServer(srv, g)

	g.logger.Info("PushService gRPC listen 시작", slog.String("addr", addr))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	select {
	case <-ctx.Done():
		srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

// unsolicitedToProto 는 mymq.Unsolicited → proto PushMessage.
func unsolicitedToProto(m *mymq.Unsolicited) *wtgpb.PushMessage {
	pb := &wtgpb.PushMessage{
		Func:             uint32(m.Header.Func),
		Subc:             uint32(m.Header.Subc),
		Data:             append([]byte(nil), m.Body...),
		ReceivedUnixNano: time.Now().UnixNano(),
	}
	if m.Prefix != nil {
		pb.Exchange = m.Prefix.ExchangeString()
		pb.Channel = m.Prefix.ChanString()
		pb.LogonId = m.Prefix.LogonIDString()
	}
	return pb
}
