package admin

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

func TestAuditRing_RedisBackendPersistsAndLists(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ring := NewAuditRing(10)
	ring.SetRedisBackend(rdb, "test:audit", 100, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 5; i++ {
		ring.Push(AuditEntry{
			Action:   "PUT_ROUTE",
			Resource: "route",
			Usid:     "alice",
			Attrs:    map[string]any{"alias": fmt.Sprintf("ALIAS_%d", i)},
		})
	}

	// 재시작 시뮬레이션 — 같은 Redis 에 새 ring 붙임.
	newRing := NewAuditRing(10)
	newRing.SetRedisBackend(rdb, "test:audit", 100, slog.New(slog.NewTextHandler(io.Discard, nil)))

	out := newRing.List(0)
	if len(out) != 5 {
		t.Fatalf("재시작 후 list len=%d, want 5", len(out))
	}
	// 최신 (ALIAS_4) 부터 역순.
	if got, _ := out[0].Attrs["alias"].(string); got != "ALIAS_4" {
		t.Errorf("최신 entry alias=%q, want ALIAS_4", got)
	}
	if got, _ := out[4].Attrs["alias"].(string); got != "ALIAS_0" {
		t.Errorf("가장 오래된 alias=%q, want ALIAS_0", got)
	}
}

// Redis 다운 → fail-open + failCount 증가. in-memory 는 그대로.
func TestAuditRing_RedisFailOpenFallsBackToMemory(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ring := NewAuditRing(10)
	ring.SetRedisBackend(rdb, "test:audit", 100, slog.New(slog.NewTextHandler(io.Discard, nil)))

	mr.Close() // Redis 다운

	ring.Push(AuditEntry{Action: "PUT_ROUTE", Resource: "route"})
	if ring.RedisFailCount() < 1 {
		t.Errorf("Redis 다운인데 failCount=%d", ring.RedisFailCount())
	}
	// in-memory 로 fallback — List 가 in-memory 에서 가져옴.
	out := ring.List(0)
	if len(out) != 1 {
		t.Errorf("in-memory fallback len=%d, want 1", len(out))
	}
}

// LTRIM 으로 maxLen 초과 자동 정리.
func TestAuditRing_RedisLTrimRespectsMaxLen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ring := NewAuditRing(100)
	ring.SetRedisBackend(rdb, "test:audit", 3, slog.New(slog.NewTextHandler(io.Discard, nil))) // maxLen=3

	for i := 0; i < 10; i++ {
		ring.Push(AuditEntry{Action: fmt.Sprintf("A%d", i)})
	}

	out := ring.List(0)
	if len(out) != 3 {
		t.Errorf("LTRIM 후 len=%d, want 3", len(out))
	}
	// 최신 3개 (A7, A8, A9) 만 남아야.
	wantActions := []string{"A9", "A8", "A7"}
	for i, w := range wantActions {
		if out[i].Action != w {
			t.Errorf("out[%d].Action=%q, want %q", i, out[i].Action, w)
		}
	}
}
