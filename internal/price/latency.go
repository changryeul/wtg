package price

import (
	"sync/atomic"
	"time"
)

// LatencyTracker — envelope.ts (cooker 측 시각) 와 mci-price IngestEnvelopes 진입
// 시각의 차이를 누계 + bucket 으로 가시화.
//
// 운영 의미:
//   - forwarder → mci-price 의 publish 까지 e2e 지연
//   - broker path: forwarder TCP → broker fan-out → mci-price unsolicited
//   - grpc path:   forwarder gRPC stream → mci-price PublishTick handler
//   - 두 path 의 latency 비교가 broker 우회의 정량적 효과 검증에 사용됨.
//
// clock 가정: forwarder 와 mci-price 가 같은 호스트 (또는 NTP 동기). 다른 host
// 면 clock skew 가 latency 에 섞임 — 그 경우 음수 또는 큰 음수치 보일 수 있음.
type LatencyTracker struct {
	count      atomic.Uint64
	sumNs      atomic.Uint64 // 누적 nanosecond (~ 18e9 hours overflow 후 wrap, 운영 단위로는 안전)
	maxNs      atomic.Uint64
	negativeCount atomic.Uint64 // clock skew or future-dated ts

	// 버킷 — bound 단위 (ns).
	bucketLT1ms    atomic.Uint64 // <    1ms
	bucketLT10ms   atomic.Uint64 // 1ms ~ 10ms
	bucketLT100ms  atomic.Uint64 // 10ms ~ 100ms
	bucketLT1s     atomic.Uint64 // 100ms ~ 1s
	bucketGE1s     atomic.Uint64 // >= 1s
}

// Observe — 단일 envelope 의 latency 기록.
func (t *LatencyTracker) Observe(producerTS time.Time, now time.Time) {
	if producerTS.IsZero() {
		return
	}
	delta := now.Sub(producerTS)
	if delta < 0 {
		t.negativeCount.Add(1)
		return
	}
	ns := uint64(delta.Nanoseconds())
	t.count.Add(1)
	t.sumNs.Add(ns)
	// max — CAS 루프.
	for {
		cur := t.maxNs.Load()
		if ns <= cur {
			break
		}
		if t.maxNs.CompareAndSwap(cur, ns) {
			break
		}
	}
	// bucket — 누적.
	switch {
	case delta < time.Millisecond:
		t.bucketLT1ms.Add(1)
	case delta < 10*time.Millisecond:
		t.bucketLT10ms.Add(1)
	case delta < 100*time.Millisecond:
		t.bucketLT100ms.Add(1)
	case delta < time.Second:
		t.bucketLT1s.Add(1)
	default:
		t.bucketGE1s.Add(1)
	}
}

// LatencySnapshot — 운영 모니터링 노출용 read-only 스냅샷.
type LatencySnapshot struct {
	Count           uint64  `json:"count"`
	AvgNs           uint64  `json:"avg_ns"`
	MaxNs           uint64  `json:"max_ns"`
	NegativeCount   uint64  `json:"negative_count"`           // clock skew / future ts
	BucketLT1ms     uint64  `json:"bucket_lt_1ms"`
	BucketLT10ms    uint64  `json:"bucket_lt_10ms"`
	BucketLT100ms   uint64  `json:"bucket_lt_100ms"`
	BucketLT1s      uint64  `json:"bucket_lt_1s"`
	BucketGE1s      uint64  `json:"bucket_ge_1s"`
	AvgMs           float64 `json:"avg_ms"`
	MaxMs           float64 `json:"max_ms"`
}

// Snapshot — atomic load 후 평균/ms 계산.
func (t *LatencyTracker) Snapshot() LatencySnapshot {
	count := t.count.Load()
	sumNs := t.sumNs.Load()
	maxNs := t.maxNs.Load()
	var avgNs uint64
	if count > 0 {
		avgNs = sumNs / count
	}
	return LatencySnapshot{
		Count:         count,
		AvgNs:         avgNs,
		MaxNs:         maxNs,
		NegativeCount: t.negativeCount.Load(),
		BucketLT1ms:   t.bucketLT1ms.Load(),
		BucketLT10ms:  t.bucketLT10ms.Load(),
		BucketLT100ms: t.bucketLT100ms.Load(),
		BucketLT1s:    t.bucketLT1s.Load(),
		BucketGE1s:    t.bucketGE1s.Load(),
		AvgMs:         float64(avgNs) / 1e6,
		MaxMs:         float64(maxNs) / 1e6,
	}
}
