package price

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quoteid"
)

// ---- S3-a: input validation ----

func TestValidateSwapLegs(t *testing.T) {
	tests := []struct {
		name    string
		near    SwapLeg
		far     SwapLeg
		wantErr error
	}{
		{"정상 — SPOT vs 1M", SwapLeg{Tenor: "SPOT"}, SwapLeg{Tenor: "1M"}, nil},
		{"정상 — 1W vs 1M (forward-forward)", SwapLeg{Tenor: "1W"}, SwapLeg{Tenor: "1M"}, nil},
		{"정상 — value_date 양쪽", SwapLeg{ValueDate: "2026-06-15"}, SwapLeg{ValueDate: "2026-07-15"}, nil},
		{"near 비어있음", SwapLeg{}, SwapLeg{Tenor: "1M"}, errSwapLegMissing},
		{"far 비어있음", SwapLeg{Tenor: "SPOT"}, SwapLeg{}, errSwapLegMissing},
		{"동일 tenor", SwapLeg{Tenor: "1M"}, SwapLeg{Tenor: "1M"}, errSwapSameLeg},
		{"동일 value_date", SwapLeg{ValueDate: "2026-06-15"}, SwapLeg{ValueDate: "2026-06-15"}, errSwapSameLeg},
		{"value_date 역전", SwapLeg{ValueDate: "2026-07-15"}, SwapLeg{ValueDate: "2026-06-15"}, errSwapLegInvert},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSwapLegs(tc.near, tc.far, 2)
			if !errors.Is(err, tc.wantErr) && err != tc.wantErr {
				if tc.wantErr == nil || err == nil || err.Error() != tc.wantErr.Error() {
					t.Fatalf("기대=%v 실제=%v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestLegKind(t *testing.T) {
	cases := []struct {
		in   SwapLeg
		want legKindEnum
	}{
		{SwapLeg{}, legKindMissing},
		{SwapLeg{Tenor: "SPOT"}, legKindTenor},
		{SwapLeg{ValueDate: "2026-06-15"}, legKindValueDate},
		{SwapLeg{Tenor: "1M", ValueDate: "2026-06-15"}, legKindValueDate},
	}
	for _, c := range cases {
		if got := legKind(c.in); got != c.want {
			t.Errorf("legKind(%+v): 기대=%d 실제=%d", c.in, c.want, got)
		}
	}
}

// ---- S3-b: handler happy-path + partial-failure revoke ----

func newSwapDeps(t *testing.T, metrics SwapLockMetrics) (SwapLockDeps, *quoteid.MemoryRegistry) {
	t.Helper()
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})
	gen := quoteid.NewGenerator("T")
	reg := quoteid.NewMemoryRegistry(5 * time.Second)
	return SwapLockDeps{
		Store: store, Best: best, Gen: gen, Reg: reg, Idx: reg,
		Validity: 500 * time.Millisecond, PutTimeout: 200 * time.Millisecond,
		SpotDays: 2, Metrics: metrics,
	}, reg
}

func TestSwapLock_HappyPath(t *testing.T) {
	metrics := &AtomicSwapLockMetrics{}
	deps, reg := newSwapDeps(t, metrics)
	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice", Side: "buy_sell", Amount: 10000,
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got SwapLockResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.SwapID == "" || got.Near.QuoteID == "" || got.Far.QuoteID == "" {
		t.Fatalf("ID 비어있음: %+v", got)
	}
	if got.Near.RawBid != got.Far.RawBid || got.Near.RawAsk != got.Far.RawAsk {
		t.Errorf("두 leg 가 동일 raw 를 봐야 함: near=%v/%v far=%v/%v",
			got.Near.RawBid, got.Near.RawAsk, got.Far.RawBid, got.Far.RawAsk)
	}
	// Registry 에 두 leg + swap_id 인덱스 모두 존재해야.
	if _, err := reg.Get(context.Background(), quoteid.QuoteID(got.Near.QuoteID)); err != nil {
		t.Errorf("near record 미존재: %v", err)
	}
	if _, err := reg.Get(context.Background(), quoteid.QuoteID(got.Far.QuoteID)); err != nil {
		t.Errorf("far record 미존재: %v", err)
	}
	sw, err := reg.GetSwap(context.Background(), got.SwapID)
	if err != nil {
		t.Fatalf("swap 인덱스 미존재: %v", err)
	}
	if sw.NearID != quoteid.QuoteID(got.Near.QuoteID) || sw.FarID != quoteid.QuoteID(got.Far.QuoteID) {
		t.Errorf("swap 인덱스 mismatch: %+v vs near=%s far=%s", sw, got.Near.QuoteID, got.Far.QuoteID)
	}
	// 메트릭.
	snap := metrics.Snapshot()
	if snap.Requests != 1 || snap.Successes != 1 {
		t.Errorf("metrics: %+v", snap)
	}
}

// failingRegistry — Put 을 지정 시점에 실패시키는 wrapper.
type failingRegistry struct {
	quoteid.Registry
	failNthPut int // 1-base. N 번째 Put 만 실패.
	putCount   int
}

func (f *failingRegistry) Put(ctx context.Context, rec quoteid.Record) error {
	f.putCount++
	if f.putCount == f.failNthPut {
		return errors.New("simulated registry put failure")
	}
	return f.Registry.Put(ctx, rec)
}

// failingSwapIndex — PutSwap 만 실패.
type failingSwapIndex struct {
	quoteid.SwapIndex
	failPutSwap bool
}

func (f *failingSwapIndex) PutSwap(ctx context.Context, rec quoteid.SwapRecord) error {
	if f.failPutSwap {
		return errors.New("simulated swap index put failure")
	}
	return f.SwapIndex.PutSwap(ctx, rec)
}

func TestSwapLock_NearPutFailure_NoRevoke(t *testing.T) {
	metrics := &AtomicSwapLockMetrics{}
	deps, reg := newSwapDeps(t, metrics)
	deps.Reg = &failingRegistry{Registry: reg, failNthPut: 1}

	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// Memory 에 아무것도 남지 않아야.
	if reg.Len() != 0 {
		t.Errorf("registry 에 leak: %d", reg.Len())
	}
	snap := metrics.Snapshot()
	if snap.FailNear != 1 || snap.Successes != 0 {
		t.Errorf("metrics: %+v", snap)
	}
	if snap.RevokeOK != 0 || snap.RevokeFail != 0 {
		t.Errorf("near 실패 시 revoke 호출 없어야: %+v", snap)
	}
}

func TestSwapLock_FarPutFailure_RevokesNear(t *testing.T) {
	metrics := &AtomicSwapLockMetrics{}
	deps, reg := newSwapDeps(t, metrics)
	deps.Reg = &failingRegistry{Registry: reg, failNthPut: 2}

	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// near 가 등록됐다가 revoke 되어 0 이어야.
	if reg.Len() != 0 {
		t.Errorf("registry 에 near leak: %d", reg.Len())
	}
	snap := metrics.Snapshot()
	if snap.FailFar != 1 || snap.RevokeOK != 1 {
		t.Errorf("metrics: %+v (FailFar=1, RevokeOK=1 기대)", snap)
	}
}

func TestSwapLock_SwapIndexFailure_RevokesBothLegs(t *testing.T) {
	metrics := &AtomicSwapLockMetrics{}
	deps, reg := newSwapDeps(t, metrics)
	deps.Idx = &failingSwapIndex{SwapIndex: reg, failPutSwap: true}

	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// near + far 모두 revoke 되어 0 이어야.
	if reg.Len() != 0 {
		t.Errorf("registry 에 leak: %d", reg.Len())
	}
	snap := metrics.Snapshot()
	if snap.FailSwapIndex != 1 || snap.RevokeOK != 2 {
		t.Errorf("metrics: %+v (FailSwapIndex=1, RevokeOK=2 기대)", snap)
	}
}

// ---- S3-e: Server 라우트 등록 + /v1/quote/swap/stats ----

// makeSwapServer — registerSwapLockRoutes 테스트 helper. 라우트 등록에 필요한
// deps 만 주입 (broker / etcd / grpc 우회).
func makeSwapServer(t *testing.T, enable bool, withSwapIdx bool) (*Server, *quoteid.MemoryRegistry) {
	t.Helper()
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})
	reg := quoteid.NewMemoryRegistry(time.Second)
	gen := quoteid.NewGenerator("T")

	cfg := DefaultConfig()
	cfg.EnableSwapLock = enable
	cfg.DevMode = true
	cfg.QuoteIDRegistryTimeout = 200 * time.Millisecond
	srv := NewServer(cfg, nil)
	srv.best = best
	srv.AttachPricing(store)
	srv.AttachQuoteID(gen, reg, 500*time.Millisecond)
	if withSwapIdx {
		srv.AttachSwapIndex(reg)
	}
	return srv, reg
}

