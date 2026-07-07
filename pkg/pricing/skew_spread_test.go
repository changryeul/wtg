package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// approxEqSS — float 비교 헬퍼 (epsilon 0.0001). skew/spread 테스트 전용
// (customer_window_test.go 의 approxEq 와 중복 회피).
func approxEqSS(a, b float64) bool {
	return math.Abs(a-b) < 0.0001
}

// TestApply_SkewOnly — HQ 의 SkewAmount=+0.05 가 양쪽 bid/ask 위로 5pip shift.
// 기존 BidAmount/AskAmount 는 그대로 widen 방향, skew 는 동방향 shift.
func TestApply_SkewOnly(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02, SkewAmount: 0.05},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.Apply(raw, profile, TenorSpot)
	// 산식: bid = 1300 + 0.05 - 0.02 = 1300.03
	//       ask = 1300.10 + 0.05 + 0.02 = 1300.17
	if !approxEqSS(cq.Bid, 1300.03) {
		t.Errorf("bid: got %.4f, want 1300.0300", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.17) {
		t.Errorf("ask: got %.4f, want 1300.1700", cq.Ask)
	}
	if !approxEqSS(cq.RawBid, 1300.00) || !approxEqSS(cq.RawAsk, 1300.10) {
		t.Errorf("raw 보존 깨짐: bid=%.4f ask=%.4f", cq.RawBid, cq.RawAsk)
	}
}

// TestApply_SpreadOnly — SpreadAmount=0.10 → 양쪽 절반씩 (0.05) widen.
func TestApply_SpreadOnly(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02, SpreadAmount: 0.10},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.Apply(raw, profile, TenorSpot)
	// 산식: bid = 1300 - 0.02 - 0.05 = 1299.93
	//       ask = 1300.10 + 0.02 + 0.05 = 1300.17
	if !approxEqSS(cq.Bid, 1299.93) {
		t.Errorf("bid: got %.4f, want 1299.9300", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.17) {
		t.Errorf("ask: got %.4f, want 1300.1700", cq.Ask)
	}
}

