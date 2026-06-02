package price

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 5 — forward snapshot endpoint 검증.
//
// 픽스처: USD/KRW HQ(VIP) 0.02, Site(WEB.BRANCH) 0.05.
//        Swap: 1W 0.05/0.07, 1M 0.15/0.25, 3M 0.40/0.55.
//        Customer alice (add -0.01) — pair=USD/KRW.
//
// BEST raw: bid 1378.65 / ask 1378.69.
//
// SPOT customer-applied (alice 매칭 시): bid -0.06 (HQ+Site+cust), ask +0.06
//   → 1378.59 / 1378.75
// 1M forward (swap 더해짐): bid -0.06 - 0.15 = -0.21, ask +0.06 + 0.25 = +0.31
//   → 1378.44 / 1378.99

func newForwardTestSetup(t *testing.T, customerEntries []pricing.CustomerRule) (*pricing.Store, *BestConsumer) {
	t.Helper()
	tbl := &pricing.PricingTable{
		Version: 11,
		SwapPoint: map[pricing.SwapKey]pricing.Margin{
			{Pair: "USD/KRW", Tenor: "1W"}: {BidAmount: 0.05, AskAmount: 0.07},
			{Pair: "USD/KRW", Tenor: "1M"}: {BidAmount: 0.15, AskAmount: 0.25},
			{Pair: "USD/KRW", Tenor: "3M"}: {BidAmount: 0.40, AskAmount: 0.55},
		},
		HQMargin: map[pricing.HQKey]pricing.Margin{
			{Pair: "USD/KRW", Tier: session.TierVIP}: {BidAmount: 0.02, AskAmount: 0.02},
		},
		SiteMargin: map[pricing.SiteKey]pricing.Margin{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch}: {BidAmount: 0.05, AskAmount: 0.05},
		},
		CustomerMargin: customerEntries,
	}
	store := pricing.NewStore()
	store.Replace(tbl)

	best := NewBestConsumer(BestOptions{Logger: quietLogger(), MaxStaleness: 30 * time.Second})
	// USD/KRW 의 BEST 가 1378.65/1378.69 가 되도록 한 source 의 tick 주입.
	// BestConsumer.OnTick 은 Tick.Source 필드를 요구.
	best.OnTick(&Tick{
		Symbol:   "USDKRW",
		Source:   "SMB",
		Body:     []byte(`{"sym":"USDKRW","bid":1378.65,"ask":1378.69,"src":"SMB","ts":"` + time.Now().Format(time.RFC3339Nano) + `"}`),
		Received: time.Now(),
	})
	return store, best
}

func TestForwardSnapshot_AllTenors(t *testing.T) {
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})

	handler := ForwardSnapshotHandler(ForwardSnapshotDeps{Store: store, Best: best}, true)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?pair=USD/KRW&profile=WEB.BRANCH.VIP&customer_id=alice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got ForwardSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	// CORS 헤더.
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("CORS 헤더 누락")
	}

	if got.Pair != "USD/KRW" || got.Profile != "WEB.BRANCH.VIP" || got.CustomerID != "alice" {
		t.Errorf("응답 헤더 필드: %+v", got)
	}
	if got.TableVersion != 11 {
		t.Errorf("TableVersion = %d", got.TableVersion)
	}

	// SPOT raw + customer-applied.
	if !near(got.Spot.RawBid, 1378.65) || !near(got.Spot.RawAsk, 1378.69) {
		t.Errorf("SPOT raw: %+v", got.Spot)
	}
	// HQ 0.02 + Site 0.05 + cust -0.01 = 0.06
	if !near(got.Spot.RawBid-got.Spot.Bid, 0.06) {
		t.Errorf("SPOT customer bid 차이 = %v, want 0.06", got.Spot.RawBid-got.Spot.Bid)
	}
	if !near(got.Spot.Ask-got.Spot.RawAsk, 0.06) {
		t.Errorf("SPOT customer ask 차이 = %v, want 0.06", got.Spot.Ask-got.Spot.RawAsk)
	}

	// 3개 tenor 모두 정렬 순서로.
	if len(got.Tenors) != 3 {
		t.Fatalf("tenors len = %d, want 3", len(got.Tenors))
	}
	wantOrder := []string{"1M", "1W", "3M"} // 문자열 sort 결과
	for i, tn := range got.Tenors {
		if tn.Tenor != wantOrder[i] {
			t.Errorf("tenor[%d] = %q, want %q", i, tn.Tenor, wantOrder[i])
		}
	}

	// 1M 검증 — swap 0.15/0.25 + SPOT customer (-0.06/+0.06) = -0.21/+0.31
	var oneM ForwardSnapshotTenor
	for _, tn := range got.Tenors {
		if tn.Tenor == "1M" {
			oneM = tn
		}
	}
	if !near(oneM.SwapBid, 0.15) || !near(oneM.SwapAsk, 0.25) {
		t.Errorf("1M swap: %+v", oneM)
	}
	if !near(got.Spot.RawBid-oneM.Bid, 0.21) {
		t.Errorf("1M total bid 차이 = %v, want 0.21", got.Spot.RawBid-oneM.Bid)
	}
	if !near(oneM.Ask-got.Spot.RawAsk, 0.31) {
		t.Errorf("1M total ask 차이 = %v, want 0.31", oneM.Ask-got.Spot.RawAsk)
	}
}

// customer 미매칭 — Profile-only (HQ+Site = 0.07) 로 떨어지는지.
func TestForwardSnapshot_NoCustomerMatch(t *testing.T) {
	store, best := newForwardTestSetup(t, nil)
	handler := ForwardSnapshotHandler(ForwardSnapshotDeps{Store: store, Best: best}, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?pair=USD/KRW&profile=WEB.BRANCH.VIP")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got ForwardSnapshot
	_ = json.NewDecoder(resp.Body).Decode(&got)

	// DevMode=false → CORS 헤더 X.
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("non-dev 에서 CORS 노출됨")
	}
	if !near(got.Spot.RawBid-got.Spot.Bid, 0.07) {
		t.Errorf("Profile-only SPOT bid 차이 = %v, want 0.07", got.Spot.RawBid-got.Spot.Bid)
	}
}

// 빈 pair / invalid profile → 400.
func TestForwardSnapshot_BadRequest(t *testing.T) {
	store, best := newForwardTestSetup(t, nil)
	handler := ForwardSnapshotHandler(ForwardSnapshotDeps{Store: store, Best: best}, true)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	cases := []string{
		"?profile=WEB.BRANCH.VIP",       // pair 누락
		"?pair=USD/KRW",                 // profile 누락
		"?pair=USD/KRW&profile=INVALID", // profile token 수 != 3
	}
	for _, q := range cases {
		resp, _ := http.Get(srv.URL + q)
		if resp.StatusCode != 400 {
			t.Errorf("query=%q: status = %d, want 400", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// 미등록 pair → 404 (BEST snapshot 없음).
func TestForwardSnapshot_UnknownPair(t *testing.T) {
	store, best := newForwardTestSetup(t, nil)
	handler := ForwardSnapshotHandler(ForwardSnapshotDeps{Store: store, Best: best}, true)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "?pair=ZZZ/KRW&profile=WEB.BRANCH.VIP")
	if resp.StatusCode != 404 {
		t.Errorf("unknown pair: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
