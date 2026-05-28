package price

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// fakeSubscriber 는 server.go 의 Subscriber 인터페이스를 만족시키는 mock.
type fakeSubscriber struct {
	ch chan *mymq.Unsolicited
}

func (f *fakeSubscriber) Subscribe() <-chan *mymq.Unsolicited { return f.ch }

func mkUnsol(fn mymq.Func, exch string, body []byte) *mymq.Unsolicited {
	var prefix mymq.BroadcastHeader
	copy(prefix.Exchange[:], exch)
	prefix.Function = uint8(fn)
	u := &mymq.Unsolicited{
		Prefix: &prefix,
		Body:   body,
	}
	u.Header.Func = fn
	return u
}

func newTestServer(consumer TickConsumer) (*Server, *fakeSubscriber) {
	cfg := DefaultConfig()
	cfg.ExchangeName = "PRICE"
	cfg.StatsInterval = 0 // 통계 루프 비활성
	// BestConsumer 는 v1 envelope body + Source 가 채워진 raw 를 기대.
	// server_test 는 opaque body 로 hot path 만 검증하므로 비활성.
	// BestConsumer 단독 검증은 best_test.go.
	cfg.BestEnabled = false
	srv := NewServer(cfg, nil)
	if consumer != nil {
		srv.AddConsumer(consumer)
	}
	sub := &fakeSubscriber{ch: make(chan *mymq.Unsolicited, 16)}
	return srv, sub
}

func TestServerHandleMatchedExchange(t *testing.T) {
	var got atomic.Int32
	srv, sub := newTestServer(TickConsumerFunc(func(t *Tick) { got.Add(1) }))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	tick := &Tick{Symbol: "USDKRW", SeqNum: 1, Body: []byte(`{"sym":"USDKRW","bid":1.0,"ask":1.1,"src":"T"}`)}
	body := EncodePushData(tick)
	sub.ch <- mkUnsol(mymq.FCCast, "PRICE", body)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() != 1 {
		t.Errorf("consumer 호출 수: %d, want 1", got.Load())
	}
	if srv.Stats().Matched != 1 {
		t.Errorf("Matched: %d", srv.Stats().Matched)
	}
	if srv.conflation.Latest("USDKRW") == nil {
		t.Error("Latest USDKRW nil")
	}
}

func TestServerFiltersWrongExchange(t *testing.T) {
	var got atomic.Int32
	srv, sub := newTestServer(TickConsumerFunc(func(t *Tick) { got.Add(1) }))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	body := EncodePushData(&Tick{Symbol: "USDKRW"})
	sub.ch <- mkUnsol(mymq.FCCast, "ORDER", body) // 다른 exchange

	time.Sleep(100 * time.Millisecond)

	if got.Load() != 0 {
		t.Errorf("필터되어야 하는데 consumer 호출됨: %d", got.Load())
	}
	if srv.Stats().Matched != 0 {
		t.Errorf("Matched: %d, want 0", srv.Stats().Matched)
	}
	// 그래도 Recv 카운트는 올라가야 함.
	if srv.Stats().Received != 1 {
		t.Errorf("Received: %d", srv.Stats().Received)
	}
}

func TestServerIgnoresNonCastFuncs(t *testing.T) {
	srv, sub := newTestServer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	body := EncodePushData(&Tick{Symbol: "USDKRW"})
	sub.ch <- mkUnsol(mymq.FCTran, "PRICE", body) // FC_TRAN — 무시

	time.Sleep(100 * time.Millisecond)
	if srv.Stats().Matched != 0 {
		t.Errorf("Matched: %d, want 0 (FC_TRAN 무시)", srv.Stats().Matched)
	}
}

func TestServerDecodeFailureCounted(t *testing.T) {
	srv, sub := newTestServer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	// 너무 짧은 본문 → 디코딩 실패.
	sub.ch <- mkUnsol(mymq.FCCast, "PRICE", []byte("short"))

	time.Sleep(100 * time.Millisecond)
	if srv.Stats().Dropped != 1 {
		t.Errorf("Dropped: %d, want 1", srv.Stats().Dropped)
	}
}

func TestServerSubscribeLoopStopsOnChannelClose(t *testing.T) {
	srv, sub := newTestServer(nil)

	done := make(chan struct{})
	go func() {
		srv.subscribeLoop(context.Background(), sub)
		close(done)
	}()
	close(sub.ch)

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Subscribe 채널 close 시 subscribeLoop 가 안 끝남")
	}
}

func TestServerMultipleConsumers(t *testing.T) {
	var c1, c2 atomic.Int32
	srv, sub := newTestServer(TickConsumerFunc(func(t *Tick) { c1.Add(1) }))
	srv.AddConsumer(TickConsumerFunc(func(t *Tick) { c2.Add(1) }))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	body := EncodePushData(&Tick{
		Symbol: "USDKRW",
		Body:   []byte(`{"sym":"USDKRW","bid":1.0,"ask":1.1,"src":"T"}`),
	})
	sub.ch <- mkUnsol(mymq.FCCast, "PRICE", body)

	time.Sleep(100 * time.Millisecond)
	if c1.Load() != 1 || c2.Load() != 1 {
		t.Errorf("두 consumer 모두 호출되어야 함: c1=%d c2=%d", c1.Load(), c2.Load())
	}
}

func TestServerEmptyExchangeFilterPassesAll(t *testing.T) {
	// ExchangeName 이 "" 이면 모든 exchange 통과.
	cfg := DefaultConfig()
	cfg.ExchangeName = ""
	cfg.StatsInterval = 0
	cfg.BestEnabled = false // opaque body 로 raw fan-out 검증
	srv := NewServer(cfg, nil)
	var got atomic.Int32
	srv.AddConsumer(TickConsumerFunc(func(t *Tick) { got.Add(1) }))

	sub := &fakeSubscriber{ch: make(chan *mymq.Unsolicited, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.subscribeLoop(ctx, sub)

	body := EncodePushData(&Tick{
		Symbol: "X",
		Body:   []byte(`{"sym":"X","bid":1.0,"ask":1.1,"src":"T"}`),
	})
	sub.ch <- mkUnsol(mymq.FCCast, "ANY_EXCH", body)

	time.Sleep(100 * time.Millisecond)
	if got.Load() != 1 {
		t.Errorf("ExchangeName 빈 값일 때 모두 통과: got=%d", got.Load())
	}
}
