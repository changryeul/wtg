package mymq

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 통합 mock 테스트 — fakeBroker 를 띄우고 Client 의 핸드셰이크/Call/Subscribe 등을 검증.

func TestClientOpenAndDeclareSession(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Channel:  ChannelWeb,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	si := c.SessionInfo()
	if si.ConnectionID != b.sessionResp.ConnectionID {
		t.Errorf("ConnectionID: got %d, want %d", si.ConnectionID, b.sessionResp.ConnectionID)
	}
	if c.ConnectInfo() != nil {
		t.Errorf("DECLARE_SESSION 모드에선 ConnectInfo() == nil 이어야 함")
	}
}

func TestClientOpenAndConnect(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	b.connectResp.QueueName = "mci_push"
	b.connectResp.ConnectionID = 99999
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: ApplMciPush,
		Channel:  ChannelWeb,
		Queue: &QueueOptions{
			Name:  QueueMciPush,
			Attr:  QtClient,
			Flags: QfUnsolMsg | QfUnsolHdr,
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	ci := c.ConnectInfo()
	if ci == nil {
		t.Fatal("CONNECT 모드에선 ConnectInfo() != nil")
	}
	if ci.QueueName != "mci_push" {
		t.Errorf("QueueName: %q", ci.QueueName)
	}
	if ci.ConnectionID != 99999 {
		t.Errorf("ConnectionID: %d", ci.ConnectionID)
	}
}

func TestClientCallEchoesCkey(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{ApplName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 백그라운드에서 fakeBroker 가 받은 frame 을 ckey echo 로 응답.
	go func() {
		for df := range b.received {
			reply, _ := EncodeFrame(&FrameInput{
				Func: df.Header.Func,
				Subc: SubTranMsg,
				Dirf: DirOrigin,
				Ckey: df.Header.Ckey, // ckey echo (option C 핵심)
				Body: []byte(`{"status":"ok"}`),
			})
			_ = b.Push(reply)
		}
	}()

	callCtx, cancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel2()
	r, err := c.Call(callCtx, &FrameInput{
		Func: FCTran,
		Subc: SubTranMsg,
		Dirf: DirForward,
		Keyc: KeySend,
		Xchg: "ORDER",
		Rkey: "NEW",
		Body: []byte(`{"symbol":"USDKRW"}`),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !bytes.Equal(r.Body, []byte(`{"status":"ok"}`)) {
		t.Errorf("body: %q", r.Body)
	}
	if r.Errn != 0 {
		t.Errorf("Errn: %d", r.Errn)
	}
}

// Client.Call 의 FrameInput.TraceID 가 wire frame 의 trcid 영역에 정확히
// 기록되고, broker 가 echo back 한 reply 의 trcid 도 보존되는지.
func TestClientCallPropagatesTraceID(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{ApplName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	rid := "deadbeef00112233"
	want := TraceIDFromHex(rid)

	// fakeBroker 가 받은 trace_id 캡처 + reply 에 동일 trace_id echo.
	gotChan := make(chan [TraceIDSize]byte, 1)
	go func() {
		for df := range b.received {
			select {
			case gotChan <- df.Header.TraceID:
			default:
			}
			reply, _ := EncodeFrame(&FrameInput{
				Func:    df.Header.Func,
				Subc:    SubTranMsg,
				Dirf:    DirOrigin,
				Ckey:    df.Header.Ckey,
				TraceID: df.Header.TraceID, // broker 가 trcid echo back
				Body:    []byte(`{"status":"ok"}`),
			})
			_ = b.Push(reply)
		}
	}()

	callCtx, cancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel2()
	r, err := c.Call(callCtx, &FrameInput{
		Func:    FCTran,
		Subc:    SubTranMsg,
		Dirf:    DirForward,
		Keyc:    KeySend,
		Xchg:    "ECHOSVC",
		Rkey:    "PING",
		TraceID: want,
		Body:    []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	select {
	case got := <-gotChan:
		if got != want {
			t.Errorf("broker received trace_id = %x, want %x", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("broker frame 수신 timeout")
	}

	if r.Header.TraceID != want {
		t.Errorf("reply trace_id = %x, want %x (broker echo back)", r.Header.TraceID, want)
	}
	if got := TraceIDToHex(r.Header.TraceID); got != rid {
		t.Errorf("reply hex = %q, want %q", got, rid)
	}
}

func TestClientCallContextCancel(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{ApplName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// fakeBroker 는 응답하지 않음 → ctx deadline 에 걸려야 함.
	callCtx, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, err = c.Call(callCtx, &FrameInput{
		Func: FCTran,
		Subc: SubTranMsg,
		Dirf: DirForward,
		Keyc: KeySend,
		Rkey: "NEW",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("DeadlineExceeded 기대, got %v", err)
	}
}

func TestClientCloseFailsPending(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{ApplName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}

	// pending 한 개 등록 후 Close → ErrClientClosed 또는 ErrBroker 받아야 함.
	var wg sync.WaitGroup
	wg.Add(1)
	var callErr error
	go func() {
		defer wg.Done()
		callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, callErr = c.Call(callCtx, &FrameInput{
			Func: FCTran,
			Subc: SubTranMsg,
			Dirf: DirForward,
			Keyc: KeySend,
			Rkey: "NEW",
		})
	}()

	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	wg.Wait()

	if callErr == nil {
		t.Error("Close 후 Call 은 에러 반환해야 함")
	}
}

func TestClientSubscribeReceivesUnsolicited(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Queue: &QueueOptions{
			Name:  "test_q",
			Attr:  QtClient,
			Flags: QfUnsolMsg | QfUnsolHdr,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// broker 가 unsolicited 메시지 push (ckey=0 으로 unsol 식별).
	var prefix BroadcastHeader
	copy(prefix.Exchange[:], "PRICE")
	copy(prefix.LogonID[:], "trader01")
	prefix.Function = uint8(FCCast)
	prefix.SubFunction = uint8(SubBroadcast)

	prefixBytes := make([]byte, BroadcastPrefixSize)
	EncodeBroadcastHeader(prefixBytes, &prefix)
	body := append(prefixBytes, []byte(`{"tick":1}`)...)

	frame, _ := EncodeFrame(&FrameInput{
		Func: FCCast,
		Subc: SubBroadcast,
		Dirf: DirPublish,
		Ckey: 0,
		Body: body,
	})
	if err := b.Push(frame); err != nil {
		t.Fatalf("Push: %v", err)
	}

	select {
	case msg, ok := <-c.Subscribe():
		if !ok {
			t.Fatal("subCh 가 미리 닫힘")
		}
		if msg.Prefix == nil {
			t.Fatal("Prefix 자동 파싱 실패")
		}
		if msg.Prefix.ExchangeString() != "PRICE" {
			t.Errorf("Prefix.Exchange: %q", msg.Prefix.ExchangeString())
		}
		if !strings.Contains(string(msg.Body), "tick") {
			t.Errorf("Body: %q", msg.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unsolicited 메시지 수신 안 됨")
	}
}

func TestClientHeartbeatAutoSend(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	b.sessionResp.Heartbeat = 1 // 1초 간격 자동 heartbeat 활성
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Open(ctx, host, port, Options{ApplName: "mci-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 잠시 기다리면 heartbeat 가 fakeBroker 에 도달해서 (length=4 frame) connLoop 이
	// continue 한다. fakeBroker 가 heartbeat 를 셀 수 있는 hook 은 없지만, lastSent
	// 가 갱신되는지로 간접 검증.
	time.Sleep(1500 * time.Millisecond)

	if c.lastSent.Load() == 0 {
		t.Error("자동 heartbeat 가 송신되지 않음 (lastSent == 0)")
	}
}

func TestClientCallReconnectingError(t *testing.T) {
	// reconnecting flag 가 켜져있을 때 Call 은 즉시 ErrReconnecting 반환.
	c := &Client{}
	c.reconnecting.Store(true)
	_, err := c.Call(context.Background(), &FrameInput{Rkey: "X"})
	if !errors.Is(err, ErrReconnecting) {
		t.Errorf("ErrReconnecting 기대, got %v", err)
	}
}

func TestClientCallClosedError(t *testing.T) {
	c := &Client{}
	c.closed.Store(true)
	_, err := c.Call(context.Background(), &FrameInput{Rkey: "X"})
	if !errors.Is(err, ErrClientClosed) {
		t.Errorf("ErrClientClosed 기대, got %v", err)
	}
}

func TestClientApplyDefaultsChannel(t *testing.T) {
	c := &Client{chanCode: ChannelWeb.Bytes()}
	in := &FrameInput{Rkey: "X"}
	c.applyDefaults(in)
	expected := ChannelWeb.Bytes()
	if in.Chan != expected {
		t.Errorf("자동 채널 코드 첨부 실패: got %v, want %v", in.Chan, expected)
	}

	// 명시적 chan 이 있으면 덮어쓰지 않음.
	manual := [4]byte{'A', 'D', 'M', ' '}
	in2 := &FrameInput{Chan: manual, Rkey: "X"}
	c.applyDefaults(in2)
	if in2.Chan != manual {
		t.Errorf("명시 chan 을 덮어씀: got %v", in2.Chan)
	}
}

// TestClientApplyDefaultsBroadcastNoNavi — FCCast/FCPush/FCSignal 의 경우
// applyDefaults 가 navi 자동채움을 하면 안 된다. broker 의 packet_proc 가
// nvia==0 일 때만 publish_packet (broadcast fan-out) 으로 분기하기 때문이다.
// nvia!=0 이면 message_packet_transfer (transaction) 로 잘못 분기 후
// "Lost reply message" 로 drop 됨. 또한 Dirf 가 0(IOCTL) 이면 DirPublish 로
// 자동 채워야 broker 가 PUBLISH 방향으로 해석.
func TestClientApplyDefaultsBroadcastNoNavi(t *testing.T) {
	c := &Client{chanCode: ChannelWeb.Bytes(), whoamiSc: 12345}

	for _, fn := range []Func{FCCast, FCPush, FCSignal} {
		// Xchg 가 채워진 broadcast — transaction 패턴이면 navi 자동채움이
		// 트리거되는 조건. 하지만 broadcast 는 navi 가 비어 있어야 한다.
		in := &FrameInput{Func: fn, Xchg: "PRICE"}
		c.applyDefaults(in)
		if len(in.Navis) != 0 {
			t.Errorf("func=%d: broadcast 인데 navi 자동채움됨 (nvia=%d) — broker 가 transaction 으로 오인하여 drop 한다",
				fn, len(in.Navis))
		}
		if in.Dirf != DirPublish {
			t.Errorf("func=%d: Dirf=%d (DirPublish=%d 기대) — broker 가 PUBLISH 로 해석 못 함",
				fn, in.Dirf, DirPublish)
		}
	}

	// 호출자가 Dirf 를 명시적으로 설정한 경우 덮어쓰지 않는다.
	in := &FrameInput{Func: FCCast, Xchg: "PRICE", Dirf: DirForward}
	c.applyDefaults(in)
	if in.Dirf != DirForward {
		t.Errorf("명시 Dirf 를 덮어씀: got %d, want %d", in.Dirf, DirForward)
	}

	// 일반 transaction (FCNotify 등) 은 기존대로 navi 자동채움이 동작해야 한다.
	in2 := &FrameInput{Func: FCNotify, Xchg: "ECHO", Rkey: "PING"}
	c.applyDefaults(in2)
	if len(in2.Navis) != 2 {
		t.Errorf("transaction navi 자동채움 회귀: got %d navi, want 2", len(in2.Navis))
	}
}
