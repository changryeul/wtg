package price

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// Phase 4b — gRPC RegisterCustomer / SubscribeCustomerQuote 통합 검증.

// RegisterCustomer stream 으로 등록 → ack 수신 → CustomerRegistry count 증가.
// stream 종료 시 자동 unregister.
func TestGRPCServer_RegisterCustomer_RegisterAck(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.RegisterCustomer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := stream.Send(&wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_REGISTER,
		CustomerId: "VIP-7",
		ProfileKey: "WEB.BRANCH.VIP",
	}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !ack.GetOk() || ack.GetCustomerId() != "VIP-7" {
		t.Errorf("ack: %+v", ack)
	}

	// CustomerRegistry 에 반영됐는지.
	if gs.CustomerRegistry().Count() != 1 {
		t.Errorf("registry count = %d, want 1", gs.CustomerRegistry().Count())
	}

	// 두번째 등록.
	if err := stream.Send(&wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_REGISTER,
		CustomerId: "GOLD-3",
		ProfileKey: "WEB.HQ.STD",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if gs.CustomerRegistry().Count() != 2 {
		t.Errorf("registry count = %d, want 2", gs.CustomerRegistry().Count())
	}

	// stream 종료 → 자동 unregister.
	_ = stream.CloseSend()
	// server 측 cleanup 대기.
	waitFor(t, 500*time.Millisecond, func() bool {
		return gs.CustomerRegistry().Count() == 0
	}, "auto-unregister after stream close")
}

// 빈 customer_id / invalid profile_key 는 ok=false ack.
func TestGRPCServer_RegisterCustomer_ValidationErrors(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.RegisterCustomer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 빈 customer_id.
	if err := stream.Send(&wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_REGISTER,
		CustomerId: "",
		ProfileKey: "WEB.BRANCH.VIP",
	}); err != nil {
		t.Fatal(err)
	}
	ack, _ := stream.Recv()
	if ack.GetOk() {
		t.Errorf("empty cid: 가짜로 ok=true: %+v", ack)
	}

	// invalid profile key (token 개수 ≠ 3).
	if err := stream.Send(&wtgpb.CustomerRegistration{
		Op:         wtgpb.CustomerRegistration_OP_REGISTER,
		CustomerId: "X",
		ProfileKey: "INVALID",
	}); err != nil {
		t.Fatal(err)
	}
	ack, _ = stream.Recv()
	if ack.GetOk() {
		t.Errorf("invalid profile: 가짜로 ok=true: %+v", ack)
	}

	if gs.CustomerRegistry().Count() != 0 {
		t.Errorf("실패 등록은 registry 무변동: count = %d", gs.CustomerRegistry().Count())
	}
}

// 명시적 UNREGISTER 도 registry 반영.
func TestGRPCServer_RegisterCustomer_ExplicitUnregister(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, _ := client.RegisterCustomer(ctx)

	_ = stream.Send(&wtgpb.CustomerRegistration{
		Op: wtgpb.CustomerRegistration_OP_REGISTER, CustomerId: "A", ProfileKey: "WEB.BRANCH.VIP",
	})
	_, _ = stream.Recv()

	_ = stream.Send(&wtgpb.CustomerRegistration{
		Op: wtgpb.CustomerRegistration_OP_UNREGISTER, CustomerId: "A",
	})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if gs.CustomerRegistry().Count() != 0 {
		t.Errorf("after unregister: count = %d, want 0", gs.CustomerRegistry().Count())
	}
}

