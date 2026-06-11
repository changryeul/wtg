package price

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startTestGRPC 는 in-memory gRPC server 를 띄우고 client conn 을 반환.
func startTestGRPC(t *testing.T, gs *GRPCServer) (wtgpb.PriceServiceClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	wtgpb.RegisterPriceServiceServer(srv, gs)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		t.Fatal(err)
	}
	return wtgpb.NewPriceServiceClient(conn), func() {
		_ = conn.Close()
		srv.Stop()
	}
}

func TestGRPCServerSubscribeBasic(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "test-sub"})
	if err != nil {
		t.Fatal(err)
	}

	// 잠시 대기해서 server 측 등록 완료.
	time.Sleep(50 * time.Millisecond)
	if gs.SubscriberCount() != 1 {
		t.Errorf("SubscriberCount: %d", gs.SubscriberCount())
	}

	// Tick fan-out.
	gs.OnTick(&Tick{Symbol: "USDKRW", SeqNum: 1, Body: []byte(`{"bid":1300}`)})
	tick, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if tick.GetSymbol() != "USDKRW" {
		t.Errorf("symbol: %q", tick.GetSymbol())
	}
	if tick.GetSeqNum() != 1 {
		t.Errorf("seq: %d", tick.GetSeqNum())
	}
}

func TestGRPCServerSymbolFilter(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{
		SubscriberId: "filter-test",
		Symbols:      []string{"USDKRW"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// 매칭되지 않는 심볼 — drop.
	gs.OnTick(&Tick{Symbol: "EURUSD", SeqNum: 1})
	// 매칭되는 심볼 — 전달.
	gs.OnTick(&Tick{Symbol: "USDKRW", SeqNum: 2})

	tick, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if tick.GetSymbol() != "USDKRW" {
		t.Errorf("필터링 실패: 받은 symbol=%q", tick.GetSymbol())
	}
}

func TestGRPCServerSubscriberRemovedOnClose(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "ctx-test"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if gs.SubscriberCount() != 1 {
		t.Errorf("count: %d", gs.SubscriberCount())
	}

	cancel() // 클라이언트 측 종료.
	// stream.Recv 는 ctx.Err 반환.
	_, _ = stream.Recv()

	// server 측 정리 시간.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if gs.SubscriberCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gs.SubscriberCount() != 0 {
		t.Errorf("subscriber 정리 실패: count=%d", gs.SubscriberCount())
	}
}

func TestGRPCServerMultipleSubscribers(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream1, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	stream2, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	gs.OnTick(&Tick{Symbol: "X", SeqNum: 99})

	for _, st := range []wtgpb.PriceService_SubscribeClient{stream1, stream2} {
		tick, err := st.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if tick.GetSeqNum() != 99 {
			t.Errorf("seq: %d", tick.GetSeqNum())
		}
	}
}

func TestGRPCServerSlowConsumerEviction(t *testing.T) {
	// 큐 size 1 — 첫 tick 전송 후 두 번째에서 slow consumer 격리.
	gs := NewGRPCServer(quietLogger(), 1)

	// In-memory: client 가 stream.Recv 를 호출 안 하도록 설정.
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer()
	wtgpb.RegisterPriceServiceServer(srv, gs)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, _ := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer conn.Close()
	client := wtgpb.NewPriceServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.Subscribe(ctx, &wtgpb.SubscribeRequest{SubscriberId: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	// stream.Recv 호출 안 함 → 큐 잡히면 slow.

	// 다수 tick 으로 큐 overflow 유도.
	for i := 0; i < 10; i++ {
		gs.OnTick(&Tick{Symbol: "X", SeqNum: uint32(i)})
	}

	// 격리 후 SubscriberCount 가 0 으로 감소해야 함.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gs.SubscriberCount() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gs.SubscriberCount() != 0 {
		t.Errorf("slow consumer 격리 실패: count=%d", gs.SubscriberCount())
	}
}

func TestTickToProto(t *testing.T) {
	in := &Tick{
		MarketID: 0xDEADBEEF,
		Symbol:   "USDKRW",
		SeqNum:   42,
		Mask:     0xFF,
		Type:     1,
		Flag:     2,
		Body:     []byte("payload"),
		Received: time.Unix(1700000000, 123456789),
	}
	out := tickToProto(in)
	if out.GetMarketId() != in.MarketID {
		t.Errorf("MarketId: %d", out.GetMarketId())
	}
	if out.GetSymbol() != in.Symbol {
		t.Errorf("Symbol: %q", out.GetSymbol())
	}
	if string(out.GetBody()) != "payload" {
		t.Errorf("Body: %q", out.GetBody())
	}
	if out.GetReceivedUnixNano() != in.Received.UnixNano() {
		t.Errorf("ReceivedUnixNano: %d", out.GetReceivedUnixNano())
	}
}

// 빈 GRPCServer 의 SubscribersSnapshot — 4개 슬라이스 모두 nil 아닌 []
// (JSON null 회피).
func TestSubscribersSnapshot_EmptyServer(t *testing.T) {
	g := NewGRPCServer(quietLogger(), 1024)
	snap := g.SubscribersSnapshot()
	if snap.Tick == nil || snap.Quote == nil || snap.Bar == nil || snap.CustomerQ == nil {
		t.Errorf("빈 slice 가 nil — null 직렬화 위험: %+v", snap)
	}
	if len(snap.Tick) != 0 || len(snap.Quote) != 0 || len(snap.Bar) != 0 || len(snap.CustomerQ) != 0 {
		t.Errorf("빈 서버인데 len 비-0: %+v", snap)
	}
}

// 1 tick + 1 quote subscriber 등록 후 Snapshot.
// gRPC Serve 없이 map 직접 채움 — Snapshot 의 read-only 검증에 집중.
func TestSubscribersSnapshot_Populated(t *testing.T) {
	g := NewGRPCServer(quietLogger(), 256)
	g.mu.Lock()
	g.subscribers[101] = &subscriber{
		id:      101,
		symbols: map[string]struct{}{"USDKRW": {}},
		out:     make(chan *wtgpb.Tick, 256),
		srvID:   "edge-A",
	}
	g.mu.Unlock()
	g.qmu.Lock()
	g.quoteSubscribers[202] = &quoteSubscriber{
		id:       202,
		profiles: map[string]struct{}{"WEB.BRANCH.VIP": {}},
		pairs:    map[string]struct{}{"USD/KRW": {}},
		out:      make(chan *wtgpb.CustomerQuote, 256),
		srvID:    "edge-B",
	}
	g.qmu.Unlock()

	snap := g.SubscribersSnapshot()
	if len(snap.Tick) != 1 || snap.Tick[0].ID != 101 || snap.Tick[0].SrvID != "edge-A" {
		t.Errorf("Tick subscriber 미반영: %+v", snap.Tick)
	}
	if len(snap.Quote) != 1 || snap.Quote[0].ID != 202 ||
		len(snap.Quote[0].Profiles) != 1 || snap.Quote[0].Profiles[0] != "WEB.BRANCH.VIP" {
		t.Errorf("Quote subscriber 필터 미반영: %+v", snap.Quote)
	}
	if snap.Quote[0].QueueCap != 256 {
		t.Errorf("QueueCap = %d, want 256", snap.Quote[0].QueueCap)
	}
}
