package pricing

import (
	"encoding/json"
	"testing"
)

func TestApplySwapToDoc(t *testing.T) {
	seed := []byte(`{"version":3,"swap_point":[
		{"pair":"USD/KRW","tenor":"1M","bid_amount":0.1,"ask_amount":0.2,"skew_amount":0.05},
		{"pair":"EUR/USD","tenor":"1M","bid_amount":9,"ask_amount":9}]}`)

	out, err := ApplySwapToDoc(seed, "USD/KRW", []SwapUpdate{
		{Pair: "USD/KRW", Tenor: "1M", Bid: 0.15, Ask: 0.25}, // 기존 upsert
		{Pair: "USD/KRW", Tenor: "1W", Bid: 0.01, Ask: 0.02}, // 신규
	}, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var doc PricingTableDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("out 파싱: %v", err)
	}
	if doc.Version != 4 {
		t.Fatalf("version=%d, want 4", doc.Version)
	}
	if len(doc.SwapPoint) != 3 {
		t.Fatalf("entries=%d, want 3", len(doc.SwapPoint))
	}
	if doc.SwapPoint[0].BidAmount != 0.15 || doc.SwapPoint[0].SkewAmount != 0.05 {
		t.Fatalf("upsert 시 Skew 보존 실패: %+v", doc.SwapPoint[0])
	}

	// clear — USD/KRW 만 삭제, EUR/USD 잔존
	out2, err := ApplySwapToDoc(out, "USD/KRW", nil, true)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	doc = PricingTableDoc{}
	if err := json.Unmarshal(out2, &doc); err != nil {
		t.Fatalf("out2 파싱: %v", err)
	}
	if len(doc.SwapPoint) != 1 || string(doc.SwapPoint[0].Pair) != "EUR/USD" {
		t.Fatalf("clear 결과 불일치: %+v", doc.SwapPoint)
	}
}

func TestApplySwapToDoc_EmptySeed(t *testing.T) {
	out, err := ApplySwapToDoc(nil, "USD/KRW",
		[]SwapUpdate{{Pair: "USD/KRW", Tenor: "1M", Bid: 1, Ask: 2}}, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var doc PricingTableDoc
	_ = json.Unmarshal(out, &doc)
	if doc.Version != 1 || len(doc.SwapPoint) != 1 {
		t.Fatalf("빈 seed 시작 실패: %+v", doc)
	}
}
