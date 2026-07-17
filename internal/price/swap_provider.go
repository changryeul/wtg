package price

import (
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_provider.go — AlgoSwapProvider 구현. AlgoStream 이 forward tenor 별 swap 을
// 합성할 때 쓴다. 세 소스 결합:
//   - received (ReceivedSwapStore) : 로이터 수신 원본 (in-memory)
//   - delta    (PricingTable.SwapPoint via Store) : 운영자 조정 (etcd)
//   - symbol→pair (SymbolMap)      : AlgoQuote symbol("USDKRW") → SwapKey pair("USD/KRW")
//
// effective = received + delta (EffectiveSwap). 로이터 미연결 시 received=0 →
// effective=delta (기존 운영자 수동 swap 하위호환).

// SwapProvider 는 AlgoSwapProvider 를 구현한다.
type SwapProvider struct {
	received *ReceivedSwapStore
	store    *pricing.Store   // delta = Load().SwapPoint
	symbols  *quote.SymbolMap // symbol → session.Pair
}

// NewSwapProvider 는 provider 를 생성한다. store/symbols 가 nil 이면 해당 소스 무시.
func NewSwapProvider(received *ReceivedSwapStore, store *pricing.Store, symbols *quote.SymbolMap) *SwapProvider {
	return &SwapProvider{received: received, store: store, symbols: symbols}
}

// pairFor — AlgoQuote symbol → session.Pair (SwapKey 용).
func (p *SwapProvider) pairFor(symbol string) (session.Pair, bool) {
	if p.symbols == nil {
		return "", false
	}
	pair, active, found := p.symbols.Lookup(symbol)
	return pair, found && active
}

// deltaTable — 현재 PricingTable (nil 안전).
func (p *SwapProvider) deltaTable() *pricing.PricingTable {
	if p.store == nil {
		return nil
	}
	return p.store.Load()
}

// Tenors — symbol 에 등록된 forward tenor (received + delta 합집합, spot 제외).
func (p *SwapProvider) Tenors(symbol string) []string {
	pair, ok := p.pairFor(symbol)
	if !ok {
		return nil
	}
	set := map[pricing.Tenor]struct{}{}
	if p.received != nil {
		for k := range p.received.Snapshot() {
			if k.Pair == pair {
				set[k.Tenor] = struct{}{}
			}
		}
	}
	if tbl := p.deltaTable(); tbl != nil {
		for k := range tbl.SwapPoint {
			if k.Pair == pair {
				set[k.Tenor] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for tn := range set {
		if tn == pricing.TenorSpot || string(tn) == SpotTenor {
			continue // spot 은 swap 없음.
		}
		out = append(out, string(tn))
	}
	return out
}

// Effective — (symbol, tenor) 의 적용 swap = received + delta.
func (p *SwapProvider) Effective(symbol, tenor string) (bid, ask float64, ok bool) {
	pair, found := p.pairFor(symbol)
	if !found {
		return 0, 0, false
	}
	var recv pricing.Margin
	if p.received != nil {
		recv, _ = p.received.Get(pair, pricing.Tenor(tenor))
	}
	var delta pricing.Margin
	if tbl := p.deltaTable(); tbl != nil {
		delta = tbl.SwapPoint[pricing.SwapKey{Pair: pair, Tenor: pricing.Tenor(tenor)}]
	}
	eff := EffectiveSwap(recv, delta)
	return eff.BidAmount, eff.AskAmount, true
}
