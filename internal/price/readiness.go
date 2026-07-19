package price

import (
	"sync/atomic"
	"time"
)

// readiness — warm-up gate. 갓 뜬(또는 재연결한) mci-price 인스턴스는 아직 모든
// source tick 을 못 봐 BEST 가 불완전하다. 그 사이 서빙하면 반쪽 BEST 를 낸다.
//
// 정책: 부팅 후 not-ready → edge round_robin / LB healthcheck 가 skip.
//
//	ready 조건: warmup 경과 + tick ≥1 (활성 feed 를 봄), 또는
//	            maxWarmup 경과 (조용한 시장 fallback — 영구 not-ready 방지).
//	한 번 ready 면 되돌아가지 않는다 (latch).
//
// /v1/ready 가 이 상태를 노출 (docs/price-ha-grpc.md §3).
type readiness struct {
	start     time.Time
	warmup    time.Duration
	maxWarmup time.Duration
	ticks     atomic.Int64
	ready     atomic.Bool
	nowFn     func() time.Time // 테스트 주입용
}

func newReadiness(warmup, maxWarmup time.Duration) *readiness {
	r := &readiness{
		warmup:    warmup,
		maxWarmup: maxWarmup,
		nowFn:     time.Now,
	}
	r.start = time.Now()
	return r
}

// markTick — tick 1건 관측 (IngestEnvelopes 에서 호출).
func (r *readiness) markTick() { r.ticks.Add(1) }

// isReady — 현재 ready 여부. latch: 한 번 true 면 계속 true.
func (r *readiness) isReady() bool {
	if r.ready.Load() {
		return true
	}
	elapsed := r.nowFn().Sub(r.start)
	if (elapsed >= r.warmup && r.ticks.Load() > 0) || elapsed >= r.maxWarmup {
		r.ready.Store(true)
		return true
	}
	return false
}
