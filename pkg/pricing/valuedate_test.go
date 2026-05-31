package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// ─── weekend-only calendar helpers ─────────────────────────────────────────

func TestIsBusinessDay(t *testing.T) {
	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // 월
	tue := mon.AddDate(0, 0, 1)
	fri := mon.AddDate(0, 0, 4)
	sat := mon.AddDate(0, 0, 5)
	sun := mon.AddDate(0, 0, 6)
	cases := []struct {
		name string
		d    time.Time
		want bool
	}{
		{"MON", mon, true}, {"TUE", tue, true}, {"FRI", fri, true},
		{"SAT", sat, false}, {"SUN", sun, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsBusinessDay(c.d); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestAddBusinessDays(t *testing.T) {
	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // 월

	cases := []struct {
		name string
		from time.Time
		add  int
		want time.Time
	}{
		{"0 day", mon, 0, mon},
		{"1 → tue", mon, 1, mon.AddDate(0, 0, 1)},
		{"4 → fri", mon, 4, mon.AddDate(0, 0, 4)},
		{"5 → next mon (skip sat/sun)", mon, 5, mon.AddDate(0, 0, 7)},
		{"7 → next wed", mon, 7, mon.AddDate(0, 0, 9)},
		// 거꾸로
		{"fri -1 → thu", mon.AddDate(0, 0, 4), -1, mon.AddDate(0, 0, 3)},
		{"mon -1 → prev fri", mon, -1, mon.AddDate(0, 0, -3)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AddBusinessDays(c.from, c.add)
			if !got.Equal(c.want) {
				t.Errorf("got %s, want %s", got.Format("2006-01-02 Mon"), c.want.Format("2006-01-02 Mon"))
			}
		})
	}
}

func TestBusinessDaysBetween(t *testing.T) {
	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	fri := mon.AddDate(0, 0, 4)
	nextMon := mon.AddDate(0, 0, 7)
	sat := mon.AddDate(0, 0, 5)

	cases := []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{"same day", mon, mon, 0},
		{"mon → fri", mon, fri, 4},
		{"fri → next mon", fri, nextMon, 1},
		{"mon → next mon", mon, nextMon, 5},
		{"sat → next mon", sat, nextMon, 1},
		{"reverse fri → mon", fri, mon, -4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BusinessDaysBetween(c.from, c.to); got != c.want {
				t.Errorf("from %s to %s: got %d, want %d",
					c.from.Format("2006-01-02 Mon"), c.to.Format("2006-01-02 Mon"), got, c.want)
			}
		})
	}
}

func TestSpotDate_T2(t *testing.T) {
	mon := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC) // 시각 무시
	got := SpotDate(mon, 2)
	want := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC) // 수요일
	if !got.Equal(want) {
		t.Errorf("SPOT(MON, T+2) = %s, want %s", got, want)
	}
	// 금요일 + T+2 → 다음 화요일.
	fri := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	got = SpotDate(fri, 2)
	want = time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("SPOT(FRI, T+2) = %s, want %s", got, want)
	}
}

// ─── InterpolateSwap ────────────────────────────────────────────────────────

func newSwapTable(t *testing.T) *PricingTable {
	t.Helper()
	return &PricingTable{
		Version: 99,
		SwapPoint: map[SwapKey]Margin{
			{Pair: "USD/KRW", Tenor: Tenor1W}: {BidAmount: 0.05, AskAmount: 0.07},
			{Pair: "USD/KRW", Tenor: Tenor1M}: {BidAmount: 0.20, AskAmount: 0.30},
			{Pair: "USD/KRW", Tenor: Tenor3M}: {BidAmount: 0.50, AskAmount: 0.70},
		},
	}
}

func TestInterpolateSwap_ExactMatch(t *testing.T) {
	tbl := newSwapTable(t)
	r, err := tbl.InterpolateSwap("USD/KRW", DefaultTenorDays[Tenor1W])
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exact || r.From != Tenor1W {
		t.Errorf("Exact 매칭 실패: %+v", r)
	}
	if !near(r.Margin.BidAmount, 0.05) || !near(r.Margin.AskAmount, 0.07) {
		t.Errorf("exact swap = %+v", r.Margin)
	}
}

func TestInterpolateSwap_BetweenTenors(t *testing.T) {
	tbl := newSwapTable(t)
	// 1W=7, 1M=30. offset=15 → weight = (15-7)/(30-7) = 8/23
	r, err := tbl.InterpolateSwap("USD/KRW", 15)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exact {
		t.Errorf("Exact 일 수 없음: %+v", r)
	}
	if r.From != Tenor1W || r.To != Tenor1M {
		t.Errorf("보간 인접: from=%s to=%s", r.From, r.To)
	}
	wantW := float64(8) / float64(23)
	if !near(r.Weight, wantW) {
		t.Errorf("weight = %v, want %v", r.Weight, wantW)
	}
	// bid: 0.05 + (0.20 - 0.05) * 8/23 = 0.05 + 0.0521739... = 0.1021739...
	wantBid := 0.05 + (0.20-0.05)*wantW
	wantAsk := 0.07 + (0.30-0.07)*wantW
	if !near(r.Margin.BidAmount, wantBid) {
		t.Errorf("bid = %v, want %v", r.Margin.BidAmount, wantBid)
	}
	if !near(r.Margin.AskAmount, wantAsk) {
		t.Errorf("ask = %v, want %v", r.Margin.AskAmount, wantAsk)
	}
}

