package price

import (
	"sort"
	"testing"

	"github.com/winwaysystems/wtg/pkg/quote"
)

func newTestSwapProvider() (*SwapProvider, *SwapStore) {
	store := NewSwapStore()
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}})
	return NewSwapProvider(store, syms), store
}

// Effective = received + delta (add 규약).
func TestSwapProvider_Effective(t *testing.T) {
	p, store := newTestSwapProvider()
	store.SetReceived("USD/KRW", "M01", 2.50, 2.70)
	store.SetDelta("USD/KRW", "M01", 0.05, -0.03)
	bid, ask, ok := p.Effective("USDKRW", "M01")
	if !ok {
		t.Fatal("Effective ok=false")
	}
	if !near(bid, 2.55) || !near(ask, 2.67) {
		t.Errorf("effective bid=%v ask=%v, want 2.55/2.67", bid, ask)
	}
}

func TestSwapProvider_UnknownSymbol(t *testing.T) {
	p, _ := newTestSwapProvider()
	if _, _, ok := p.Effective("EURJPY", "M01"); ok {
		t.Error("미등록 심볼인데 ok=true")
	}
}

// 등록 안 된 (pair,tenor) 는 ok=false → forward 미emit.
func TestSwapProvider_NoSwap(t *testing.T) {
	p, _ := newTestSwapProvider()
	if _, _, ok := p.Effective("USDKRW", "M01"); ok {
		t.Error("swap 없는데 ok=true")
	}
}

func TestSwapProvider_Tenors(t *testing.T) {
	p, store := newTestSwapProvider()
	store.SetReceived("USD/KRW", "M01", 2.5, 2.7)
	store.SetDelta("USD/KRW", "M03", 7, 7.4)
	got := p.Tenors("USDKRW")
	sort.Strings(got)
	if len(got) != 2 || got[0] != "M01" || got[1] != "M03" {
		t.Errorf("Tenors=%v, want [M01 M03]", got)
	}
}
