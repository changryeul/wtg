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
	resp, _ := http.Post(srv.URL, "application/json", bytes.NewReader(body))
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

	bodies := []ForwardLockRequest{
		{Tenor: "1M", Profile: "WEB.BRANCH.VIP"},                   // pair 누락
		{Pair: "USD/KRW", Profile: "WEB.BRANCH.VIP"},               // tenor 누락
		{Pair: "USD/KRW", Tenor: "1M"},                             // profile 누락
		{Pair: "USD/KRW", Tenor: "1M", Profile: "INVALID"},         // profile 형식
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