// TestApply_SkewAndSpread_Cumulative — HQ + Site 의 Skew/Spread 가 누계.
func TestApply_SkewAndSpread_Cumulative(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02,
				SkewAmount: 0.03, SpreadAmount: 0.04},
		},
		SiteMargin: []SiteEntryDoc{
			{Pair: "USD/KRW", Channel: "WEB", Site: "BRANCH", BidAmount: 0.01, AskAmount: 0.01,
				SkewAmount: 0.02, SpreadAmount: 0.06},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.Apply(raw, profile, TenorSpot)
	// 누계: BidAmount=0.03, AskAmount=0.03, Skew=0.05, Spread=0.10 → spreadHalf=0.05
	// bid = 1300 + 0.05 - 0.03 - 0.05 = 1299.97
	// ask = 1300.10 + 0.05 + 0.03 + 0.05 = 1300.23
	if !approxEqSS(cq.Bid, 1299.97) {
		t.Errorf("bid: got %.4f, want 1299.9700", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.23) {
		t.Errorf("ask: got %.4f, want 1300.2300", cq.Ask)
	}
}

// TestApply_ZeroSkewSpread_BackwardCompat — Skew/Spread 가 0 이면 기존 산식과 정확히 동일.
func TestApply_ZeroSkewSpread_BackwardCompat(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
		},
		SiteMargin: []SiteEntryDoc{
			{Pair: "USD/KRW", Channel: "WEB", Site: "BRANCH", BidAmount: 0.05, AskAmount: 0.05},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.Apply(raw, profile, TenorSpot)
	// bid = 1300 - (0.02 + 0.05) = 1299.93
	// ask = 1300.10 + (0.02 + 0.05) = 1300.17
	if !approxEqSS(cq.Bid, 1299.93) {
		t.Errorf("backward compat 깨짐 — bid: got %.4f, want 1299.9300", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.17) {
		t.Errorf("backward compat 깨짐 — ask: got %.4f, want 1300.1700", cq.Ask)
	}
}

// TestApply_NegativeSkew — Skew 음수 (양쪽 아래로 shift) — USD 매도 인벤토리 헷지.
func TestApply_NegativeSkew(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02, SkewAmount: -0.10},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.Apply(raw, profile, TenorSpot)
	// bid = 1300 - 0.10 - 0.02 = 1299.88
	// ask = 1300.10 - 0.10 + 0.02 = 1300.02
	if !approxEqSS(cq.Bid, 1299.88) {
		t.Errorf("bid: got %.4f, want 1299.8800", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.02) {
		t.Errorf("ask: got %.4f, want 1300.0200", cq.Ask)
	}
}

// TestApply_CustomerSkewSpread_Add — customer 가 add 모드로 SkewDelta/SpreadDelta 추가.
func TestApply_CustomerSkewSpread_Add(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
		},
		CustomerMargin: []CustomerEntryDoc{
			{CustomerID: "CUST001", Pair: "USD/KRW",
				BidDelta: -0.01, AskDelta: -0.01, // VIP 할인
				SkewDelta: 0.03, SpreadDelta: 0.04, Mode: "add"},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.ApplyForCustomer(raw, profile, TenorSpot,
		time.Time{}, "CUST001")
	// HQ 0.02 + Customer -0.01 = 0.01 (BidAmount/AskAmount)
	// Skew 0 + 0.03 = 0.03, Spread 0 + 0.04 = 0.04 → spreadHalf=0.02
	// bid = 1300 + 0.03 - 0.01 - 0.02 = 1300.00
	// ask = 1300.10 + 0.03 + 0.01 + 0.02 = 1300.16
	if !approxEqSS(cq.Bid, 1300.00) {
		t.Errorf("bid: got %.4f, want 1300.0000", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.16) {
		t.Errorf("ask: got %.4f, want 1300.1600", cq.Ask)
	}
}

// TestApply_CustomerSkewSpread_Override — override 모드는 HQ/Site 무시 + customer 단독.
// swap 의 Skew/Spread 는 살아남음 (만기 cost 는 독립).
func TestApply_CustomerSkewSpread_Override(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		SwapPoint: []SwapEntryDoc{
			{Pair: "USD/KRW", Tenor: "SPOT", BidAmount: 0.01, AskAmount: 0.01,
				SkewAmount: 0.02, SpreadAmount: 0.02},
		},
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.99, AskAmount: 0.99,
				SkewAmount: 0.99, SpreadAmount: 0.99}, // override 시 무시되어야 함
		},
		CustomerMargin: []CustomerEntryDoc{
			{CustomerID: "VIP01", Pair: "USD/KRW",
				BidDelta: 0.05, AskDelta: 0.05,
				SkewDelta: 0.03, SpreadDelta: 0.04, Mode: "override"},
		},
	}
	table := BuildPricingTable(doc)
	raw := Quote{Pair: "USD/KRW", Bid: 1300.00, Ask: 1300.10}
	profile := session.Profile{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}

	cq := table.ApplyForCustomer(raw, profile, TenorSpot,
		time.Time{}, "VIP01")
	// override: swap(0.01,0.01,0.02,0.02) + customer(0.05,0.05,0.03,0.04) → HQ 무시
	// BidAmount=0.06, AskAmount=0.06, Skew=0.05, Spread=0.06 → spreadHalf=0.03
	// bid = 1300 + 0.05 - 0.06 - 0.03 = 1299.96
	// ask = 1300.10 + 0.05 + 0.06 + 0.03 = 1300.24
	if !approxEqSS(cq.Bid, 1299.96) {
		t.Errorf("bid: got %.4f, want 1299.9600", cq.Bid)
	}
	if !approxEqSS(cq.Ask, 1300.24) {
		t.Errorf("ask: got %.4f, want 1300.2400", cq.Ask)
	}
}
