package pricing

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestParsePricingTable_BasicRoundTrip(t *testing.T) {
	body := []byte(`{
		"version": 42,
		"hq_margin": [
			{"pair":"USD/KRW","tier":"STD","bid_amount":0.10,"ask_amount":0.10},
			{"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}
		],
		"site_margin": [
			{"pair":"USD/KRW","channel":"WEB","site":"BRANCH","bid_amount":0.05,"ask_amount":0.05}
		],
		"swap_point": [
			{"pair":"USD/KRW","tenor":"1M","bid_amount":-0.20,"ask_amount":0.30}
		]
	}`)

	tbl, err := ParsePricingTable(body)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Version != 42 {
		t.Errorf("Version = %d", tbl.Version)
	}

	if m := tbl.lookupHQ("USD/KRW", session.TierVIP); m.BidAmount != 0.02 {
		t.Errorf("HQ VIP bid = %v", m.BidAmount)
	}
	if m := tbl.lookupSite("USD/KRW", session.ChannelWeb, session.SiteBranch); m.BidAmount != 0.05 {
		t.Errorf("Site WEB.BRANCH bid = %v", m.BidAmount)
	}
	if m := tbl.lookupSwap("USD/KRW", Tenor1M); m.BidAmount != -0.20 {
		t.Errorf("Swap 1M bid = %v", m.BidAmount)
	}
}

func TestParsePricingTable_Empty(t *testing.T) {
	tbl, err := ParsePricingTable([]byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Version != 1 || len(tbl.HQMargin) != 0 || len(tbl.SiteMargin) != 0 || len(tbl.SwapPoint) != 0 {
		t.Errorf("empty doc 결과: %+v", tbl)
	}
}

func TestParsePricingTable_Malformed(t *testing.T) {
	if _, err := ParsePricingTable([]byte(`not json`)); err == nil {
		t.Error("malformed JSON 통과")
	}
}
