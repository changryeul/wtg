package price

import (
	"sync"
	"time"
)

// pairLiveness — per-pair 최신 quote 도착 시각 추적 + stale state.
//
// 운영 의미:
//   - upstream feed 단절 (cooker/forwarder 죽음)
//   - mci-price 측 정체 (pricing 비활성, broker → edge gRPC stream 끊김)
//   - 특정 pair 의 SymbolMap 비활성으로 PricingConsumer 가 발행 중단
//
// 위 어느 경우든 N초 무음 후 stale 진입 → client 에 알림.
type pairLiveness struct {
	mu     sync.Mutex
	lastTS map[string]time.Time // pair → 마지막 quote 도착 시각
	stale  map[string]bool      // 이미 stale 알림 보낸 pair set
}

// newPairLiveness — 빈 트래커 생성.
func newPairLiveness() *pairLiveness {
	return &pairLiveness{
		lastTS: make(map[string]time.Time),
		stale:  make(map[string]bool),
	}
}

// Update — quote 도착 기록. 만약 그 pair 가 stale 상태였으면 회복으로 판단해
// becameFresh=true 반환 (caller 가 fresh 알림 전송).
func (l *pairLiveness) Update(pair string, ts time.Time) (becameFresh bool) {
	if pair == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastTS[pair] = ts
	if l.stale[pair] {
		delete(l.stale, pair)
		return true
	}
	return false
}

// ScanForStale — threshold 초과 무음인 pair 를 새로 stale 로 표시하고
// 그 pair 목록을 반환. 이미 stale 인 pair 는 다시 보고하지 않음
// (알림 중복 방지).
func (l *pairLiveness) ScanForStale(now time.Time, threshold time.Duration) (newlyStale []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for pair, ts := range l.lastTS {
		if l.stale[pair] {
			continue
		}
		if now.Sub(ts) > threshold {
			l.stale[pair] = true
			newlyStale = append(newlyStale, pair)
		}
	}
	return
}

// Snapshot — 현재 상태 (디버그 / /v1/edge-stats 용). pair 별 last_ts + stale.
type pairLivenessEntry struct {
	Pair    string    `json:"pair"`
	LastTS  time.Time `json:"last_ts"`
	IsStale bool      `json:"is_stale"`
}

func (l *pairLiveness) Snapshot() []pairLivenessEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]pairLivenessEntry, 0, len(l.lastTS))
	for pair, ts := range l.lastTS {
		out = append(out, pairLivenessEntry{
			Pair:    pair,
			LastTS:  ts,
			IsStale: l.stale[pair],
		})
	}
	return out
}
