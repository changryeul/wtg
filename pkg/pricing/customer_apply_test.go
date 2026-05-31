package pricing

import (
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 3 — ApplyForCustomer 시나리오 검증.
//
// 공통 픽스처:
//   - USD/KRW HQ VIP bid_amount=0.02 / ask_amount=0.02 (window 무관)
//   - USD/KRW Site WEB.BRANCH bid_amount=0.03 / ask_amount=0.03
//   - VIP-7 customer: mode=add, bid_delta=-0.01, ask_delta=-0.01 (VIP 할인 = 마진 축소)
//   - GOLD-3 customer: mode=override, bid_delta=0.005, ask_delta=0.005 (HQ/Site 무시 단독)

func newP3Table(t *testing.T) *PricingTable {
	t.Helper()
	body := []byte(`{
	  "version": 33,
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}
	  ],
	  "site_margin": [
	    {"pair":"USD/KRW","channel":"WEB","site":"BRANCH","bid_amount":0.03,"ask_amount":0.03}
	  ],
	  "customer_margin": [
	    {"customer_id":"VIP-7","pair":"USD/KRW","bid_delta":-0.01,"ask_delta":-0.01,"mode":"add","priority":10},
	    {"customer_id":"GOLD-3","pair":"USD/KRW","bid_delta":0.005,"ask_delta":0.005,"mode":"override","priority":10}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return tbl
}

func p3RawAndProfile() (Quote, session.Profile) {
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	prof := session.Profile{Tier: session.TierVIP, Channel: session.ChannelWeb, Site: session.SiteBranch}
	return raw, prof
}

// add 모드 — HQ(0.02) + Site(0.03) + customer(-0.01) = 0.04
// bid 차이 = 0.04, ask 차이 = 0.04 (대칭 픽스처)
func TestApplyForCustomer_AddMode_AccumulatesDelta(t *testing.T) {
	tbl := newP3Table(t)
	raw, prof := p3RawAndProfile()

	cq := tbl.ApplyForCustomer(raw, prof, TenorSpot, time.Now(), "VIP-7")
	if got := raw.Bid - cq.Bid; !approxEq(got, 0.04) {
		t.Errorf("VIP-7 add mode bid 차이 = %v, want 0.04", got)
	}
	if got := cq.Ask - raw.Ask; !approxEq(got, 0.04) {
		t.Errorf("VIP-7 add mode ask 차이 = %v, want 0.04", got)
	}
}

// override 모드 — HQ/Site 무시. swap=0 환경이므로 customer delta 단독.
// bid 차이 = 0.005
func TestApplyForCustomer_OverrideMode_IgnoresHQSite(t *testing.T) {
	tbl := newP3Table(t)
	raw, prof := p3RawAndProfile()

	cq := tbl.ApplyForCustomer(raw, prof, TenorSpot, time.Now(), "GOLD-3")
	if got := raw.Bid - cq.Bid; !approxEq(got, 0.005) {
		t.Errorf("GOLD-3 override bid 차이 = %v, want 0.005 (HQ/Site 무시)", got)
	}
}

// customer 미매칭 — ApplyAt 와 동일 결과 (HQ + Site).
func TestApplyForCustomer_NoMatch_FallsBackToApplyAt(t *testing.T) {
	tbl := newP3Table(t)
	raw, prof := p3RawAndProfile()
	now := time.Now()

	cqFor := tbl.ApplyForCustomer(raw, prof, TenorSpot, now, "UNKNOWN-CUST")
	cqAt := tbl.ApplyAt(raw, prof, TenorSpot, now)
	if cqFor.Bid != cqAt.Bid || cqFor.Ask != cqAt.Ask {
		t.Errorf("미매칭은 ApplyAt 와 동일해야: %+v vs %+v", cqFor, cqAt)
	}
}

// customerID="" — 빈 문자열은 무조건 매칭 X (ApplyAt 와 동일).
func TestApplyForCustomer_EmptyID_NoCustomerMatch(t *testing.T) {
	tbl := newP3Table(t)
	raw, prof := p3RawAndProfile()
	now := time.Now()

	cqEmpty := tbl.ApplyForCustomer(raw, prof, TenorSpot, now, "")
	cqAt := tbl.ApplyAt(raw, prof, TenorSpot, now)
	if cqEmpty.Bid != cqAt.Bid || cqEmpty.Ask != cqAt.Ask {
		t.Errorf("customerID=\"\" 는 ApplyAt 와 동일해야: %+v vs %+v", cqEmpty, cqAt)
	}
}

// priority desc — 같은 customer 의 여러 entry 중 priority 큰 게 먼저 매칭.
// VIP-7 의 add (-0.01, p=10) vs add (-0.02, p=100) → priority 100 이 선택.
func TestApplyForCustomer_PriorityOrder(t *testing.T) {
	body := []byte(`{
	  "version": 34,
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}
	  ],
	  "customer_margin": [
	    {"customer_id":"VIP-7","pair":"USD/KRW","bid_delta":-0.01,"ask_delta":-0.01,"mode":"add","priority":10},
	    {"customer_id":"VIP-7","pair":"USD/KRW","bid_delta":-0.02,"ask_delta":-0.02,"mode":"add","priority":100}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	prof := session.Profile{Tier: session.TierVIP}

	cq := tbl.ApplyForCustomer(raw, prof, TenorSpot, time.Now(), "VIP-7")
	// priority 100 → -0.02 + HQ 0.02 = 0.00 차이
	if got := raw.Bid - cq.Bid; !approxEq(got, 0.0) {
		t.Errorf("priority 100 entry 선택 실패: bid 차이 = %v, want 0.0", got)
	}
}

// window 매칭 — customer entry 의 window 가 활성일 때만 적용.
// regular (MON-FRI 09:00-15:30 KST) 에 한정된 customer entry 가 MON 10:00 에 적용,
// MON 18:00 에는 비활성 (= ApplyAt 결과).
func TestApplyForCustomer_WindowFilter(t *testing.T) {
	body := []byte(`{
	  "version": 35,
	  "time_windows": [
	    {"name":"regular","start":"09:00","end":"15:30","tz":"Asia/Seoul","days":"MON-FRI"}
	  ],
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}
	  ],
	  "customer_margin": [
	    {"customer_id":"VIP-7","pair":"USD/KRW","bid_delta":-0.01,"ask_delta":-0.01,"mode":"add","window":"regular"}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	prof := session.Profile{Tier: session.TierVIP}

	mon10 := time.Date(2026, 6, 1, 10, 0, 0, 0, mustLoc("Asia/Seoul"))
	cqActive := tbl.ApplyForCustomer(raw, prof, TenorSpot, mon10, "VIP-7")
	if got := raw.Bid - cqActive.Bid; !approxEq(got, 0.01) {
		t.Errorf("regular window 활성: bid 차이 = %v, want 0.01 (HQ 0.02 - cust 0.01)", got)
	}

	mon18 := time.Date(2026, 6, 1, 18, 0, 0, 0, mustLoc("Asia/Seoul"))
	cqInactive := tbl.ApplyForCustomer(raw, prof, TenorSpot, mon18, "VIP-7")
	if got := raw.Bid - cqInactive.Bid; !approxEq(got, 0.02) {
		t.Errorf("regular window 비활성: bid 차이 = %v, want 0.02 (HQ 단독, customer 미적용)", got)
	}
}

// pair="" 와일드카드 customer — 모든 pair 에 매칭.
func TestApplyForCustomer_PairWildcard(t *testing.T) {
	body := []byte(`{
	  "version": 36,
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02},
	    {"pair":"EUR/KRW","tier":"VIP","bid_amount":0.04,"ask_amount":0.04}
	  ],
	  "customer_margin": [
	    {"customer_id":"VIP-7","bid_delta":-0.01,"ask_delta":-0.01,"mode":"add"}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	prof := session.Profile{Tier: session.TierVIP}

	// USD/KRW
	usd := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	cqU := tbl.ApplyForCustomer(usd, prof, TenorSpot, time.Now(), "VIP-7")
	if got := usd.Bid - cqU.Bid; !approxEq(got, 0.01) {
		t.Errorf("USD/KRW 와일드카드 cust: bid 차이 = %v, want 0.01", got)
	}
	// EUR/KRW — 동일 customer entry 가 와일드카드로 적용
	eur := Quote{Pair: "EUR/KRW", Bid: 1450.00, Ask: 1450.10}
	cqE := tbl.ApplyForCustomer(eur, prof, TenorSpot, time.Now(), "VIP-7")
	if got := eur.Bid - cqE.Bid; !approxEq(got, 0.03) {
		t.Errorf("EUR/KRW 와일드카드 cust: bid 차이 = %v, want 0.03 (HQ 0.04 - cust 0.01)", got)
	}
}

// override + window 조합 — override entry 가 window 활성일 때만 동작,
// 비활성이면 HQ+Site fallback.
func TestApplyForCustomer_OverrideWithWindow(t *testing.T) {
	body := []byte(`{
	  "version": 37,
	  "time_windows": [
	    {"name":"regular","start":"09:00","end":"15:30","tz":"Asia/Seoul","days":"MON-FRI"}
	  ],
	  "hq_margin": [
	    {"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}
	  ],
	  "customer_margin": [
	    {"customer_id":"GOLD-3","pair":"USD/KRW","bid_delta":0.005,"ask_delta":0.005,"mode":"override","window":"regular"}
	  ]
	}`)
	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	prof := session.Profile{Tier: session.TierVIP}

	mon10 := time.Date(2026, 6, 1, 10, 0, 0, 0, mustLoc("Asia/Seoul"))
	cqOn := tbl.ApplyForCustomer(raw, prof, TenorSpot, mon10, "GOLD-3")
	if got := raw.Bid - cqOn.Bid; !approxEq(got, 0.005) {
		t.Errorf("override + window 활성: bid 차이 = %v, want 0.005 (HQ 무시)", got)
	}

	mon18 := time.Date(2026, 6, 1, 18, 0, 0, 0, mustLoc("Asia/Seoul"))
	cqOff := tbl.ApplyForCustomer(raw, prof, TenorSpot, mon18, "GOLD-3")
	if got := raw.Bid - cqOff.Bid; !approxEq(got, 0.02) {
		t.Errorf("override + window 비활성: bid 차이 = %v, want 0.02 (HQ 적용)", got)
	}
}
