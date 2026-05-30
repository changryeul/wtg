package admin

import (
	"sync"
	"testing"
	"time"
)

func TestAuditRingPushList(t *testing.T) {
	r := NewAuditRing(5)
	if r.Len() != 0 {
		t.Errorf("초기 Len=%d", r.Len())
	}

	for i := 1; i <= 3; i++ {
		r.Push(AuditEntry{Action: "A", Usid: "u", At: time.Unix(int64(i), 0)})
	}
	if r.Len() != 3 {
		t.Errorf("Len=%d, want 3", r.Len())
	}

	// 시간 역순 (최신 → 오래된).
	out := r.List(0)
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].At.Unix() != 3 || out[1].At.Unix() != 2 || out[2].At.Unix() != 1 {
		t.Errorf("순서: %+v", out)
	}
}

func TestAuditRingOverflowsFIFO(t *testing.T) {
	r := NewAuditRing(3)
	for i := 1; i <= 5; i++ {
		r.Push(AuditEntry{Action: "A", At: time.Unix(int64(i), 0)})
	}
	if r.Len() != 3 {
		t.Errorf("Len=%d, want 3 (cap)", r.Len())
	}
	out := r.List(0)
	// 최신 → 가장 오래: 5, 4, 3 (1, 2 는 덮어쓰여짐).
	if out[0].At.Unix() != 5 || out[1].At.Unix() != 4 || out[2].At.Unix() != 3 {
		t.Errorf("FIFO 실패: %+v", out)
	}
}

func TestAuditRingLimit(t *testing.T) {
	r := NewAuditRing(10)
	for i := 1; i <= 5; i++ {
		r.Push(AuditEntry{At: time.Unix(int64(i), 0)})
	}
	out := r.List(2)
	if len(out) != 2 {
		t.Fatalf("limit 2 인데 len=%d", len(out))
	}
	if out[0].At.Unix() != 5 || out[1].At.Unix() != 4 {
		t.Errorf("limit 결과: %+v", out)
	}
}

func TestAuditRingDefaultCapacity(t *testing.T) {
	r := NewAuditRing(0)
	for i := 0; i < 250; i++ {
		r.Push(AuditEntry{Action: "X"})
	}
	if r.Len() != 200 {
		t.Errorf("default cap 200, got %d", r.Len())
	}
}

func TestAuditRingAtAutoFill(t *testing.T) {
	r := NewAuditRing(2)
	r.Push(AuditEntry{Action: "X"})
	out := r.List(0)
	if out[0].At.IsZero() {
		t.Error("At 자동 채움 안됨")
	}
}

func TestAuditRingResourceField(t *testing.T) {
	// Resource 필드는 카테고리 필터/UI 칩에 사용된다 — round-trip 손실 X 검증.
	r := NewAuditRing(4)
	r.Push(AuditEntry{Action: "PUT_SYMBOL", Resource: "symbol", Usid: "alice"})
	r.Push(AuditEntry{Action: "POLICY_KILL_SWITCH", Resource: "policy", Usid: "bob"})
	out := r.List(0)
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].Resource != "policy" || out[1].Resource != "symbol" {
		t.Errorf("Resource round-trip 실패: %+v", out)
	}
}

func TestAuditRingConcurrent(t *testing.T) {
	r := NewAuditRing(50)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Push(AuditEntry{Action: "X", Usid: "u"})
			r.List(10)
			r.Len()
		}(i)
	}
	wg.Wait()
	if r.Len() != 50 {
		t.Errorf("Len=%d, want 50", r.Len())
	}
}
