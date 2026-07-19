package tickhub

import (
	"testing"
)

// Hub — forwarder 의 tick fan-out 허브. dial-in 한 mci-price 구독자 전체에 broadcast,
// slow consumer 는 자동 격리. (mci-edge-price Registry 패턴을 forwarder 로 재사용)

func TestHub_BroadcastToAll(t *testing.T) {
	h := New(nil)
	a := NewSubscriber(SubscriberOptions{SendQueueSize: 4})
	b := NewSubscriber(SubscriberOptions{SendQueueSize: 4})
	h.Add(a)
	h.Add(b)
	if h.Count() != 2 {
		t.Fatalf("count = %d, want 2", h.Count())
	}

	sent, dropped := h.Broadcast([]byte("tick-1"))
	if sent != 2 || dropped != 0 {
		t.Fatalf("sent=%d dropped=%d, want 2/0", sent, dropped)
	}
	for _, s := range []*Subscriber{a, b} {
		select {
		case p := <-s.C():
			if string(p) != "tick-1" {
				t.Errorf("payload = %q", p)
			}
		default:
			t.Errorf("sub 가 tick 을 못 받음")
		}
	}
}

func TestHub_SlowConsumerEvicted(t *testing.T) {
	h := New(nil)
	slow := NewSubscriber(SubscriberOptions{SendQueueSize: 2}) // 안 읽음 → 곧 참
	h.Add(slow)

	// 큐(2) 를 넘겨 보냄 → 가득 차면 그 broadcast 에서 격리(Close+Remove).
	var lastDropped int
	for i := 0; i < 5; i++ {
		_, d := h.Broadcast([]byte("x"))
		lastDropped += d
	}
	if lastDropped == 0 {
		t.Errorf("slow consumer 격리 안 됨 (dropped=0)")
	}
	if h.Count() != 0 {
		t.Errorf("격리 후 count = %d, want 0", h.Count())
	}
	if !slow.IsClosed() {
		t.Errorf("slow sub 가 Close 안 됨")
	}
}

func TestHub_RemoveOnClose(t *testing.T) {
	h := New(nil)
	s := NewSubscriber(SubscriberOptions{SendQueueSize: 2})
	h.Add(s)
	s.Close() // onClose 콜백으로 Hub 에서 자동 제거되어야
	if h.Count() != 0 {
		t.Errorf("Close 후 count = %d, want 0", h.Count())
	}
	// idempotent
	s.Close()
}

func TestHub_AddRemove(t *testing.T) {
	h := New(nil)
	s := NewSubscriber(SubscriberOptions{})
	h.Add(s)
	if h.Count() != 1 {
		t.Fatalf("count = %d", h.Count())
	}
	h.Remove(s)
	if h.Count() != 0 {
		t.Errorf("Remove 후 count = %d", h.Count())
	}
	h.Remove(s) // idempotent
}
