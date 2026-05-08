package admin

import (
	"sync"
	"testing"
	"time"
)

// Hub.Broadcast 가 모든 구독자 channel 에 fan-out 되는지.
func TestHubBroadcast(t *testing.T) {
	h := NewHub(nil)
	defer h.Close()

	subs := make([]*subscriber, 3)
	for i := range subs {
		s := &subscriber{
			id:    uint64(i + 1),
			out:   make(chan Event, 4),
			close: make(chan struct{}),
		}
		h.add(s)
		subs[i] = s
	}

	h.Broadcast("audit", map[string]any{"k": "v"})

	for i, s := range subs {
		select {
		case ev := <-s.out:
			if ev.Type != "audit" {
				t.Errorf("sub %d 받은 type=%q", i, ev.Type)
			}
			if ev.At <= 0 {
				t.Errorf("sub %d At=0", i)
			}
		case <-time.After(time.Second):
			t.Errorf("sub %d 메시지 미수신", i)
		}
	}
	if h.Count() != 3 {
		t.Errorf("Count=%d", h.Count())
	}
}

// Slow consumer (out 가득) → broadcast 가 강제 종료.
func TestHubSlowConsumerEvicted(t *testing.T) {
	h := NewHub(nil)
	defer h.Close()

	slow := &subscriber{
		id:    1,
		out:   make(chan Event, 1),
		close: make(chan struct{}),
	}
	h.add(slow)

	// 채널 가득 채움.
	slow.out <- Event{Type: "fill"}

	h.Broadcast("audit", "x") // 가득 → 끊김.

	// close 채널이 닫혔는지.
	select {
	case <-slow.close:
		// ok
	case <-time.After(time.Second):
		t.Error("slow subscriber close 채널이 닫히지 않음")
	}
	if h.Count() != 0 {
		t.Errorf("Count=%d, want 0", h.Count())
	}
}

func TestHubCloseStopsAll(t *testing.T) {
	h := NewHub(nil)
	subs := []*subscriber{
		{id: 1, out: make(chan Event, 1), close: make(chan struct{})},
		{id: 2, out: make(chan Event, 1), close: make(chan struct{})},
	}
	for _, s := range subs {
		h.add(s)
	}
	h.Close()
	for i, s := range subs {
		select {
		case <-s.close:
		case <-time.After(time.Second):
			t.Errorf("sub %d close 신호 미수신", i)
		}
	}
	// Broadcast 후엔 추가 안됨.
	h.Broadcast("x", nil) // no-op
}

// add 후 Close 되면 add 는 false 반환.
func TestHubAddAfterClose(t *testing.T) {
	h := NewHub(nil)
	h.Close()
	s := &subscriber{out: make(chan Event, 1), close: make(chan struct{})}
	if h.add(s) {
		t.Error("close 후 add 가 true")
	}
}

// 동시 broadcast / add / remove.
func TestHubConcurrent(t *testing.T) {
	h := NewHub(nil)
	defer h.Close()
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := &subscriber{
				id:    uint64(time.Now().UnixNano()),
				out:   make(chan Event, 32),
				close: make(chan struct{}),
			}
			if !h.add(s) {
				return
			}
			// drain.
			go func() {
				for {
					select {
					case <-s.out:
					case <-s.close:
						return
					}
				}
			}()
			time.Sleep(5 * time.Millisecond)
			h.removeAndClose(s)
		}()
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Broadcast("audit", i)
		}(i)
	}
	wg.Wait()
}
