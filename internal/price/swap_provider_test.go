package price

import (
	"sort"
	"testing"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
)

func newTestSwapProvider() (*SwapProvider, *ReceivedSwapStore, *pricing.Store) {
	recv := NewReceivedSwapStore()
	store := pricing.NewStore()
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}})
	return NewSwapProvider(recv, store, syms), recv, store
}

// Effective = received + delta(PricingTable.SwapPoint).
func TestSwapProvider_Effective(t *testing.T) {
	p, recv, store := newTestSwapProvider()
	recv.Set("USD/KRW", "M01", 2.50, 2.70) // 로이터 수신
	tbl := &pricing.PricingTable{SwapPoint: map[pricing.SwapKey]pricing.Margin{}}
	tbl.SwapPoint[pricing.SwapKey{Pair: "USD/KRW", Tenor: "M01"}] = pricing.Margin{BidAmount: 0.05, AskAmount: -0.03} // 운영자 delta
	store.Replace(tbl)

	bid, ask, ok := p.Effective("USDKRW", "M01")
	if !ok {
		t.Fatal("Effective ok=false")
	}
	if !near(bid, 2.55) || !near(ask, 2.67) {
		t.Errorf("effective bid=%v ask=%v, want 2.55/2.67 (received+delta)", bid, ask)
	}
}

// 미등록 심볼은 ok=false.
func TestSwapProvider_UnknownSymbol(t *testing.T) {
	p, _, _ := newTestSwapProvider()
	if _, _, ok := p.Effective("EURJPY", "M01"); ok {
		t.Error("미등록 심볼인데 ok=true")
	}
}

// Tenors = received + delta 의 forward tenor 합집합 (spot 제외).
func TestSwapProvider_Tenors(t *testing.T) {
	p, recv, store := newTestSwapProvider()
	recv.Set("USD/KRW", "M01", 2.5, 2.7)
	tbl := &pricing.PricingTable{SwapPoint: map[pricing.SwapKey]pricing.Margin{}}
	tbl.SwapPoint[pricing.SwapKey{Pair: "USD/KRW", Tenor: "M03"}] = pricing.Margin{BidAmount: 7, AskAmount: 7.4}
	tbl.SwapPoint[pricing.SwapKey{Pair: "USD/KRW", Tenor: pricing.TenorSpot}] = pricing.Margin{} // spot — 제외돼야
	store.Replace(tbl)

	got := p.Tenors("USDKRW")
	sort.Strings(got)
	if len(got) != 2 || got[0] != "M01" || got[1] != "M03" {
		t.Errorf("Tenors=%v, want [M01 M03] (spot 제외, received+delta 합집합)", got)
	}
}
