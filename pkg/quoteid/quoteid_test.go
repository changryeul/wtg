package quoteid

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerator_NextUnique(t *testing.T) {
	g := NewGenerator("A")
	seen := map[QuoteID]struct{}{}
	for i := 0; i < 10_000; i++ {
		id := g.Next()
		if _, dup := seen[id]; dup {
			t.Fatalf("중복 QuoteID 발생: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestGenerator_InstancePrefix(t *testing.T) {
	a := NewGenerator("A").Next()
	b := NewGenerator("B").Next()
	if !strings.HasPrefix(string(a), "A-") {
		t.Errorf("A 인스턴스 prefix mismatch: %s", a)
	}
	if !strings.HasPrefix(string(b), "B-") {
		t.Errorf("B 인스턴스 prefix mismatch: %s", b)
	}
}

func TestGenerator_DefaultInstanceFallback(t *testing.T) {
	g := NewGenerator("")
	if g.Instance() != "A" {
		t.Errorf("빈 instance 는 'A' 로 fallback 해야 함, got %q", g.Instance())
	}
}

func TestGenerator_FixedTime(t *testing.T) {
	g := NewGenerator("X")
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.SetNow(func() time.Time { return fixed })
	id1 := g.Next()
	id2 := g.Next()
	if id1 == id2 {
		t.Fatal("같은 ms 내 seq 가 다르므로 ID 가 달라야 함")
	}
	// 같은 ms 부분, 다른 seq.
	if !strings.HasPrefix(string(id1), "X-") || !strings.HasPrefix(string(id2), "X-") {
		t.Errorf("instance prefix 누락: %s / %s", id1, id2)
	}
}

func TestGenerator_ConcurrentUnique(t *testing.T) {
	g := NewGenerator("C")
	const goroutines = 32
	const perG = 1_000
	out := make(chan QuoteID, goroutines*perG)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				out <- g.Next()
			}
		}()
	}
	wg.Wait()
	close(out)
	seen := map[QuoteID]struct{}{}
	for id := range out {
		if _, dup := seen[id]; dup {
			t.Fatalf("동시 호출에서 ID 중복: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != goroutines*perG {
		t.Errorf("유니크 ID 수 mismatch: got %d, want %d", len(seen), goroutines*perG)
	}
}

func TestRecord_ValidAt(t *testing.T) {
	now := time.Unix(1700000000, 0)
	rec := Record{
		IssuedAt:   now.UnixNano(),
		ValidUntil: now.Add(500 * time.Millisecond).UnixNano(),
	}
	if !rec.ValidAt(now.Add(100 * time.Millisecond)) {
		t.Error("100ms 후는 valid 여야 함")
	}
	if !rec.ValidAt(now) {
		t.Error("IssuedAt 정확히 일치도 valid (반열림)")
	}
	if rec.ValidAt(now.Add(500 * time.Millisecond)) {
		t.Error("ValidUntil 정확히 일치는 invalid (반열림)")
	}
	if rec.ValidAt(now.Add(time.Second)) {
		t.Error("ValidUntil 이후는 invalid")
	}
	if rec.ValidAt(now.Add(-time.Second)) {
		t.Error("IssuedAt 이전은 invalid")
	}
}
