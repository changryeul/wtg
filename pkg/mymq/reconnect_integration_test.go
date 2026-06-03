package mymq

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconnectOnBrokerDisconnect(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	var reconnectCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
			BackoffFactor:  2.0,
			OnReconnect: func(c *Client) {
				reconnectCount.Add(1)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if reconnectCount.Load() != 0 {
		t.Errorf("초기에는 reconnect 카운트 0 이어야 함")
	}

	// broker 측에서 활성 연결만 닫음 (listener 는 살아있어 새 연결 받을 수 있음).
	b.CloseClientConn()

	// supervisor 가 재연결할 시간 부여.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reconnectCount.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if reconnectCount.Load() < 1 {
		t.Fatalf("OnReconnect 콜백이 호출되지 않음 (count=%d)", reconnectCount.Load())
	}
}

func TestReconnectMaxAttemptsExhausts(t *testing.T) {
	b := newFakeBroker(t)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 30 * time.Millisecond,
			MaxBackoff:     30 * time.Millisecond,
			BackoffFactor:  1.1,
			MaxAttempts:    2, // 2번만 시도 후 종료
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// listener + 활성 연결 모두 종료 → 클라이언트가 재연결 못 함.
	b.Close()

	// supervisor 가 MaxAttempts 도달 후 subCh close 할 때까지 대기.
	timeout := time.After(3 * time.Second)
loop:
	for {
		select {
		case _, ok := <-c.Subscribe():
			if !ok {
				// subCh 가 닫힘 — supervisor 가 영구 종료 처리.
				break loop
			}
		case <-timeout:
			t.Fatal("MaxAttempts 도달 후 supervisor 가 종료 안 됨")
		}
	}

	// readErr 에 ErrReconnectExhausted 또는 그 부분 문자열이 있어야 함.
	err2 := c.lastReadErr()
	if err2 == nil {
		t.Error("lastReadErr 에 종료 사유 기록되어야 함")
	}
}

func TestReconnectOnDisconnectCallback(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	var disconnectCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 50 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
			BackoffFactor:  1.5,
			OnDisconnect: func(c *Client, err error) {
				disconnectCount.Add(1)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	b.CloseClientConn()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disconnectCount.Load() >= 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if disconnectCount.Load() < 1 {
		t.Errorf("OnDisconnect 콜백 미호출 (count=%d)", disconnectCount.Load())
	}
}

func TestReconnectSubscribeChannelPersists(t *testing.T) {
	// 재연결 후에도 동일 subCh 로 unsolicited 메시지를 받을 수 있어야 함.
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reconnected := make(chan struct{}, 1)
	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Queue: &QueueOptions{
			Name:  "test_q",
			Attr:  QtClient,
			Flags: QfUnsolMsg,
		},
		Reconnect: &ReconnectOptions{
			InitialBackoff: 30 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
			BackoffFactor:  2.0,
			OnReconnect: func(c *Client) {
				select {
				case reconnected <- struct{}{}:
				default:
				}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sub := c.Subscribe()

	// 1차 끊김 → 재연결.
	b.CloseClientConn()
	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("재연결 안 됨")
	}

	// 재연결 후 unsolicited 송신.
	frame, _ := EncodeFrame(&FrameInput{
		Func: FCCast,
		Subc: SubBroadcast,
		Dirf: DirPublish,
		Ckey: 0,
		Body: []byte("hello"),
	})
	if err := b.Push(frame); err != nil {
		t.Fatalf("재연결 후 Push: %v", err)
	}

	select {
	case msg, ok := <-sub:
		if !ok {
			t.Fatal("subCh 가 재연결 시 닫혀버림 — 영구 채널이어야 함")
		}
		if msg.Header.Func != FCCast {
			t.Errorf("Func: %d", msg.Header.Func)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("재연결 후 unsolicited 수신 안 됨")
	}
}

func TestSupervisorIgnoredWhenClosing(t *testing.T) {
	// Close() 후에는 supervisor 가 추가 재연결 시도를 하지 않아야 함.
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	var rcCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 30 * time.Millisecond,
			OnReconnect:    func(c *Client) { rcCount.Add(1) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = c.Close()
	time.Sleep(200 * time.Millisecond)

	if rcCount.Load() != 0 {
		t.Errorf("Close 후 재연결 발생: count=%d", rcCount.Load())
	}
}

// MetricsHook — disconnect/reconnect/inflight_aborted 모두 호출되는지.
func TestMetricsHook_DisconnectReconnect(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	var disc, recAttempt atomic.Int32
	var lastDur atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 30 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
			BackoffFactor:  1.5,
		},
		Metrics: MetricsHook{
			OnDisconnect: func(_ error) { disc.Add(1) },
			OnReconnect: func(attempts int, d time.Duration) {
				recAttempt.Store(int32(attempts))
				lastDur.Store(int64(d))
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	b.CloseClientConn()

	// disconnect + reconnect 둘 다 도달.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disc.Load() >= 1 && recAttempt.Load() >= 1 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if disc.Load() < 1 {
		t.Errorf("OnDisconnect 미호출")
	}
	if recAttempt.Load() < 1 {
		t.Errorf("OnReconnect 미호출 (attempts=%d)", recAttempt.Load())
	}
	if d := time.Duration(lastDur.Load()); d <= 0 {
		t.Errorf("OnReconnect duration=%v, want > 0", d)
	}
}

// failPending → MetricsHook.OnInflightAborted 호출.
func TestMetricsHook_InflightAborted(t *testing.T) {
	b := newFakeBroker(t)
	t.Cleanup(b.Close)
	host, port := b.hostPort()

	var aborted atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Open(ctx, host, port, Options{
		ApplName: "mci-test",
		Reconnect: &ReconnectOptions{
			InitialBackoff: 30 * time.Millisecond,
			MaxBackoff:     100 * time.Millisecond,
		},
		Metrics: MetricsHook{
			OnInflightAborted: func(n int) { aborted.Add(int32(n)) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 가짜 pending RPC 3개 등록 — fakeBroker 가 응답 안 보내므로 무한 대기 상태.
	for i := 0; i < 3; i++ {
		ch := make(chan *Reply, 1)
		c.pending.Store(uint64(i+1), ch)
	}

	// broker 끊김 → failPending → OnInflightAborted(3) 기대.
	b.CloseClientConn()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if aborted.Load() >= 3 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if got := aborted.Load(); got < 3 {
		t.Errorf("OnInflightAborted 누적=%d, want >= 3", got)
	}
}
