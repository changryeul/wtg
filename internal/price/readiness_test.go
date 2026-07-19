package price

import (
	"testing"
	"time"
)

// readiness — warm-up gate. 갓 뜬 인스턴스는 tick 을 충분히 볼 때까지 not-ready
// → edge round_robin / LB healthcheck 가 skip. 조용한 합류 후 서빙.

func TestReadiness_NotReadyBeforeWarmup(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newReadiness(2*time.Second, 30*time.Second)
	r.nowFn = func() time.Time { return now }
	r.start = now

	r.markTick()
	// warmup(2s) 경과 전 → tick 봤어도 not-ready.
	if r.isReady() {
		t.Error("warmup 전인데 ready")
	}
}

func TestReadiness_ReadyAfterWarmupWithTicks(t *testing.T) {
	base := time.Unix(1000, 0)
	cur := base
	r := newReadiness(2*time.Second, 30*time.Second)
	r.nowFn = func() time.Time { return cur }
	r.start = base

	r.markTick()
	cur = base.Add(3 * time.Second) // warmup 경과
	if !r.isReady() {
		t.Error("warmup 경과 + tick 있는데 not-ready")
	}
	// 한 번 ready 면 계속 ready (되돌아가지 않음).
	cur = base
	if !r.isReady() {
		t.Error("ready 가 not-ready 로 되돌아감")
	}
}

func TestReadiness_NotReadyWithoutTicks(t *testing.T) {
	base := time.Unix(1000, 0)
	cur := base
	r := newReadiness(2*time.Second, 30*time.Second)
	r.nowFn = func() time.Time { return cur }
	r.start = base

	// tick 0 + warmup 경과 → 아직 not-ready (조용한 시장은 maxWarmup 까지 대기).
	cur = base.Add(3 * time.Second)
	if r.isReady() {
		t.Error("tick 없이 warmup 만으로 ready")
	}
}

func TestReadiness_MaxWarmupFallback(t *testing.T) {
	base := time.Unix(1000, 0)
	cur := base
	r := newReadiness(2*time.Second, 30*time.Second)
	r.nowFn = func() time.Time { return cur }
	r.start = base

	// tick 이 전혀 없어도 maxWarmup(30s) 경과 시 ready (stuck 방지).
	cur = base.Add(31 * time.Second)
	if !r.isReady() {
		t.Error("maxWarmup 경과인데 not-ready")
	}
}
