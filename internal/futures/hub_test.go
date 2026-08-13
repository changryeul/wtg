package futures

import "testing"

func drain(s *Subscriber) []string {
	var out []string
	for {
		select {
		case p := <-s.Out():
			out = append(out, string(p))
		default:
			return out
		}
	}
}

// 종목 구독 필터 — 구독한 종목만 받고 미구독은 안 받는다 (구독형 핵심).
func TestHub_SymbolSubscription(t *testing.T) {
	h := NewHub()
	a := NewSubscriber("a", 16) // 101V6000 만 구독
	b := NewSubscriber("b", 16) // all 모드 (구독 안 함)
	a.Subscribe([]string{"101V6000"})
	h.Add(a)
	h.Add(b)

	// 101V6000 틱 → a(구독) + b(all) 둘 다
	if sent, _ := h.BroadcastBySymbol("101V6000", []byte(`{"code":"101V6000"}`)); sent != 2 {
		t.Errorf("101V6000 sent=%d, want 2", sent)
	}
	// 다른 종목 105V3000 → b(all)만, a 는 미구독이라 skip
	if sent, _ := h.BroadcastBySymbol("105V3000", []byte(`{"code":"105V3000"}`)); sent != 1 {
		t.Errorf("105V3000 sent=%d, want 1 (a 미구독)", sent)
	}

	ga, gb := drain(a), drain(b)
	if len(ga) != 1 || ga[0] != `{"code":"101V6000"}` {
		t.Errorf("a 수신=%v, want [101V6000] 만", ga)
	}
	if len(gb) != 2 {
		t.Errorf("b(all) 수신 %d건, want 2", len(gb))
	}
}

// subscribe → unsubscribe → all 모드 복귀.
func TestSubscriber_SubUnsub(t *testing.T) {
	s := NewSubscriber("s", 8)
	if !s.Matches("X") {
		t.Error("초기 all 모드여야 함")
	}
	s.Subscribe([]string{"A", "B"})
	if s.Matches("X") || !s.Matches("A") {
		t.Error("filtered: A 매칭, X 비매칭이어야")
	}
	s.Unsubscribe([]string{"A", "B"})
	if !s.Matches("X") {
		t.Error("전부 unsub → all 모드 복귀여야")
	}
}

// slow consumer 격리 — 큐 포화 subscriber 는 drop, 나머지는 정상.
func TestHub_SlowConsumerIsolation(t *testing.T) {
	h := NewHub()
	slow := NewSubscriber("slow", 1) // 큐 1
	fast := NewSubscriber("fast", 64)
	h.Add(slow)
	h.Add(fast)

	// slow 큐(1)를 채우고 추가 송신은 drop 되어야; fast 는 계속 성공.
	var totalDrop int
	for i := 0; i < 5; i++ {
		_, d := h.BroadcastBySymbol("A", []byte(`{}`))
		totalDrop += d
	}
	if totalDrop == 0 {
		t.Error("slow consumer drop 이 있어야 (격리)")
	}
	if len(drain(fast)) != 5 {
		t.Errorf("fast 는 5건 다 받아야, got %d", len(drain(fast)))
	}
}
