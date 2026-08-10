package pricing

import (
	"errors"
	"testing"
)

// 데모 풀커브(SPOT→1Y) 시드가 이전에 거부되던 결제일을 커버함을 증명.
// 기존 성긴 곡선(1W/1M/3M)은 offset<7 또는 >91 을 ErrOutOfRange 로 거부했다.
// #4-① 회귀: 단기(3일)·중기(6M)·장기(1Y) 가 모두 보간/exact 로 해소돼야 한다.
func fullDemoCurve() *PricingTable {
	return &PricingTable{
		Version: 14063,
		SwapPoint: map[SwapKey]Margin{
			{Pair: "USD/KRW", Tenor: TenorSpot}: {BidAmount: 0.00, AskAmount: 0.00},
			{Pair: "USD/KRW", Tenor: Tenor1W}:   {BidAmount: 0.05, AskAmount: 0.07},
			{Pair: "USD/KRW", Tenor: Tenor2W}:   {BidAmount: 0.09, AskAmount: 0.13},
			{Pair: "USD/KRW", Tenor: Tenor1M}:   {BidAmount: 0.15, AskAmount: 0.25},
			{Pair: "USD/KRW", Tenor: Tenor2M}:   {BidAmount: 0.27, AskAmount: 0.40},
			{Pair: "USD/KRW", Tenor: Tenor3M}:   {BidAmount: 0.40, AskAmount: 0.55},
			{Pair: "USD/KRW", Tenor: Tenor6M}:   {BidAmount: 0.80, AskAmount: 1.10},
			{Pair: "USD/KRW", Tenor: Tenor9M}:   {BidAmount: 1.20, AskAmount: 1.65},
			{Pair: "USD/KRW", Tenor: Tenor1Y}:   {BidAmount: 1.60, AskAmount: 2.20},
		},
	}
}

func TestSwapCurve_DemoFullCoverage(t *testing.T) {
	tbl := fullDemoCurve()
	// 이전 곡선이라면 거부됐을 결제일들 — 이제 전부 해소돼야 한다.
	cases := []struct {
		name       string
		offsetDays int
	}{
		{"단기 3일 (SPOT~1W 사이)", 3},
		{"5주 (1M~2M 사이)", 35},
		{"6M exact", DefaultTenorDays[Tenor6M]},
		{"장기 300일 (9M~1Y 사이)", 300},
		{"1Y exact", DefaultTenorDays[Tenor1Y]},
	}
	for _, c := range cases {
		r, err := tbl.InterpolateSwap("USD/KRW", c.offsetDays)
		if err != nil {
			t.Errorf("%s: 여전히 거부됨 (%v) — 곡선 커버리지 부족", c.name, err)
			continue
		}
		// bid/ask 가 단조 증가 곡선상 SPOT(0)~1Y(1.60/2.20) 범위 안이어야 한다.
		if r.Margin.BidAmount < 0 || r.Margin.AskAmount > 2.20+1e-9 {
			t.Errorf("%s: 보간값 범위 이상 %+v", c.name, r.Margin)
		}
		t.Logf("%-24s offset=%3d → bid=%.4f ask=%.4f (from=%s to=%s exact=%v)",
			c.name, c.offsetDays, r.Margin.BidAmount, r.Margin.AskAmount, r.From, r.To, r.Exact)
	}
	// 대조: 곡선을 SPOT/1Y 없이 성기게 만들면 장기(300일)는 거부돼야 한다 (회귀 근거).
	sparse := &PricingTable{Version: 1, SwapPoint: map[SwapKey]Margin{
		{Pair: "USD/KRW", Tenor: Tenor1W}: {BidAmount: 0.05, AskAmount: 0.07},
		{Pair: "USD/KRW", Tenor: Tenor3M}: {BidAmount: 0.40, AskAmount: 0.55},
	}}
	if _, err := sparse.InterpolateSwap("USD/KRW", 300); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("성긴 곡선 300일은 ErrOutOfRange 여야 함, got %v", err)
	}
}
