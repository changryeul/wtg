package pricing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// ─── HolidayCalendar 단위 검증 ─────────────────────────────────────────────

func TestWeekendCalendar_IsBusinessDay(t *testing.T) {
	cal := WeekendCalendar{}
	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		d    time.Time
		want bool
	}{
		{"MON", mon, true},
		{"FRI", mon.AddDate(0, 0, 4), true},
		{"SAT", mon.AddDate(0, 0, 5), false},
		{"SUN", mon.AddDate(0, 0, 6), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cal.IsBusinessDay(c.d); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestHolidayCalendar_IsBusinessDay(t *testing.T) {
	hd, _ := ParseHolidayDate("2026-06-03") // 수
	cal := NewHolidayCalendar([]time.Time{hd})

	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		d    time.Time
		want bool
	}{
		{"MON 6/1", mon, true},
		{"TUE 6/2", mon.AddDate(0, 0, 1), true},
		{"WED 6/3 휴일", mon.AddDate(0, 0, 2), false},
		{"THU 6/4", mon.AddDate(0, 0, 3), true},
		{"SAT 6/6", mon.AddDate(0, 0, 5), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cal.IsBusinessDay(c.d); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestHolidayCalendar_LocationAgnostic(t *testing.T) {
	// 한국 timezone 으로 입력해도 calendar date 만 키로 보존.
	kst := time.FixedZone("KST", 9*3600)
	hd := time.Date(2026, 6, 3, 0, 0, 0, 0, kst)
	cal := NewHolidayCalendar([]time.Time{hd})

	// UTC 자정 6/3 도 같은 휴일로 인식.
	d := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	if cal.IsBusinessDay(d) {
		t.Errorf("KST 입력 휴일이 UTC date 에서 매칭 안 됨")
	}
}

// ─── PricingTable.Cal() — nil fallback / 빌드 통합 ─────────────────────────

func TestPricingTable_Cal_NilDefaultsToWeekend(t *testing.T) {
	tbl := &PricingTable{Calendar: nil}
	cal := tbl.Cal()
	// 토 비영업.
	sat := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	if cal.IsBusinessDay(sat) {
		t.Errorf("nil Calendar fallback 이 weekend 처리 안 함")
	}
}

func TestBuildPricingTable_HolidaysApplied(t *testing.T) {
	body := []byte(`{
	  "version": 21,
	  "holidays": ["2026-06-03", "2026-06-15"]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	cal := tbl.Cal()
	hc, ok := cal.(*HolidayCalendar)
	if !ok {
		t.Fatalf("Calendar 타입 = %T, want *HolidayCalendar", cal)
	}
	if hc.Count() != 2 {
		t.Errorf("휴일 count = %d, want 2", hc.Count())
	}
	// 6/3 (수) 비영업.
	wed := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	if cal.IsBusinessDay(wed) {
		t.Errorf("6/3 휴일 누락")
	}
}

func TestBuildPricingTable_EmptyHolidays_WeekendFallback(t *testing.T) {
	body := []byte(`{"version": 1}`)
	tbl, _ := ParsePricingTable(body)
	if tbl.Calendar != nil {
		t.Errorf("empty doc 의 Calendar = %T, want nil", tbl.Calendar)
	}
	// Cal() 은 WeekendCalendar 반환.
	if _, ok := tbl.Cal().(WeekendCalendar); !ok {
		t.Errorf("Cal() fallback = %T, want WeekendCalendar", tbl.Cal())
	}
}

func TestBuildPricingTable_InvalidHolidayDate_Skipped(t *testing.T) {
	body := []byte(`{
	  "version": 1,
	  "holidays": ["2026-06-03", "not-a-date", "2026-13-99", "2026-06-04"]
	}`)
	tbl, _ := ParsePricingTable(body)
	hc, ok := tbl.Calendar.(*HolidayCalendar)
	if !ok {
		t.Fatalf("Calendar 타입 = %T", tbl.Calendar)
	}
	// 유효 2건 (6/3, 6/4) 만.
	if hc.Count() != 2 {
		t.Errorf("invalid skip 실패: count = %d, want 2", hc.Count())
	}
}

// ─── Calendar-aware 함수 ───────────────────────────────────────────────────

func TestAddBusinessDaysCal_WithHoliday(t *testing.T) {
	// 6/3 (수) 휴일 → MON 6/1 + 2 영업일 = TUE 6/2 + 1 = THU 6/4 (수 skip).
	hd, _ := ParseHolidayDate("2026-06-03")
	cal := NewHolidayCalendar([]time.Time{hd})
	mon := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := AddBusinessDaysCal(mon, 2, cal)
	want := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC) // THU
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got.Format("2006-01-02 Mon"), want.Format("2006-01-02 Mon"))
	}
}

func TestBusinessDaysBetweenCal_WithHoliday(t *testing.T) {
	// MON 6/1 → MON 6/8. weekend-only 면 5 영업일.
	// 6/3, 6/4 둘 다 휴일이면 영업일 = 3 (6/2, 6/5, 6/8).
	holidays := []time.Time{
		mustDate("2026-06-03"),
		mustDate("2026-06-04"),
	}
	cal := NewHolidayCalendar(holidays)
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	if got := BusinessDaysBetweenCal(from, to, cal); got != 3 {
		t.Errorf("with holidays: got %d, want 3", got)
	}
	// weekend-only fallback (cal=nil) 는 5.
	if got := BusinessDaysBetweenCal(from, to, nil); got != 5 {
		t.Errorf("nil cal fallback: got %d, want 5", got)
	}
}

func TestSpotDateCal_WithHoliday(t *testing.T) {
	// FRI 6/5 거래일, T+2 영업일. 6/8 (월) 휴일.
	// 6/6 SAT, 6/7 SUN, 6/8 holiday → 모두 skip. 1번째 영업일 = 6/9 (화).
	// 2번째 영업일 = 6/10 (수). T+2 = 6/10.
	hd := mustDate("2026-06-08")
	cal := NewHolidayCalendar([]time.Time{hd})
	fri := time.Date(2026, 6, 5, 14, 0, 0, 0, time.UTC)
	got := SpotDateCal(fri, 2, cal)
	want := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("SPOT FRI+2 (월요일 휴일): got %s, want %s",
			got.Format("2006-01-02 Mon"), want.Format("2006-01-02 Mon"))
	}
}

// ─── ApplyForValueDate 가 PricingTable.Calendar 사용 확인 ──────────────────

// 휴일이 ApplyForValueDate 의 SPOT 계산 + 영업일 offset 에 정확히 반영됨.
// 거래일 6/1 (MON), 휴일 6/3 (WED). SPOT(T+2):
//
//	weekend-only → 6/3 (WED)
//	holiday-aware → 6/4 (THU, 6/3 skip)
//
// value_date 6/22 (MON) 까지:
//
//	from SPOT=6/3 → 6/22 영업일: 4,5,8,9,10,11,12,15,16,17,18,19,22 = 13.
//	from SPOT=6/4 → 6/22 영업일: 5,8,9,10,11,12,15,16,17,18,19,22 = 12.
//
// 따라서 offsetDays 가 cal 따라 달라짐.
func TestApplyForValueDate_UsesCalendar(t *testing.T) {
	body := []byte(`{
	  "version": 1,
	  "holidays": ["2026-06-03"],
	  "swap_point": [
	    {"pair": "USD/KRW", "tenor": "1W", "bid_amount": 0.05, "ask_amount": 0.07},
	    {"pair": "USD/KRW", "tenor": "1M", "bid_amount": 0.15, "ask_amount": 0.25}
	  ]
	}`)
	tbl, _ := ParsePricingTable(body)
	if tbl.Calendar == nil {
		t.Fatal("holiday 빌드 실패")
	}
	raw := Quote{Pair: "USD/KRW", Bid: 1300, Ask: 1300.04, TS: time.Now()}
	prof := session.Profile{}
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC) // MON 6/1
	vd := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)  // MON 6/22
	_, interp, err := tbl.ApplyForValueDate(raw, prof, vd, now, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if interp.OffsetDays != 12 {
		t.Errorf("offset (holiday-aware) = %d, want 12 (6/4 SPOT 기준)", interp.OffsetDays)
	}

	// 휴일 제거 (weekend-only) 비교.
	tbl2, _ := ParsePricingTable([]byte(`{
	  "version": 1,
	  "swap_point": [
	    {"pair": "USD/KRW", "tenor": "1W", "bid_amount": 0.05, "ask_amount": 0.07},
	    {"pair": "USD/KRW", "tenor": "1M", "bid_amount": 0.15, "ask_amount": 0.25}
	  ]
	}`))
	_, interp2, _ := tbl2.ApplyForValueDate(raw, prof, vd, now, 2, "")
	if interp2.OffsetDays != 13 {
		t.Errorf("offset (weekend-only) = %d, want 13 (6/3 SPOT 기준)", interp2.OffsetDays)
	}
}

// helpers ───────────────────────────────────────────────────────────────────

func mustDate(s string) time.Time {
	d, err := ParseHolidayDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// JSON round-trip 검증 — Doc 에 Holidays 가 보존되는지.
func TestPricingTableDoc_Holidays_RoundTrip(t *testing.T) {
	doc := PricingTableDoc{Version: 1, Holidays: []string{"2026-06-03", "2026-06-04"}}
	b, _ := json.Marshal(doc)
	var got PricingTableDoc
	_ = json.Unmarshal(b, &got)
	if len(got.Holidays) != 2 || got.Holidays[0] != "2026-06-03" {
		t.Errorf("roundtrip: %+v", got)
	}
}
