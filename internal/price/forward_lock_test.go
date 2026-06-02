package price

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quoteid"
)

// Phase 5 3단계 — forward/lock endpoint 단위 검증.

func newLockDeps(t *testing.T) ForwardLockDeps {
	t.Helper()
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})
	gen := quoteid.NewGenerator("T")
	reg := quoteid.NewMemoryRegistry(5 * time.Second)
	return ForwardLockDeps{
		Store: store, Best: best, Gen: gen, Reg: reg,
		Validity: 500 * time.Millisecond,
	}
}

func TestForwardLock_HappyPath_AliceCustomer(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", Tenor: "1M", Profile: "WEB.BRANCH.VIP",
		CustomerID: "alice", Side: "buy", Amount: 10000,
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got ForwardLockResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.QuoteID == "" {
		t.Errorf("quote_id 비어있음")
	}
	if got.Pair != "USD/KRW" || got.Tenor != "1M" {
		t.Errorf("응답 헤더: %+v", got)
	}
	// HQ 0.02 + Site 0.05 + alice -0.01 + swap 0.15/0.25 = bid 0.21 / ask 0.31
	if !near(got.RawBid-got.Bid, 0.21) {
		t.Errorf("bid 차이 = %v, want 0.21", got.RawBid-got.Bid)
	}
	if !near(got.Ask-got.RawAsk, 0.31) {
		t.Errorf("ask 차이 = %v, want 0.31", got.Ask-got.RawAsk)
	}
	if got.ValidUntilUnixNano <= got.IssuedUnixNano {
		t.Errorf("valid_until ≤ issued: %d ≤ %d", got.ValidUntilUnixNano, got.IssuedUnixNano)
	}

	// Registry 에 record 존재 확인.
	rec, err := deps.Reg.Get(context.Background(), quoteid.QuoteID(got.QuoteID))
	if err != nil {
		t.Fatalf("Registry.Get: %v", err)
	}
	if rec.Tenor != "1M" || string(rec.Pair) != "USD/KRW" {
		t.Errorf("Record: %+v", rec)
	}
	if !near(rec.Bid, got.Bid) || !near(rec.Ask, got.Ask) {
		t.Errorf("Record price drift: %+v vs response %+v", rec, got)
	}
}

func TestForwardLock_NoCustomer_ProfileOnly(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", Tenor: "1M", Profile: "WEB.BRANCH.VIP",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got ForwardLockResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	// Profile-only: HQ 0.02 + Site 0.05 + swap 0.15/0.25 = bid 0.22 / ask 0.32
	if !near(got.RawBid-got.Bid, 0.22) {
		t.Errorf("profile-only bid 차이 = %v, want 0.22", got.RawBid-got.Bid)
	}
	if !near(got.Ask-got.RawAsk, 0.32) {
		t.Errorf("profile-only ask 차이 = %v, want 0.32", got.Ask-got.RawAsk)
	}
}