func TestInterpolateSwap_HalfwayBoundary(t *testing.T) {
	tbl := newSwapTable(t)
	// 1W=7, 1M=30. 중간 = (7+30)/2 = 18.5 → offset=18 (정수만), weight 가 0.5 근처.
	// 보다 깨끗한 case: 1M=30, 3M=91. 중간 = 60.5 → offset=61 → weight = (61-30)/(91-30) = 31/61
	r, err := tbl.InterpolateSwap("USD/KRW", 61)
	if err != nil {
		t.Fatal(err)
	}
	if r.From != Tenor1M || r.To != Tenor3M {
		t.Errorf("range pick: from=%s to=%s", r.From, r.To)
	}
	wantW := float64(31) / float64(61)
	if !near(r.Weight, wantW) {
		t.Errorf("weight = %v, want %v", r.Weight, wantW)
	}
}

func TestInterpolateSwap_OutOfRange_TooSmall(t *testing.T) {
	tbl := newSwapTable(t)
	// 1W(7) 보다 작음 → SPOT entry 없으니 prev 없음 → ErrOutOfRange.
	_, err := tbl.InterpolateSwap("USD/KRW", 3)
	if !errors.Is(err, ErrOutOfRange) {
		t.Errorf("err = %v, want ErrOutOfRange", err)
	}
}

func TestInterpolateSwap_OutOfRange_TooLarge(t *testing.T) {
	tbl := newSwapTable(t)
	// 3M(91) 보다 큼 → next 없음.
	_, err := tbl.InterpolateSwap("USD/KRW", 200)
	if !errors.Is(err, ErrOutOfRange) {
		t.Errorf("err = %v, want ErrOutOfRange", err)
	}
}

func TestInterpolateSwap_NoSwapForPair(t *testing.T) {
	tbl := newSwapTable(t)
	_, err := tbl.InterpolateSwap("ZZZ/KRW", 30)
	if !errors.Is(err, ErrNoSwap) {
		t.Errorf("err = %v, want ErrNoSwap", err)
	}
}

func TestInterpolateSwap_UnknownTenor_Ignored(t *testing.T) {
	// DefaultTenorDays 에 없는 tenor 가 etcd 에 들어있으면 보간 무시.
	tbl := &PricingTable{
		Version: 1,
		SwapPoint: map[SwapKey]Margin{
			{Pair: "USD/KRW", Tenor: "WEIRD"}: {BidAmount: 1, AskAmount: 1},
			{Pair: "USD/KRW", Tenor: Tenor1W}: {BidAmount: 0.05, AskAmount: 0.07},
			{Pair: "USD/KRW", Tenor: Tenor1M}: {BidAmount: 0.20, AskAmount: 0.30},
		},
	}
	r, err := tbl.InterpolateSwap("USD/KRW", 15)
	if err != nil {
		t.Fatal(err)
	}
	// WEIRD 가 무시되고 1W~1M 사이 보간이 되는지.
	if r.From != Tenor1W || r.To != Tenor1M {
		t.Errorf("unknown tenor 무시 실패: %+v", r)
	}
}

// 실제 거래 흐름 시뮬레이션: 거래일 MON → SPOT(WED, T+2) → value_date 6/24 (FRI)
// → offsetDays 계산 → 보간.
func TestEndToEnd_ValueDateToInterpolatedSwap(t *testing.T) {
	tbl := newSwapTable(t)
	// 거래일 = 2026-06-01 (월).
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	spot := SpotDate(now, 2) // 2026-06-03 (수)
	// 고객이 결제일 = 2026-06-19 (금) 선택.
	valueDate := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	offsetDays := BusinessDaysBetween(spot, valueDate)
	// 6/3 (수) → 6/19 (금): 16일 차이지만 영업일만 카운트.
	// 6/3, 4, 5 (Mon... actually 6/3 = 수). 다음 영업일: 6/4(목), 5(금), 8(월), 9(화), 10(수),
	// 11(목), 12(금), 15(월), 16(화), 17(수), 18(목), 19(금) = 12 영업일.
	if offsetDays != 12 {
		t.Fatalf("offsetDays = %d, want 12", offsetDays)
	}
	// 12 일은 1W(7) ~ 1M(30) 사이. weight = (12-7)/(30-7) = 5/23
	r, err := tbl.InterpolateSwap("USD/KRW", offsetDays)
	if err != nil {
		t.Fatal(err)
	}
	wantW := float64(5) / float64(23)
	if !near(r.Weight, wantW) {
		t.Errorf("weight = %v, want %v", r.Weight, wantW)
	}
	wantBid := 0.05 + (0.20-0.05)*wantW
	if !near(r.Margin.BidAmount, wantBid) {
		t.Errorf("bid = %v, want %v", r.Margin.BidAmount, wantBid)
	}
}

// 외부 import 회피 — 별도 dummy 로직 들어가지 않게.
var _ = session.Pair("dummy")

// near — 부동소수점 epsilon 비교 (다른 test 파일과 별개로 자체 정의).
func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
