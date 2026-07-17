package price

import (
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// swap_provider.go — AlgoSwapProvider 구현. AlgoStream 이 forward tenor 별 swap 을
// 합성할 때 쓴다. SwapStore(received+delta, add 규약) + SymbolMap(symbol→pair) 결합.
//
// effective = received + delta (SwapStore.Effective). 부호 규약: mds add —
// forward = spot + effective (AlgoStream/forward_snapshot 동일).

// SwapProvider 는 AlgoSwapProvider 를 구현한다.
type SwapProvider struct {
	store   *SwapStore
	symbols *quote.SymbolMap // symbol → session.Pair
}

// NewSwapProvider 는 provider 를 생성한다.
func NewSwapProvider(store *SwapStore, symbols *quote.SymbolMap) *SwapProvider {
	return &SwapProvider{store: store, symbols: symbols}
}

// pairFor — AlgoQuote symbol → session.Pair (SwapKey 용).
func (p *SwapProvider) pairFor(symbol string) (session.Pair, bool) {
	if p.symbols == nil {
		return "", false
	}
	pair, active, found := p.symbols.Lookup(symbol)
	return pair, found && active
}

// Tenors — symbol 에 등록된 forward tenor (received ∪ delta, spot 제외).
func (p *SwapProvider) Tenors(symbol string) []string {
	pair, ok := p.pairFor(symbol)
	if !ok || p.store == nil {
		return nil
	}
	tns := p.store.Tenors(pair)
	out := make([]string, 0, len(tns))
	for _, tn := range tns {
		out = append(out, string(tn))
	}
	return out
}

// Effective — (symbol, tenor) 의 적용 swap = received + delta.
func (p *SwapProvider) Effective(symbol, tenor string) (bid, ask float64, ok bool) {
	pair, found := p.pairFor(symbol)
	if !found || p.store == nil {
		return 0, 0, false
	}
	m, has := p.store.Effective(pair, pricing.Tenor(tenor))
	if !has {
		return 0, 0, false
	}
	return m.BidAmount, m.AskAmount, true
}