func TestForwardLock_BadRequests(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()

	// P5 5단계 2차: tenor / value_date 둘 다 비면 SPOT 으로 fallback (의도된 동작) —
	// 그 case 는 더 이상 400 아님. pair / profile 자체 누락 + invalid profile 만 검증.
	bodies := []ForwardLockRequest{
		{Tenor: "1M", Profile: "WEB.BRANCH.VIP"},           // pair 누락
		{Pair: "USD/KRW", Tenor: "1M"},                     // profile 누락
		{Pair: "USD/KRW", Tenor: "1M", Profile: "INVALID"}, // profile 형식
	}
	for i, b := range bodies {
		body, _ := json.Marshal(b)
		resp, _ := http.Post(srv.URL, "application/json", bytes.NewReader(body))
		if resp.StatusCode != 400 {
			t.Errorf("case %d: status = %d, want 400", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestForwardLock_GET_MethodNotAllowed(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, false))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	if resp.StatusCode != 405 {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()
}

// P5 5단계 2차 — value_date 흐름. broken-date 보간.
func TestForwardLock_ValueDate_BrokenDate(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()

	// 픽스처의 swap_point: 1W(7), 1M(30), 3M(91). 보간을 위한 offsetDays=15
	// 가 되도록 value_date 계산해서 보낸다 — 단, 현재 시각이 매번 바뀌므로
	// "오늘로부터 충분히 멀고 보간 가능한 범위" 위주로 확인.
	// 가장 단순한 검증: value_date 가 1M(30 영업일) 와 일치하지 않으나
	// 보간 범위 안에 있어 broken-date 응답이 반환되는지 + Interpolation 필드 채워짐.
	now := time.Now()
	spot := pricing.SpotDate(now, 2)
	// SPOT + 15 영업일.
	vd := pricing.AddBusinessDays(spot, 15)
	body, _ := json.Marshal(ForwardLockRequest{
		Pair:       "USD/KRW",
		ValueDate:  vd.Format("2006-01-02"),
		Profile:    "WEB.BRANCH.VIP",
		CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got ForwardLockResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Interpolation == nil {
		t.Fatalf("Interpolation 필드 누락: %+v", got)
	}
	ip := got.Interpolation
	if ip.OffsetDays != 15 {
		t.Errorf("OffsetDays = %d, want 15", ip.OffsetDays)
	}
	// 픽스처는 swap 1W=0.05/0.07, 1M=0.15/0.25, 3M=0.40/0.55. 15일 → 1W~1M 사이.
	if ip.From != "1W" || ip.To != "1M" {
		t.Errorf("interpolation range: from=%s to=%s, want 1W~1M", ip.From, ip.To)
	}
	wantW := float64(15-7) / float64(30-7) // 8/23
	if !near(ip.Weight, wantW) {
		t.Errorf("weight = %v, want %v", ip.Weight, wantW)
	}
	// alice add: HQ(0.02)+Site(0.05)+cust(-0.01)+swap_bid(0.05+(0.15-0.05)*8/23) = 0.06 + 0.0848 = 0.1448
	wantSwapBid := 0.05 + (0.15-0.05)*wantW
	if !near(ip.SwapBid, wantSwapBid) {
		t.Errorf("swap_bid = %v, want %v", ip.SwapBid, wantSwapBid)
	}
	// 최종 bid 차이 = 0.06 + swap_bid 보간값.
	wantBidDiff := 0.06 + wantSwapBid
	if !near(got.RawBid-got.Bid, wantBidDiff) {
		t.Errorf("final bid 차이 = %v, want %v", got.RawBid-got.Bid, wantBidDiff)
	}

	// Registry 의 Record 에도 보간 정보 보존됐는지.
	rec, err := deps.Reg.Get(context.Background(), quoteid.QuoteID(got.QuoteID))
	if err != nil {
		t.Fatal(err)
	}
	if rec.OffsetDays != 15 || rec.InterpolatedFrom != "1W" || rec.InterpolatedTo != "1M" {
		t.Errorf("Record 보간정보 누락: %+v", rec)
	}
	if !near(rec.InterpolationWeight, wantW) {
		t.Errorf("Record weight = %v, want %v", rec.InterpolationWeight, wantW)
	}
}

// value_date 가 SPOT (offset=0) 일 때 — Exact 매칭이지만 swap 0/0 미등록 → out of range.
// 픽스처에는 SPOT swap 없으니 1W 부터. offset=0 (value=spot) 보내면 out of range.
func TestForwardLock_ValueDate_OutOfRange_TooSmall(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()
	now := time.Now()
	spot := pricing.SpotDate(now, 2)
	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", ValueDate: spot.Format("2006-01-02"),
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (out of range)", resp.StatusCode)
	}
}

// value_date 가 standard tenor 와 정확 일치 → Exact 응답.
func TestForwardLock_ValueDate_Exact(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()
	now := time.Now()
	spot := pricing.SpotDate(now, 2)
	vd := pricing.AddBusinessDays(spot, 30) // 1M
	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", ValueDate: vd.Format("2006-01-02"),
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got ForwardLockResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Interpolation == nil || !got.Interpolation.Exact {
		t.Fatalf("Exact 매칭 안 됨: %+v", got.Interpolation)
	}
	if got.Tenor != "1M" {
		t.Errorf("Exact 시 Tenor = %q, want 1M", got.Tenor)
	}
}

// invalid value_date 형식 → 400.
func TestForwardLock_ValueDate_BadFormat(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()
	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", ValueDate: "not-a-date",
		Profile: "WEB.BRANCH.VIP",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestForwardLock_MarkConsumedFlow(t *testing.T) {
	deps := newLockDeps(t)
	srv := httptest.NewServer(ForwardLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(ForwardLockRequest{
		Pair: "USD/KRW", Tenor: "1M", Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, _ := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	var got ForwardLockResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()

	// 첫 MarkConsumed — OK.
	r1, err := deps.Reg.MarkConsumed(context.Background(), quoteid.QuoteID(got.QuoteID), "engine-1")
	if err != nil || r1.Status != quoteid.ConsumeOK {
		t.Fatalf("first MarkConsumed: %v outcome=%v", err, r1.Status)
	}
	// 두 번째 — AlreadyDone.
	r2, _ := deps.Reg.MarkConsumed(context.Background(), quoteid.QuoteID(got.QuoteID), "engine-2")
	if r2.Status != quoteid.ConsumeAlreadyDone {
		t.Errorf("second MarkConsumed: outcome = %v, want ConsumeAlreadyDone", r2.Status)
	}
	if r2.ConsumedBy != "engine-1" {
		t.Errorf("ConsumedBy = %q, want engine-1", r2.ConsumedBy)
	}
}
