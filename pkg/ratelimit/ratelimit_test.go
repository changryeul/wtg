package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterAllowAndDeny(t *testing.T) {
	l := NewLimiter(Config{
		RatePerSec:     1, // 초당 1
		Burst:          2, // 버스트 2
		EvictionPeriod: 0, // GC 비활성
	})
	defer l.Stop()

	// 첫 2건은 burst 안에서 통과.
	if !l.Allow("k") {
		t.Error("첫 토큰 거부됨")
	}
	if !l.Allow("k") {
		t.Error("두번째 토큰 거부됨")
	}
	// 3번째는 burst 초과 → 거부.
	if l.Allow("k") {
		t.Error("burst 초과인데 통과됨")
	}
}

func TestLimiterPerKeyIndependent(t *testing.T) {
	l := NewLimiter(Config{RatePerSec: 1, Burst: 1})
	defer l.Stop()

	if !l.Allow("k1") {
		t.Error("k1 첫 토큰 거부")
	}
	if !l.Allow("k2") {
		t.Error("k2 첫 토큰 거부 (다른 키인데)")
	}
	// k1 두번째는 막혀야.
	if l.Allow("k1") {
		t.Error("k1 burst 초과인데 통과")
	}
}

func TestLimiterRecoversAfterDelay(t *testing.T) {
	l := NewLimiter(Config{RatePerSec: 100, Burst: 1})
	defer l.Stop()

	if !l.Allow("k") {
		t.Error("첫 토큰 거부")
	}
	if l.Allow("k") {
		t.Error("burst 초과인데 통과")
	}
	// 100/s 면 ~10ms 후 토큰 1개 회복.
	time.Sleep(20 * time.Millisecond)
	if !l.Allow("k") {
		t.Error("회복 후 토큰 거부")
	}
}

func TestLimiterEviction(t *testing.T) {
	l := NewLimiter(Config{
		RatePerSec:     1,
		Burst:          1,
		IdleEviction:   30 * time.Millisecond,
		EvictionPeriod: 10 * time.Millisecond,
	})
	defer l.Stop()

	l.Allow("user1")
	l.Allow("user2")
	if l.KeyCount() != 2 {
		t.Errorf("KeyCount: %d, want 2", l.KeyCount())
	}

	// 50ms 대기 → eviction 발생.
	time.Sleep(80 * time.Millisecond)
	if l.KeyCount() != 0 {
		t.Errorf("eviction 실패: KeyCount=%d", l.KeyCount())
	}
}

func TestIPKey(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"1.2.3.4:5000", "1.2.3.4"},
		{"[fd00::1]:5000", "fd00::1"},
		{"127.0.0.1:80", "127.0.0.1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = c.remote
		if got := IPKey(req); got != c.want {
			t.Errorf("IPKey(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}

func TestHeaderKey(t *testing.T) {
	fn := HeaderKey("X-WTG-User")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-WTG-User", "trader01")
	if got := fn(req); got != "trader01" {
		t.Errorf("HeaderKey: %q", got)
	}
}

func TestMiddlewareAllowsThenRejects(t *testing.T) {
	l := NewLimiter(Config{RatePerSec: 1, Burst: 1})
	defer l.Stop()

	var called atomic.Int32
	mw := Middleware(l, IPKey)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5000"

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req)
	if rr1.Code != http.StatusOK {
		t.Errorf("첫 요청 status: %d", rr1.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusTooManyRequests {
		t.Errorf("두번째 요청 status: %d, want 429", rr2.Code)
	}
	if rr2.Header().Get("Retry-After") == "" {
		t.Error("Retry-After 헤더 누락")
	}
	if called.Load() != 1 {
		t.Errorf("핸들러 호출 수: %d, want 1", called.Load())
	}
}

func TestMiddlewareEmptyKeyPasses(t *testing.T) {
	l := NewLimiter(Config{RatePerSec: 1, Burst: 1})
	defer l.Stop()
	mw := Middleware(l, HeaderKey("X-Missing"))
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("빈 키는 통과해야 함")
	}
}
