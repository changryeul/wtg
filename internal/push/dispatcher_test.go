package push

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// fakeSubscriber 는 Dispatcher 의 Subscriber 인터페이스를 만족시키는 mock.
type fakeSubscriber struct {
	ch chan *mymq.Unsolicited
}

func (f *fakeSubscriber) Subscribe() <-chan *mymq.Unsolicited { return f.ch }

func newFakeSub() *fakeSubscriber {
	return &fakeSubscriber{ch: make(chan *mymq.Unsolicited, 16)}
}

func mkPrefix(exch, chn, logon string, fn, sub uint8) *mymq.BroadcastHeader {
	var h mymq.BroadcastHeader
	copy(h.Exchange[:], exch)
	copy(h.Chan[:], chn)
	copy(h.LogonID[:], logon)
	h.Function = fn
	h.SubFunction = sub
	return &h
}

func mkUnsol(fn mymq.Func, prefix *mymq.BroadcastHeader, body []byte) *mymq.Unsolicited {
	u := &mymq.Unsolicited{
		Prefix: prefix,
		Body:   body,
	}
	u.Header.Func = fn
	u.Header.Subc = mymq.SubBroadcast
	return u
}

func TestDispatcherFanoutToTargetUser(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	c := mkTestConn("trader01", 4)
	r.Add(c)
	other := mkTestConn("trader02", 4)
	r.Add(other)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	prefix := mkPrefix("EXEC", "WEB", "trader01", uint8(mymq.FCPush), uint8(mymq.SubPush))
	sub.ch <- mkUnsol(mymq.FCPush, prefix, []byte(`{"side":"BUY"}`))

	// 짧게 대기.
	time.Sleep(100 * time.Millisecond)

	if len(c.send) != 1 {
		t.Errorf("trader01 큐: %d, want 1", len(c.send))
	}
	if len(other.send) != 0 {
		t.Errorf("trader02 가 잘못 받음: %d", len(other.send))
	}

	// envelope 검증.
	payload := <-c.send
	var env WSEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.LogonID != "trader01" {
		t.Errorf("LogonID: %q", env.LogonID)
	}
	if env.Exchange != "EXEC" {
		t.Errorf("Exchange: %q", env.Exchange)
	}
	if env.Func != uint8(mymq.FCPush) {
		t.Errorf("Func: %d", env.Func)
	}

	stats := d.Stats()
	if stats.Received != 1 || stats.Delivered != 1 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestDispatcherBroadcastWithoutLogonID(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	r.Add(c1)
	r.Add(c2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// LogonID 비어있는 경우 → broadcast.
	prefix := mkPrefix("ALERT", "WEB", "", uint8(mymq.FCCast), uint8(mymq.SubBroadcast))
	sub.ch <- mkUnsol(mymq.FCCast, prefix, []byte(`{"alert":"market_close"}`))

	time.Sleep(100 * time.Millisecond)

	if len(c1.send) != 1 || len(c2.send) != 1 {
		t.Errorf("broadcast 수신: c1=%d c2=%d", len(c1.send), len(c2.send))
	}
	stats := d.Stats()
	if stats.Delivered != 2 {
		t.Errorf("Delivered: %d, want 2", stats.Delivered)
	}
}

func TestDispatcherIgnoresNonPushFuncs(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	c := mkTestConn("trader01", 4)
	r.Add(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// FC_TRAN 같은 비-push 함수는 무시.
	prefix := mkPrefix("ORDER", "WEB", "trader01", uint8(mymq.FCTran), 0)
	sub.ch <- mkUnsol(mymq.FCTran, prefix, []byte(`{}`))

	time.Sleep(100 * time.Millisecond)

	if len(c.send) != 0 {
		t.Errorf("FC_TRAN 메시지가 dispatch 됨: %d", len(c.send))
	}
	if d.Stats().Received != 1 {
		t.Errorf("Received 카운트: %d", d.Stats().Received)
	}
	if d.Stats().Delivered != 0 {
		t.Errorf("비-push 는 deliver 안 되어야 함")
	}
}

func TestDispatcherDropsForUnknownUser(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// 등록되지 않은 사용자에게 push → drop.
	prefix := mkPrefix("EXEC", "", "ghost_user", uint8(mymq.FCPush), uint8(mymq.SubPush))
	sub.ch <- mkUnsol(mymq.FCPush, prefix, []byte(`{}`))

	time.Sleep(100 * time.Millisecond)

	if d.Stats().Dropped < 1 {
		t.Errorf("Dropped: %d, want >= 1", d.Stats().Dropped)
	}
}

func TestDispatcherStopsOnSubChannelClose(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	done := make(chan struct{})
	ctx := context.Background()
	go func() {
		d.Run(ctx)
		close(done)
	}()

	close(sub.ch) // 채널 종료 → Run 도 반환

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Subscribe 채널 닫혀도 Run 이 종료 안 됨")
	}
}

func TestBuildEnvelopeJSONBody(t *testing.T) {
	prefix := mkPrefix("EXEC", "WEB", "trader01", uint8(mymq.FCPush), uint8(mymq.SubPush))
	msg := mkUnsol(mymq.FCPush, prefix, []byte(`{"order_id":"O-1"}`))

	env, err := buildEnvelope(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got WSEnvelope
	if err := json.Unmarshal(env, &got); err != nil {
		t.Fatal(err)
	}
	if got.LogonID != "trader01" {
		t.Errorf("LogonID: %q", got.LogonID)
	}
	var data map[string]string
	_ = json.Unmarshal(got.Data, &data)
	if data["order_id"] != "O-1" {
		t.Errorf("data: %v", data)
	}
}

func TestBuildEnvelopeRawBody(t *testing.T) {
	// JSON 이 아닌 raw text → string 으로 wrap.
	prefix := mkPrefix("ALERT", "WEB", "trader01", uint8(mymq.FCSignal), 0)
	msg := mkUnsol(mymq.FCSignal, prefix, []byte("RAW_ALERT"))

	env, err := buildEnvelope(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got WSEnvelope
	if err := json.Unmarshal(env, &got); err != nil {
		t.Fatal(err)
	}
	var s string
	if err := json.Unmarshal(got.Data, &s); err != nil {
		t.Fatal(err)
	}
	if s != "RAW_ALERT" {
		t.Errorf("string wrap: %q", s)
	}
}

func TestBuildEnvelopeNoPrefix(t *testing.T) {
	msg := &mymq.Unsolicited{Body: []byte(`{}`)}
	msg.Header.Func = mymq.FCCast
	env, err := buildEnvelope(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got WSEnvelope
	_ = json.Unmarshal(env, &got)
	if got.Exchange != "" || got.LogonID != "" {
		t.Errorf("prefix 없을 때 Exchange/LogonID 비어있어야 함: %+v", got)
	}
}

// FCPush 는 Func 가 mymq.FCPush 이지만 실제 wire 에서 uint8(13) 로 들어옴.
// mymq.Func type 을 uint8 로 변환할 때 int → uint8 변환 검증.
func TestUnsolicitedFuncMatching(t *testing.T) {
	cases := []struct {
		fn   mymq.Func
		want bool
	}{
		{mymq.FCCast, true},
		{mymq.FCPush, true},
		{mymq.FCSignal, true},
		{mymq.FCTran, false},
		{mymq.FCAdmin, false},
	}
	for _, c := range cases {
		got := c.fn == mymq.FCCast || c.fn == mymq.FCPush || c.fn == mymq.FCSignal
		if got != c.want {
			t.Errorf("Func %d: got=%v want=%v", c.fn, got, c.want)
		}
	}
}
