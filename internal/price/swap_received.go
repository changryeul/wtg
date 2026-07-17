package price

import (
	"sync"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_received.go — swap point 의 in-memory store (received + delta) + effective.
//
// 배경 (mds 대체): mds 는 로이터 swap 을 SHM fold 에 적용해 algo/forward 가 읽었다.
// WTG 는 SHM 대신 mci-price 프로세스 메모리(본 store)를 쓴다.
//
// **부호 규약 (2026-07-17 방향 A)**: 전 층 mds add 규약 — `forward = spot + swap`.
// received/delta/effective 의 bid/ask amount 는 모두 "spot 에 더할 points".
// 엔진(customer) PricingTable.SwapPoint(bid 음수 저장, spread-widen 규약)와는
// 분리 — 여기 delta 는 그 SwapPoint 를 재사용하지 않는 **전용 store**.
//
// 모델:
//   - received : 로이터 수신 원본 (feed → SetReceived)
//   - delta    : 운영자 조정(skew) (admin → SetDelta). received 갱신돼도 유지.
//   - effective = received + delta : forward 견적 + AlgoStream 에 적용.

// SwapStore — (pair, tenor) → 수신·조정 swap point. 비영속(in-memory).
type SwapStore struct {
	mu       sync.RWMutex
	received map[pricing.SwapKey]pricing.Margin
	delta    map[pricing.SwapKey]pricing.Margin
}

// NewSwapStore 는 빈 store 를 만든다.
func NewSwapStore() *SwapStore {
	return &SwapStore{
		received: map[pricing.SwapKey]pricing.Margin{},
		delta:    map[pricing.SwapKey]pricing.Margin{},
	}
}

func key(pair session.Pair, tenor pricing.Tenor) pricing.SwapKey {
	return pricing.SwapKey{Pair: pair, Tenor: tenor}
}

// SetReceived — 로이터 수신값 최신 저장 (add 규약 points).
func (s *SwapStore) SetReceived(pair session.Pair, tenor pricing.Tenor, bid, ask float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received[key(pair, tenor)] = pricing.Margin{BidAmount: bid, AskAmount: ask}
}

// SetDelta — 운영자 조정값 저장 (add 규약 points).
func (s *SwapStore) SetDelta(pair session.Pair, tenor pricing.Tenor, bid, ask float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delta[key(pair, tenor)] = pricing.Margin{BidAmount: bid, AskAmount: ask}
}

// Received — 수신값. 없으면 zero + false.
func (s *SwapStore) Received(pair session.Pair, tenor pricing.Tenor) (pricing.Margin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.received[key(pair, tenor)]
	return m, ok
}

// Delta — 조정값. 없으면 zero + false.
func (s *SwapStore) Delta(pair session.Pair, tenor pricing.Tenor) (pricing.Margin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.delta[key(pair, tenor)]
	return m, ok
}

// Effective — 적용 swap = received + delta (add 규약). received/delta 둘 중 하나라도
// 있으면 ok=true.
func (s *SwapStore) Effective(pair session.Pair, tenor pricing.Tenor) (pricing.Margin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := key(pair, tenor)
	r, rok := s.received[k]
	d, dok := s.delta[k]
	if !rok && !dok {
		return pricing.Margin{}, false
	}
	return EffectiveSwap(r, d), true
}

// Tenors — pair 에 등록된 tenor (received ∪ delta, spot 제외).
func (s *SwapStore) Tenors(pair session.Pair) []pricing.Tenor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := map[pricing.Tenor]struct{}{}
	for k := range s.received {
		if k.Pair == pair {
			set[k.Tenor] = struct{}{}
		}
	}
	for k := range s.delta {
		if k.Pair == pair {
			set[k.Tenor] = struct{}{}
		}
	}
	out := make([]pricing.Tenor, 0, len(set))
	for tn := range set {
		if tn == pricing.TenorSpot || string(tn) == SpotTenor {
			continue
		}
		out = append(out, tn)
	}
	return out
}

// SwapView — admin 표시용 한 (pair, tenor) 의 수신·조정·적용.
type SwapView struct {
	Pair     string  `json:"pair"`
	Tenor    string  `json:"tenor"`
	RecvBid  float64 `json:"recv_bid"`
	RecvAsk  float64 `json:"recv_ask"`
	DeltaBid float64 `json:"delta_bid"`
	DeltaAsk float64 `json:"delta_ask"`
	EffBid   float64 `json:"eff_bid"`
	EffAsk   float64 `json:"eff_ask"`
}

// ViewSnapshot — admin 화면용 병합 스냅샷 (received ∪ delta 의 모든 key).
func (s *SwapStore) ViewSnapshot() []SwapView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[pricing.SwapKey]struct{}{}
	for k := range s.received {
		seen[k] = struct{}{}
	}
	for k := range s.delta {
		seen[k] = struct{}{}
	}
	out := make([]SwapView, 0, len(seen))
	for k := range seen {
		r := s.received[k]
		d := s.delta[k]
		eff := EffectiveSwap(r, d)
		out = append(out, SwapView{
			Pair: string(k.Pair), Tenor: string(k.Tenor),
			RecvBid: r.BidAmount, RecvAsk: r.AskAmount,
			DeltaBid: d.BidAmount, DeltaAsk: d.AskAmount,
			EffBid: eff.BidAmount, EffAsk: eff.AskAmount,
		})
	}
	return out
}

// EffectiveSwap 은 적용 swap = received + delta (모두 add 규약).
func EffectiveSwap(received, delta pricing.Margin) pricing.Margin {
	return pricing.Margin{
		BidAmount:    received.BidAmount + delta.BidAmount,
		AskAmount:    received.AskAmount + delta.AskAmount,
		SkewAmount:   received.SkewAmount + delta.SkewAmount,
		SpreadAmount: received.SpreadAmount + delta.SpreadAmount,
	}
}
