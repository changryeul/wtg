package price

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pricesvc "github.com/winwaysystems/wtg/internal/price"
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func internalPriceGRPCServer(t *testing.T) *pricesvc.GRPCServer {
	t.Helper()
	return pricesvc.NewGRPCServer(quietLogger(), 16)
}

func newProfileForTest() session.Profile {
	return session.Profile{
		Channel: session.ChannelWeb,
		Site:    session.SiteBranch,
		Tier:    session.TierVIP,
	}
}

func newCustomerQuoteForTest() pricing.CustomerQuote {
	return pricing.CustomerQuote{
		Pair:         "USD/KRW",
		Profile:      newProfileForTest(),
		Tenor:        pricing.TenorSpot,
		Bid:          1299.94,
		Ask:          1300.16,
		TS:           time.Now(),
		RawBid:       1300.00,
		RawAsk:       1300.10,
		TableVersion: 9,
	}
}

// ─── upstream mock server ──────────────────────────────────────────────────

// mockPriceServer — 최소 PriceServiceServer 모의. RegisterCustomer 의 등록
// 이벤트를 capture, SubscribeCustomerQuote 에 publish 한 메시지를 client 로 전송.
type mockPriceServer struct {
	wtgpb.UnimplementedPriceServiceServer

	mu       sync.Mutex
	regs     []*wtgpb.CustomerRegistration
	cqSubs   []wtgpb.PriceService_SubscribeCustomerQuoteServer
	regClose chan struct{} // 테스트가 stream 강제 종료에 쓰는 신호
}

func newMockPriceServer() *mockPriceServer {
	return &mockPriceServer{regClose: make(chan struct{})}
}

func (m *mockPriceServer) RegisterCustomer(stream wtgpb.PriceService_RegisterCustomerServer) error {
	for {
		select {
		case <-m.regClose:
			return io.EOF
		default:
		}
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.regs = append(m.regs, req)
		m.mu.Unlock()
		_ = stream.Send(&wtgpb.CustomerAck{CustomerId: req.GetCustomerId(), Ok: true})
	}
}

func (m *mockPriceServer) SubscribeCustomerQuote(req *wtgpb.CustomerQuoteSubscribeRequest, stream wtgpb.PriceService_SubscribeCustomerQuoteServer) error {
	m.mu.Lock()
	m.cqSubs = append(m.cqSubs, stream)
	m.mu.Unlock()
	<-stream.Context().Done()
	return nil
}

func (m *mockPriceServer) registrations() []*wtgpb.CustomerRegistration {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*wtgpb.CustomerRegistration, len(m.regs))
	copy(out, m.regs)
	return out
}

// startMockPrice — in-memory listener 에 mock 서버 가동.
func startMockPrice(t *testing.T) (*mockPriceServer, *grpc.ClientConn, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	mock := newMockPriceServer()
	wtgpb.RegisterPriceServiceServer(srv, mock)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		t.Fatal(err)
	}
	cleanup := func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
	return mock, conn, cleanup
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ─── customerRegManager 단위 테스트 ─────────────────────────────────────────

