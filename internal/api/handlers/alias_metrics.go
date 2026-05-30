package handlers

import (
	"sync"
	"sync/atomic"
	"time"
)

// AliasMetrics — per-alias 호출 통계. /v1/tx 호출마다 RecordCall 누적.
//
// 두 가지 model:
//   - 합산 (Calls / Errors / TotalLatencyNs / MaxLatencyNs) — 정확한 누계 + p50/p95 추정 불가
//   - sketch (PercentileEstimator) — 정확한 p50/p95/p99 + 메모리 약간 더 사용
//
// 1차는 합산만. 정확한 percentile 필요 시 별도 도입.
type AliasMetrics struct {
	mu sync.RWMutex
	// alias → stat. unknown_alias 도 별도 키 ("__unknown__") 로 트래킹.
	stats map[string]*aliasStat
}

type aliasStat struct {
	calls    atomic.Uint64
	errors   atomic.Uint64 // broker error / unknown_alias / policy 거부
	sumNs    atomic.Uint64 // 누적 latency
	maxNs    atomic.Uint64 // 단일 최대 latency
	lastUnix atomic.Int64  // 마지막 호출 시각 (unix sec)
}

// NewAliasMetrics — 빈 통계 store 생성.
func NewAliasMetrics() *AliasMetrics {
	return &AliasMetrics{stats: make(map[string]*aliasStat)}
}

// RecordCall — alias 호출 1건 누적. alias 빈 문자열 → "__raw__" 로 묶어 둠
// (envelope 에 alias 없이 exchange/routing_key 만 보낸 경우).
func (m *AliasMetrics) RecordCall(alias string, latency time.Duration, isError bool) {
	if m == nil {
		return
	}
	key := alias
	if key == "" {
		key = "__raw__"
	}
	m.mu.RLock()
	st, ok := m.stats[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		st, ok = m.stats[key]
		if !ok {
			st = &aliasStat{}
			m.stats[key] = st
		}
		m.mu.Unlock()
	}
	st.calls.Add(1)
	if isError {
		st.errors.Add(1)
	}
	ns := uint64(latency.Nanoseconds())
	st.sumNs.Add(ns)
	for {
		cur := st.maxNs.Load()
		if ns <= cur {
			break
		}
		if st.maxNs.CompareAndSwap(cur, ns) {
			break
		}
	}
	st.lastUnix.Store(time.Now().Unix())
}

// AliasStatSnapshot — 외부 노출용 직렬화 친화 모양.
type AliasStatSnapshot struct {
	Alias         string  `json:"alias"`
	Calls         uint64  `json:"calls"`
	Errors        uint64  `json:"errors"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	MaxLatencyMs  float64 `json:"max_latency_ms"`
	ErrorRatePct  float64 `json:"error_rate_pct"`
	LastCallUnix  int64   `json:"last_call_unix"`
}

// Snapshot — 모든 alias 통계의 현재 스냅샷 (sorted by Calls desc).
func (m *AliasMetrics) Snapshot() []AliasStatSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AliasStatSnapshot, 0, len(m.stats))
	for alias, st := range m.stats {
		calls := st.calls.Load()
		errs := st.errors.Load()
		sumNs := st.sumNs.Load()
		var avgMs float64
		if calls > 0 {
			avgMs = float64(sumNs) / float64(calls) / 1e6
		}
		var errPct float64
		if calls > 0 {
			errPct = float64(errs) / float64(calls) * 100
		}
		out = append(out, AliasStatSnapshot{
			Alias:        alias,
			Calls:        calls,
			Errors:       errs,
			AvgLatencyMs: avgMs,
			MaxLatencyMs: float64(st.maxNs.Load()) / 1e6,
			ErrorRatePct: errPct,
			LastCallUnix: st.lastUnix.Load(),
		})
	}
	// 내림차순 sort by Calls
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Calls < out[j].Calls; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
