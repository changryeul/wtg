package price

import (
	"sync"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_received.go — 로이터 수신 swap point 의 in-memory staging + effective 계산.
//
// 배경 (mds 대체): mds 는 로이터 swap 을 SHM fold 에 적용해 algo/forward 가 읽었다.
// WTG 는 SHM 대신 mci-price 프로세스 메모리(본 store)를 쓴다. 수신값은 여기 보관만
// 하고 자동 적용하지 않는다 — 운영자가 admin 화면에서 확인·조정(delta) 후 반영.
//
// 모델 (2026-07-17 결정):
//   - received (본 store, in-memory)      : 로이터 원본 forward point (bid/ask amount)
//   - delta    (PricingTable.SwapPoint, etcd): 운영자 조정(skew) — 기존 운영 경로 재해석
//   - effective = received + delta          : forward 견적 + AlgoStream 에 실제 적용
//
// 로이터 수신이 갱신되면 effective 도 delta 유지한 채 자동 따라감(delta/skew 모델).

// ReceivedSwapStore — (pair, tenor) → 로이터 수신 raw swap point. 비영속.
type ReceivedSwapStore struct {
	mu sync.RWMutex
	m  map[pricing.SwapKey]pricing.Margin
}

// NewReceivedSwapStore 는 빈 store 를 만든다.
func NewReceivedSwapStore() *ReceivedSwapStore {
	return &ReceivedSwapStore{m: map[pricing.SwapKey]pricing.Margin{}}
}

// Set 은 (pair, tenor) 의 수신 swap point 를 최신값으로 저장한다.
func (s *ReceivedSwapStore) Set(pair session.Pair, tenor pricing.Tenor, bid, ask float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[pricing.SwapKey{Pair: pair, Tenor: tenor}] = pricing.Margin{BidAmount: bid, AskAmount: ask}
}

// Get 은 (pair, tenor) 의 수신 swap point 를 돌려준다. 없으면 zero Margin + false.
func (s *ReceivedSwapStore) Get(pair session.Pair, tenor pricing.Tenor) (pricing.Margin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.m[pricing.SwapKey{Pair: pair, Tenor: tenor}]
	return m, ok
}

// Snapshot 은 admin 표시용 복사본을 돌려준다 (원본과 격리).
func (s *ReceivedSwapStore) Snapshot() map[pricing.SwapKey]pricing.Margin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[pricing.SwapKey]pricing.Margin, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// EffectiveSwap 은 적용 swap = 수신값(로이터) + 운영자 delta 를 계산한다.
// 수신값이 없으면(zero) effective = delta (Reuters 미연결 하위호환).
func EffectiveSwap(received, delta pricing.Margin) pricing.Margin {
	return pricing.Margin{
		BidAmount:    received.BidAmount + delta.BidAmount,
		AskAmount:    received.AskAmount + delta.AskAmount,
		SkewAmount:   received.SkewAmount + delta.SkewAmount,
		SpreadAmount: received.SpreadAmount + delta.SpreadAmount,
	}
}