func TestCustomerRegManager_RegisterUnregisterDelivered(t *testing.T) {
	mock, conn, cleanup := startMockPrice(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := newCustomerRegManager(conn, "edge-1", quietLogger())
	mgr.Start(ctx)

	mgr.Register("VIP-7", "WEB.BRANCH.VIP")
	mgr.Register("GOLD-3", "WEB.HQ.STD")

	// upstream 도달 대기.
	waitFor(t, time.Second, func() bool {
		return len(mock.registrations()) >= 2
	}, "2 registrations delivered")

	regs := mock.registrations()
	got := map[string]string{}
	for _, r := range regs {
		if r.GetOp() == wtgpb.CustomerRegistration_OP_REGISTER {
			got[r.GetCustomerId()] = r.GetProfileKey()
		}
	}
	if got["VIP-7"] != "WEB.BRANCH.VIP" || got["GOLD-3"] != "WEB.HQ.STD" {
		t.Errorf("registrations: %+v", got)
	}

	// Unregister.
	mgr.Unregister("VIP-7")
	waitFor(t, time.Second, func() bool {
		for _, r := range mock.registrations() {
			if r.GetOp() == wtgpb.CustomerRegistration_OP_UNREGISTER && r.GetCustomerId() == "VIP-7" {
				return true
			}
		}
		return false
	}, "unregister delivered")

	st := mgr.Stats()
	if st.AckOK < 2 {
		t.Errorf("ack ok = %d, want ≥2", st.AckOK)
	}
}

// 빈 customerID / profileKey 호출은 enqueue X.
func TestCustomerRegManager_EmptyArgsIgnored(t *testing.T) {
	_, conn, cleanup := startMockPrice(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := newCustomerRegManager(conn, "edge-1", quietLogger())
	mgr.Start(ctx)

	mgr.Register("", "WEB.BRANCH.VIP")
	mgr.Register("X", "")
	mgr.Unregister("")

	time.Sleep(150 * time.Millisecond)

	st := mgr.Stats()
	if st.Registered+st.Unregistered != 0 {
		t.Errorf("빈 인자 호출은 무시되어야: %+v", st)
	}
}

// Registry.SendByCustomerID — customerID 매칭 sub 에게만 송신.
func TestRegistry_SendByCustomerID_Filtering(t *testing.T) {
	r := NewRegistry(quietLogger())

	// VIP-7 customer 의 sub + 다른 customer 의 sub + customer 없는 sub.
	sub1 := &Subscriber{id: 1, customerID: "VIP-7", send: make(chan []byte, 4)}
	sub2 := &Subscriber{id: 2, customerID: "OTHER", send: make(chan []byte, 4)}
	sub3 := &Subscriber{id: 3, customerID: "", send: make(chan []byte, 4)}
	r.Add(sub1)
	r.Add(sub2)
	r.Add(sub3)

	sent, dropped := r.SendByCustomerID("VIP-7", "USD/KRW", []byte("payload"))
	if sent != 1 || dropped != 0 {
		t.Errorf("sent=%d dropped=%d, want 1/0", sent, dropped)
	}
	// 큐에 정확히 sub1 만 enqueue 됐는지.
	if len(sub1.send) != 1 || len(sub2.send) != 0 || len(sub3.send) != 0 {
		t.Errorf("queue counts: sub1=%d sub2=%d sub3=%d", len(sub1.send), len(sub2.send), len(sub3.send))
	}
}

// 빈 customerID 인자는 noop.
func TestRegistry_SendByCustomerID_EmptyIsNoop(t *testing.T) {
	r := NewRegistry(quietLogger())
	sub := &Subscriber{id: 1, customerID: "X", send: make(chan []byte, 4)}
	r.Add(sub)
	sent, dropped := r.SendByCustomerID("", "USD/KRW", []byte("x"))
	if sent != 0 || dropped != 0 || len(sub.send) != 0 {
		t.Errorf("empty customerID 호출이 발송됨: sent=%d dropped=%d q=%d", sent, dropped, len(sub.send))
	}
}

// e2e — 실 mci-price GRPCServer 와 edge customerRegManager 가 통신해서 등록
// 이벤트가 CustomerRegistry 에 반영되고, PublishCustomerQuote 결과가 edge 의
// SubscribeCustomerQuote stream 으로 도착해 SendByCustomerID 로 ws 큐에 enqueue
// 되는 전체 사슬 검증.
func TestEdgePrice_E2E_CustomerStream(t *testing.T) {
	// 실 mci-price GRPCServer.
	priceSrv := internalPriceGRPCServer(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	wtgpb.RegisterPriceServiceServer(srv, priceSrv)
	go func() { _ = srv.Serve(lis) }()
	defer srv.GracefulStop()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1) customerRegManager 가동.
	mgr := newCustomerRegManager(conn, "edge-e2e", quietLogger())
	mgr.Start(ctx)
	mgr.Register("VIP-7", "WEB.BRANCH.VIP")

	// 2) 등록이 mci-price CustomerRegistry 에 도달.
	waitFor(t, time.Second, func() bool {
		return priceSrv.CustomerRegistry().Count() == 1
	}, "register reached mci-price registry")

	// 3) ws subscriber (customerID=VIP-7) 등록.
	registry := NewRegistry(quietLogger())
	sub := &Subscriber{id: 99, customerID: "VIP-7", send: make(chan []byte, 4)}
	registry.Add(sub)

	// 4) edge consumer (SubscribeCustomerQuote → Registry.SendByCustomerID).
	srvObj := &Server{
		cfg:      Config{SubscriberID: "edge-e2e"},
		logger:   quietLogger(),
		registry: registry,
		upstream: conn,
	}
	go srvObj.subscribeCustomerQuoteLoop(ctx)

	// SubscribeCustomerQuote 등록 도달 대기.
	waitFor(t, time.Second, func() bool {
		return priceSrv.CustomerQuoteSubscriberCount() == 1
	}, "subscribe customer-quote stream registered")

	// 5) PublishCustomerQuote.
	if err := priceSrv.PublishCustomerQuote("VIP-7",
		newProfileForTest(),
		newCustomerQuoteForTest()); err != nil {
		t.Fatal(err)
	}

	// 6) Registry 큐에 도달 (sub.send).
	select {
	case payload := <-sub.send:
		if len(payload) == 0 {
			t.Errorf("payload empty")
		}
		// JSON 안에 customer_id / VIP-7 포함 확인.
		if !contains(payload, []byte(`"customer_id":"VIP-7"`)) {
			t.Errorf("payload missing customer_id tag: %s", string(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber 큐에 customer-quote 도달 안 함")
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

// waitFor — predicate 이 true 가 될 때까지 polling.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor: timeout — %s", what)
}
