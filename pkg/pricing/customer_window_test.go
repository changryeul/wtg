package pricing

import (
	"encoding/json"
	"testing"
	"time"
)

// P1 — schema 확장 단위 검증. Apply 통합은 Phase 2/3 에서.

func TestParsePricingTable_LoadsCustomerAndWindows(t *testing.T) {
	body := []byte(`{
	  "version": 7,
	  "time_windows": [
	    {"name":"regular","start":"09:00","end":"15:30","tz":"Asia/Seoul","days":"MON-FRI"},
	    {"name":"off_hours","complement_of":"regular"}
	  ],
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02,"window":"regular"}
	  ],
	  "customer_margin": [
	    {"customer_id":"VIP-7","pair":"USD/KRW","bid_delta":-0.01,"ask_delta":-0.01,
	     "mode":"add","priority":100,"window":"regular"},
	    {"customer_id":"GOLD-3","bid_delta":0.005,"ask_delta":0.005}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tbl.Version != 7 {
		t.Errorf("Version=%d, want 7", tbl.Version)
	}
	// time windows
	if _, ok := tbl.TimeWindows["regular"]; !ok {
		t.Errorf("regular window missing")
	}
	off, ok := tbl.TimeWindows["off_hours"]
	if !ok || off.ComplementOf != "regular" {
		t.Errorf("off_hours window invalid: %+v", off)
	}
	reg := tbl.TimeWindows["regular"]
	if reg.StartMin != 9*60 || reg.EndMin != 15*60+30 {
		t.Errorf("regular start/end: %d-%d, want 540-930", reg.StartMin, reg.EndMin)
	}
	// MON-FRI = bit 1,2,3,4,5 = 0b0111110 = 0x3E
	if reg.DaysMask != 0x3E {
		t.Errorf("MON-FRI mask: got 0x%02X, want 0x3E", reg.DaysMask)
	}
	// customer margin priority desc 정렬
	if len(tbl.CustomerMargin) != 2 {
		t.Fatalf("customer rules: %d, want 2", len(tbl.CustomerMargin))
	}
	if tbl.CustomerMargin[0].CustomerID != "VIP-7" {
		t.Errorf("first by priority: %s, want VIP-7", tbl.CustomerMargin[0].CustomerID)
	}
	if tbl.CustomerMargin[1].Mode != "add" {
		t.Errorf("default Mode: %s, want add", tbl.CustomerMargin[1].Mode)
	}
}

func TestTimeWindowRule_IsActive_BasicRange(t *testing.T) {
	w := TimeWindowRule{
		Name:     "regular",
		StartMin: 9 * 60,         // 09:00
		EndMin:   15*60 + 30,     // 15:30
		TZ:       "Asia/Seoul",
		DaysMask: 0x3E, // MON-FRI
	}
	all := map[string]TimeWindowRule{"regular": w}
	// 2026-06-01 (Monday) 10:00 KST — active
	mon10 := time.Date(2026, 6, 1, 10, 0, 0, 0, mustLoc("Asia/Seoul"))
	if !w.IsActive(mon10, all) {
		t.Errorf("MON 10:00 KST should be active")
	}
	// 2026-06-01 (Monday) 16:00 KST — outside range
	mon16 := time.Date(2026, 6, 1, 16, 0, 0, 0, mustLoc("Asia/Seoul"))
	if w.IsActive(mon16, all) {
		t.Errorf("MON 16:00 KST should be inactive (outside 09:00-15:30)")
	}
	// Sun 10:00 — outside days
	sun10 := time.Date(2026, 5, 31, 10, 0, 0, 0, mustLoc("Asia/Seoul"))
	if w.IsActive(sun10, all) {
		t.Errorf("SUN should be inactive (MON-FRI only)")
	}
}

func TestTimeWindowRule_IsActive_Complement(t *testing.T) {
	reg := TimeWindowRule{Name: "regular", StartMin: 9 * 60, EndMin: 15*60 + 30, TZ: "Asia/Seoul", DaysMask: 0x3E}
	off := TimeWindowRule{Name: "off_hours", ComplementOf: "regular"}
	all := map[string]TimeWindowRule{"regular": reg, "off_hours": off}

	mon10 := time.Date(2026, 6, 1, 10, 0, 0, 0, mustLoc("Asia/Seoul"))
	if off.IsActive(mon10, all) {
		t.Errorf("off_hours should be INACTIVE when regular ACTIVE")
	}
	mon16 := time.Date(2026, 6, 1, 16, 0, 0, 0, mustLoc("Asia/Seoul"))
	if !off.IsActive(mon16, all) {
		t.Errorf("off_hours should be ACTIVE when regular INACTIVE")
	}
}

func TestBuildPricingTable_BackwardCompat(t *testing.T) {
	// 기존 schema (customer_margin / time_windows 없음) 도 정상 빌드.
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
		},
	}
	tbl := BuildPricingTable(doc)
	if tbl.Version != 1 {
		t.Errorf("Version=%d", tbl.Version)
	}
	if len(tbl.CustomerMargin) != 0 {
		t.Errorf("CustomerMargin should be empty: %d", len(tbl.CustomerMargin))
	}
	if len(tbl.TimeWindows) != 0 {
		t.Errorf("TimeWindows should be empty: %d", len(tbl.TimeWindows))
	}
}

// JSON round-trip — DTO → bytes → DTO 동일.
func TestPricingTableDoc_RoundTrip(t *testing.T) {
	doc := PricingTableDoc{
		Version: 42,
		TimeWindows: []TimeWindowDoc{
			{Name: "regular", Start: "09:00", End: "15:30", TZ: "Asia/Seoul", Days: "MON-FRI"},
		},
		CustomerMargin: []CustomerEntryDoc{
			{CustomerID: "VIP-7", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100, Window: "regular"},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var got PricingTableDoc
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 42 || len(got.CustomerMargin) != 1 || len(got.TimeWindows) != 1 {
		t.Errorf("roundtrip: %+v", got)
	}
}

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}