// SubscribeCustomerQuote 으로 PublishCustomerQuote 결과 수신.
func TestGRPCServer_SubscribeCustomerQuote_ReceivesPublish(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.SubscribeCustomerQuote(ctx, &wtgpb.CustomerQuoteSubscribeRequest{
		SubscriberId: "edge-1",
		CustomerIds:  []string{"VIP-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// server 측 등록 완료 대기.
	waitFor(t, 500*time.Millisecond, func() bool {
		return gs.CustomerQuoteSubscriberCount() == 1
	}, "subscriber registration")

	prof := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}
	cq := pricing.CustomerQuote{
		Pair: "USD/KRW", Profile: prof, Tenor: pricing.TenorSpot,
		Bid: 1299.94, Ask: 1300.16, TS: time.Now(),
		RawBid: 1300.00, RawAsk: 1300.10, TableVersion: 9,
	}
	if err := gs.PublishCustomerQuote("VIP-7", prof, cq); err != nil {
		t.Fatal(err)
	}

	got, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCustomerId() != "VIP-7" {
		t.Errorf("customer_id = %q, want VIP-7", got.GetCustomerId())
	}
	if got.GetPair() != "USD/KRW" {
		t.Errorf("pair = %q", got.GetPair())
	}
	if !floatNear(got.GetBid(), 1299.94) {
		t.Errorf("bid = %v", got.GetBid())
	}
}

// customer_ids 필터 미일치는 수신하지 않음. cancel 로 stream 명시적 종료.
func TestGRPCServer_SubscribeCustomerQuote_FilterMismatch(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.SubscribeCustomerQuote(ctx, &wtgpb.CustomerQuoteSubscribeRequest{
		SubscriberId: "edge-1",
		CustomerIds:  []string{"ONLY-ME"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 500*time.Millisecond, func() bool {
		return gs.CustomerQuoteSubscriberCount() == 1
	}, "subscriber registration")

	prof := session.Profile{Tier: session.TierVIP}
	// 다른 customer 의 quote — 필터로 차단.
	_ = gs.PublishCustomerQuote("OTHER", prof, pricing.CustomerQuote{Pair: "USD/KRW", TS: time.Now()})

	// 잠시 대기 — 필터가 작동해야 Recv 가 block 상태 유지.
	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		done <- err
	}()
	select {
	case err := <-done:
		t.Errorf("filter mismatch: 차단 안 됨, Recv 가 받음 err=%v", err)
	case <-time.After(200 * time.Millisecond):
		// 정상 — 필터로 200ms 이상 block.
	}

	// 정리: cancel → Recv 가 cancelled 로 반환.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != io.EOF {
			// 정상 종료 패턴이면 OK.
			if err == nil {
				t.Errorf("Recv 가 nil 반환 — 가짜 수신")
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("cancel 후에도 Recv block")
	}
}

// 빈 customer_ids — 모두 수신 (subscriber_id 매칭 자동화는 P4c).
func TestGRPCServer_SubscribeCustomerQuote_EmptyFilter_ReceivesAll(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.SubscribeCustomerQuote(ctx, &wtgpb.CustomerQuoteSubscribeRequest{
		SubscriberId: "edge-1",
		CustomerIds:  nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 500*time.Millisecond, func() bool {
		return gs.CustomerQuoteSubscriberCount() == 1
	}, "subscriber registration")

	prof := session.Profile{Tier: session.TierVIP}
	_ = gs.PublishCustomerQuote("ANY-ID", prof, pricing.CustomerQuote{Pair: "USD/KRW", Bid: 100, Ask: 101, TS: time.Now()})

	got, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCustomerId() != "ANY-ID" {
		t.Errorf("empty filter 빈수신: %+v", got)
	}
}

// e2e — RegisterCustomer + PricingConsumer.OnTick + SubscribeCustomerQuote.
// 등록 → tick → customer quote stream 으로 도달.
func TestGRPCServer_E2E_RegisterTickReceive(t *testing.T) {
	gs := NewGRPCServer(quietLogger(), 16)
	client, cleanup := startTestGRPC(t, gs)
	defer cleanup()

	// PricingConsumer 가 gs 를 CustomerPub 으로 사용.
	pub := &fakeQuotePublisher{}
	pc := newCustomerFanoutConsumer(t, pub, gs, gs.CustomerRegistry(),
		[]session.Profile{{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}})

	// 1) Register customer.
	regCtx, regCancel := context.WithCancel(context.Background())
	defer regCancel()
	regStream, _ := client.RegisterCustomer(regCtx)
	_ = regStream.Send(&wtgpb.CustomerRegistration{
		Op: wtgpb.CustomerRegistration_OP_REGISTER, CustomerId: "VIP-7", ProfileKey: "WEB.BRANCH.VIP",
	})
	if _, err := regStream.Recv(); err != nil {
		t.Fatal(err)
	}

	// 2) Subscribe customer-quote.
	subCtx, subCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer subCancel()
	subStream, _ := client.SubscribeCustomerQuote(subCtx, &wtgpb.CustomerQuoteSubscribeRequest{
		SubscriberId: "edge-1", CustomerIds: []string{"VIP-7"},
	})
	waitFor(t, 500*time.Millisecond, func() bool {
		return gs.CustomerQuoteSubscriberCount() == 1
	}, "sub registered")

	// 3) OnTick → PricingConsumer 가 ApplyForCustomer + PublishCustomerQuote.
	pc.OnTick(mkEnvTick("USDKRW", 1300.00, 1300.10, time.Now()))

	// 4) stream 으로 수신.
	got, err := subStream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCustomerId() != "VIP-7" {
		t.Errorf("e2e cid = %q, want VIP-7", got.GetCustomerId())
	}
	if got.GetPair() != "USD/KRW" {
		t.Errorf("e2e pair = %q", got.GetPair())
	}
	// add 모드: HQ 0.02 + Site 0.05 + cust -0.01 = 0.06. bid 1300-0.06 = 1299.94
	if !floatNear(got.GetBid(), 1299.94) {
		t.Errorf("e2e bid = %v, want 1299.94", got.GetBid())
	}
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
