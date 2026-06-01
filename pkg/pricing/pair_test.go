package pricing

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestPairMaster_GetListSort(t *testing.T) {
	m := NewPairMaster()
	m.Replace([]Pair{
		{ID: "EURKRW", Base: "EUR", Quote: "KRW", Kind: "cross", SortOrder: 40, Active: true},
		{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Symbol: "USDKRW", SortOrder: 10, Active: true},
	})
	if m.Size() != 2 {
		t.Errorf("size = %d", m.Size())
	}
	p, ok := m.Get("USDKRW")
	if !ok || p.Kind != "direct" || p.Symbol != "USDKRW" {
		t.Errorf("Get USDKRW = %+v ok=%v", p, ok)
	}
	list := m.List()
	if list[0].ID != "USDKRW" || list[1].ID != "EURKRW" {
		t.Errorf("sort: %+v", list)
	}
}

func TestPairMaster_CrossFormulas(t *testing.T) {
	m := NewPairMaster()
	m.Replace([]Pair{
		{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Active: true},
		{ID: "EURKRW", Base: "EUR", Quote: "KRW", Kind: "cross", Active: true,
			Cross: &Cross{LegA: "EUR/USD", OpA: "mul", LegB: "USD/KRW", OpB: "mul", Scale: 1}},
		{ID: "JPYKRW", Base: "JPY", Quote: "KRW", Kind: "cross", Active: true,
			Cross: &Cross{LegA: "USD/KRW", OpA: "mul", LegB: "USD/JPY", OpB: "div", Scale: 100}},
		{ID: "HKDKRW", Base: "HKD", Quote: "KRW", Kind: "cross", Active: false, // inactive
			Cross: &Cross{LegA: "USD/KRW", OpA: "mul", LegB: "USD/HKD", OpB: "div", Scale: 1}},
	})
	f := m.CrossFormulas()
	if len(f) != 2 {
		t.Fatalf("formulas count = %d, want 2 (direct/inactive 제외)", len(f))
	}
	eur := f[session.Pair("EUR/KRW")]
	if eur.LegA != "EUR/USD" || eur.OpA != CrossOpMul || eur.Scale != 1 {
		t.Errorf("EUR/KRW formula: %+v", eur)
	}
	jpy := f[session.Pair("JPY/KRW")]
	if jpy.LegA != "USD/KRW" || jpy.OpB != CrossOpDiv || jpy.Scale != 100 {
		t.Errorf("JPY/KRW formula: %+v", jpy)
	}
	if _, ok := f[session.Pair("HKD/KRW")]; ok {
		t.Error("inactive HKD/KRW 가 formula 에 포함됨")
	}
}

func TestPairMaster_CrossFormulas_EmptyOnNoCross(t *testing.T) {
	m := NewPairMaster()
	m.Replace([]Pair{
		{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Active: true},
	})
	f := m.CrossFormulas()
	if len(f) != 0 {
		t.Errorf("direct only: formulas = %d, want 0", len(f))
	}
}
