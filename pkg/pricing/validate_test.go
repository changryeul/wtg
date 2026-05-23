package pricing

import (
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestValidate_OK(t *testing.T) {
	doc := PricingTableDoc{
		Version: 1,
		SwapPoint: []SwapEntryDoc{
			{Pair: "USD/KRW", Tenor: "1M", BidAmount: -0.20, AskAmount: 0.30}, // 음수 swap 허용
		},
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: session.TierVIP, BidAmount: 0.02, AskAmount: 0.02},
			{Pair: "USD/KRW", Tier: "", BidAmount: 0.10, AskAmount: 0.10}, // wildcard tier
		},
		SiteMargin: []SiteEntryDoc{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch, BidAmount: 0.05, AskAmount: 0.05},
			{Pair: "USD/KRW", Channel: "", Site: session.SiteHQ, BidAmount: 0.01, AskAmount: 0.01}, // site-only fallback OK
		},
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("정상 doc 인데 위반: %v", err)
	}
}

func TestValidate_NegativeHQMargin(t *testing.T) {
	doc := PricingTableDoc{
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: session.TierVIP, BidAmount: -0.01, AskAmount: 0.02},
		},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("음수 bid_amount 인데 통과")
	}
	if !IsValidationError(err) {
		t.Errorf("ValidationError 가 아님: %T", err)
	}
	if !strings.Contains(err.Error(), "bid_amount") {
		t.Errorf("에러 메시지에 'bid_amount' 없음: %v", err)
	}
}

func TestValidate_NegativeSiteMargin(t *testing.T) {
	doc := PricingTableDoc{
		SiteMargin: []SiteEntryDoc{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch, BidAmount: 0.05, AskAmount: -0.01},
		},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("음수 ask_amount 인데 통과")
	}
	if !strings.Contains(err.Error(), "ask_amount") {
		t.Errorf("에러 메시지에 'ask_amount' 없음: %v", err)
	}
}

func TestValidate_SwapPointNegativeAllowed(t *testing.T) {
	doc := PricingTableDoc{
		SwapPoint: []SwapEntryDoc{
			{Pair: "USD/KRW", Tenor: "3M", BidAmount: -1.5, AskAmount: -0.8},
		},
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("swap_point 음수가 reject 됨: %v", err)
	}
}

func TestValidate_EmptyPair(t *testing.T) {
	doc := PricingTableDoc{
		HQMargin:   []HQEntryDoc{{Pair: "", Tier: "VIP", BidAmount: 0.1, AskAmount: 0.1}},
		SiteMargin: []SiteEntryDoc{{Pair: "", Channel: "WEB", Site: "BRANCH"}},
		SwapPoint:  []SwapEntryDoc{{Pair: "", Tenor: "1M"}},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("빈 pair 통과")
	}
	// 3건 위반 모두 포함되는지 (단일 에러로 모음).
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("ValidationError 아님: %T", err)
	}
	if len(ve.Violations) < 3 {
		t.Errorf("violations = %d, want >= 3 — 모든 위반 모아야: %v", len(ve.Violations), ve.Violations)
	}
}

func TestValidate_SwapMissingTenor(t *testing.T) {
	doc := PricingTableDoc{
		SwapPoint: []SwapEntryDoc{{Pair: "USD/KRW", Tenor: "", BidAmount: 0.1, AskAmount: 0.1}},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("tenor 누락 통과")
	}
}

func TestValidate_SiteWildcardBoth(t *testing.T) {
	doc := PricingTableDoc{
		SiteMargin: []SiteEntryDoc{
			{Pair: "USD/KRW", Channel: "", Site: "", BidAmount: 0.1, AskAmount: 0.1}, // 광범위 와일드카드
		},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("channel+site 모두 빈 와일드카드 통과")
	}
	if !strings.Contains(err.Error(), "와일드카드") {
		t.Errorf("'와일드카드' 메시지 없음: %v", err)
	}
}

func TestValidate_MultipleViolationsAggregated(t *testing.T) {
	doc := PricingTableDoc{
		HQMargin: []HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: -0.01, AskAmount: -0.02}, // 2건 위반
		},
		SiteMargin: []SiteEntryDoc{
			{Pair: "EUR/KRW", Channel: "", Site: "", BidAmount: 0.1, AskAmount: 0.1}, // 1건
		},
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("위반 다수 통과")
	}
	ve := err.(*ValidationError)
	if len(ve.Violations) < 3 {
		t.Errorf("위반 합산 = %d, want >= 3", len(ve.Violations))
	}
	if !strings.Contains(err.Error(), "3건") {
		t.Errorf("에러 메시지에 '3건' 누락 (다수 위반 표시): %v", err)
	}
}
