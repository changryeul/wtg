//go:build integration

package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/test/etcdtest"
)

func newEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	srv := etcdtest.Start(t)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{srv.ClientURL},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func newTestMux(cli *clientv3.Client) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := NewAuditRing(50)
	prefix := "test/"

	mux := http.NewServeMux()
	symDeps := &SymbolsDeps{Cli: cli, Prefix: prefix + "quote/symbols/", Logger: logger, Audit: audit}
	prDeps := &PricingDeps{Cli: cli, Key: prefix + "pricing/table", Logger: logger, Audit: audit}
	pfDeps := &ProfilesDeps{Cli: cli, Prefix: prefix + "price/profiles/", Logger: logger, Audit: audit}

	mux.HandleFunc("GET /v1/admin/symbols", ListSymbols(symDeps))
	mux.HandleFunc("GET /v1/admin/symbols/{symbol}", GetSymbol(symDeps))
	mux.HandleFunc("PUT /v1/admin/symbols/{symbol}", PutSymbol(symDeps))
	mux.HandleFunc("DELETE /v1/admin/symbols/{symbol}", DeleteSymbol(symDeps))

	mux.HandleFunc("GET /v1/admin/pricing/table", GetPricingTable(prDeps))
	mux.HandleFunc("PUT /v1/admin/pricing/table", PutPricingTable(prDeps))

	mux.HandleFunc("GET /v1/admin/profiles", ListProfiles(pfDeps))
	mux.HandleFunc("GET /v1/admin/profiles/{key}", GetProfile(pfDeps))
	mux.HandleFunc("PUT /v1/admin/profiles/{key}", PutProfile(pfDeps))
	mux.HandleFunc("DELETE /v1/admin/profiles/{key}", DeleteProfile(pfDeps))
	return mux
}

func TestAdmin_Symbols_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	// 1) 빈 LIST.
	resp, err := http.Get(ts.URL + "/v1/admin/symbols")
	if err != nil {
		t.Fatal(err)
	}
	var listOut struct {
		Symbols []map[string]any `json:"symbols"`
		Count   int              `json:"count"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listOut)
	resp.Body.Close()
	if listOut.Count != 0 {
		t.Errorf("초기 count = %d, want 0", listOut.Count)
	}

	// 2) PUT 2건.
	put := func(sym, pair string, active bool) {
		body := strings.NewReader(`{"symbol":"` + sym + `","pair":"` + pair + `","active":` + boolStr(active) + `}`)
		req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/symbols/"+sym, body)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("PUT %s status=%d body=%s", sym, r.StatusCode, b)
		}
		r.Body.Close()
	}
	put("USDKRW", "USD/KRW", true)
	put("EURKRW", "EUR/KRW", true)

	// 3) GET 단건.
	r, _ := http.Get(ts.URL + "/v1/admin/symbols/USDKRW")
	if r.StatusCode != 200 {
		t.Fatalf("GET USDKRW status=%d", r.StatusCode)
	}
	r.Body.Close()

	// 4) LIST 후 2건.
	resp, _ = http.Get(ts.URL + "/v1/admin/symbols")
	_ = json.NewDecoder(resp.Body).Decode(&listOut)
	resp.Body.Close()
	if listOut.Count != 2 {
		t.Errorf("count = %d, want 2", listOut.Count)
	}

	// 5) DELETE.
	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/admin/symbols/USDKRW", nil)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("DELETE status=%d", r.StatusCode)
	}
	r.Body.Close()

	// 6) GET 삭제된 항목 → 404.
	r, _ = http.Get(ts.URL + "/v1/admin/symbols/USDKRW")
	if r.StatusCode != 404 {
		t.Errorf("삭제된 항목 GET status=%d, want 404", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdmin_Symbols_ValidationErrors(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	// pair 누락 → 400.
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/symbols/X", strings.NewReader(`{"symbol":"X","active":true}`))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("pair 누락: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()

	// bad JSON → 400.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/symbols/X", strings.NewReader(`not json`))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("bad JSON: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdmin_PricingTable_PutGet(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	// 1) 빈 상태 → 404.
	r, _ := http.Get(ts.URL + "/v1/admin/pricing/table")
	if r.StatusCode != 404 {
		t.Errorf("초기 GET: status=%d, want 404", r.StatusCode)
	}
	r.Body.Close()

	// 2) PUT.
	body := `{
		"version": 10,
		"hq_margin": [{"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}]
	}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/pricing/table", strings.NewReader(body))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT status=%d body=%s", r.StatusCode, b)
	}
	r.Body.Close()

	// 3) GET 후 version 확인.
	r, _ = http.Get(ts.URL + "/v1/admin/pricing/table")
	if r.StatusCode != 200 {
		t.Fatalf("GET status=%d", r.StatusCode)
	}
	var got struct {
		Version int64 `json:"version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got.Version != 10 {
		t.Errorf("Version = %d, want 10", got.Version)
	}
}

func TestAdmin_PricingTable_InvalidJSON(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/pricing/table", strings.NewReader(`not valid json`))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("invalid JSON: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdmin_Profiles_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	// PUT.
	body := `{"channel":"WEB","site":"BRANCH","tier":"VIP"}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/profiles/WEB.BRANCH.VIP", strings.NewReader(body))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT status=%d body=%s", r.StatusCode, b)
	}
	r.Body.Close()

	// GET.
	r, _ = http.Get(ts.URL + "/v1/admin/profiles/WEB.BRANCH.VIP")
	if r.StatusCode != 200 {
		t.Fatalf("GET status=%d", r.StatusCode)
	}
	r.Body.Close()

	// path / body key 불일치 → 400.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/profiles/WEB.BRANCH.GOLD",
		strings.NewReader(`{"channel":"WEB","site":"BRANCH","tier":"VIP"}`))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("키 불일치: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()

	// 누락 필드 → 400.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/profiles/WEB.BRANCH.STD",
		strings.NewReader(`{"channel":"WEB","site":"BRANCH"}`))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("tier 누락: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()

	// LIST.
	r, _ = http.Get(ts.URL + "/v1/admin/profiles")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var listOut struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&listOut)
	r.Body.Close()
	if listOut.Count != 1 {
		t.Errorf("LIST count = %d, want 1", listOut.Count)
	}

	// DELETE.
	req, _ = http.NewRequest("DELETE", ts.URL+"/v1/admin/profiles/WEB.BRANCH.VIP", nil)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Errorf("DELETE status=%d", r.StatusCode)
	}
	r.Body.Close()
}

func TestAdmin_503_WhenEtcdMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	// Cli nil 로 deps 구성 — 모든 핸들러는 503 반환해야.
	symDeps := &SymbolsDeps{Cli: nil, Logger: logger}
	mux.HandleFunc("GET /v1/admin/symbols", ListSymbols(symDeps))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r, _ := http.Get(ts.URL + "/v1/admin/symbols")
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", r.StatusCode)
	}
	r.Body.Close()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