func TestServer_SwapLockRoute_GatedByFlag(t *testing.T) {
	srv, _ := makeSwapServer(t, false /*enable*/, true /*withIdx*/)
	mux := http.NewServeMux()
	srv.registerSwapLockRoutes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/quote/swap/lock", bytes.NewReader([]byte("{}")))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("flag off 인데 라우트 등록됨: status=%d", rec.Code)
	}
}

func TestServer_SwapLockRoute_NoSwapIndex_404(t *testing.T) {
	srv, _ := makeSwapServer(t, true /*enable*/, false /*withIdx*/)
	mux := http.NewServeMux()
	srv.registerSwapLockRoutes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/quote/swap/lock", bytes.NewReader([]byte("{}")))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("SwapIndex 미주입인데 등록됨: status=%d", rec.Code)
	}
}

func TestServer_SwapLockRoute_EnabledFlow(t *testing.T) {
	srv, _ := makeSwapServer(t, true /*enable*/, true /*withIdx*/)
	mux := http.NewServeMux()
	srv.registerSwapLockRoutes(mux)

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP", CustomerID: "alice",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/quote/swap/lock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("swap lock 호출 실패: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// stats 검증.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/quote/swap/stats", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("stats 호출 실패: status=%d", rec.Code)
	}
	var snap SwapLockMetricsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("stats JSON 파싱: %v", err)
	}
	if snap.Requests != 1 || snap.Successes != 1 {
		t.Errorf("stats snapshot: %+v", snap)
	}
}

func TestSwapLock_MissingDeps_503(t *testing.T) {
	metrics := &AtomicSwapLockMetrics{}
	deps, _ := newSwapDeps(t, metrics)
	deps.Idx = nil // SwapIndex 미주입.

	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	body, _ := json.Marshal(SwapLockRequest{
		Pair: "USD/KRW", Near: SwapLeg{Tenor: "SPOT"}, Far: SwapLeg{Tenor: "1M"},
		Profile: "WEB.BRANCH.VIP",
	})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
