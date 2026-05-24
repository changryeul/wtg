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

	upDeps := &UserProfilesDeps{Cli: cli, Prefix: prefix + "auth/user-profiles/", Logger: logger, Audit: audit}
	mux.HandleFunc("GET /v1/admin/user-profiles", ListUserProfiles(upDeps))
	mux.HandleFunc("GET /v1/admin/user-profiles/{usid}", GetUserProfile(upDeps))
	mux.HandleFunc("PUT /v1/admin/user-profiles/{usid}", PutUserProfile(upDeps))
	mux.HandleFunc("DELETE /v1/admin/user-profiles/{usid}", DeleteUserProfile(upDeps))

	qeDeps := &QuoteIDEnginesDeps{Cli: cli, Prefix: prefix + "quoteid/engines/", Logger: logger, Audit: audit}
	mux.HandleFunc("GET /v1/admin/quoteid-engines", ListQuoteIDEngines(qeDeps))
	mux.HandleFunc("GET /v1/admin/quoteid-engines/{engine_id}", GetQuoteIDEngine(qeDeps))
	mux.HandleFunc("PUT /v1/admin/quoteid-engines/{engine_id}", PutQuoteIDEngine(qeDeps))
	mux.HandleFunc("DELETE /v1/admin/quoteid-engines/{engine_id}", DeleteQuoteIDEngine(qeDeps))
	return mux
}

func TestAdmin_UserProfiles_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	// PUT 2건.
	put := func(usid, site, tier string, wantStatus int) {
		body := `{"site":"` + site + `","tier":"` + tier + `"}`
		req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/user-profiles/"+usid, strings.NewReader(body))
		r, _ := http.DefaultClient.Do(req)
		defer r.Body.Close()
		if r.StatusCode != wantStatus {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("PUT %s: status=%d want=%d body=%s", usid, r.StatusCode, wantStatus, b)
		}
	}
	put("trader01", "BRANCH", "VIP", 200)
	put("trader02", "HQ", "STD", 200)

	// LIST.
	r, _ := http.Get(ts.URL + "/v1/admin/user-profiles")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var listOut struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&listOut)
	r.Body.Close()
	if listOut.Count != 2 {
		t.Errorf("LIST count = %d, want 2", listOut.Count)
	}

	// GET 단건.
	r, _ = http.Get(ts.URL + "/v1/admin/user-profiles/trader01")
	if r.StatusCode != 200 {
		t.Fatalf("GET status=%d", r.StatusCode)
	}
	var got struct {
		Usid string `json:"usid"`
		Site string `json:"site"`
		Tier string `json:"tier"`
	}
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got.Usid != "trader01" || got.Site != "BRANCH" || got.Tier != "VIP" {
		t.Errorf("GET payload: %+v", got)
	}

	// 검증 — site/tier 누락 400.
	put("trader03", "", "VIP", 400)
	put("trader04", "BRANCH", "", 400)

	// DELETE.
	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/admin/user-profiles/trader01", nil)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Errorf("DELETE status=%d", r.StatusCode)
	}
	r.Body.Close()
	// 삭제 후 GET 404.
	r, _ = http.Get(ts.URL + "/v1/admin/user-profiles/trader01")
	if r.StatusCode != 404 {
		t.Errorf("DELETE 후 GET: status=%d, want 404", r.StatusCode)
	}
	r.Body.Close()
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

	cases := []struct {
		name string
		body string
		want int
	}{
		{"pair 누락", `{"symbol":"X","active":true}`, 400},
		{"bad JSON", `not json`, 400},
		{"pair 슬래시 없음", `{"symbol":"X","pair":"USDKRW","active":true}`, 400},
		{"pair 슬래시 2개", `{"symbol":"X","pair":"USD/KR/W","active":true}`, 400},
		{"pair base 빈값", `{"symbol":"X","pair":"/KRW","active":true}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/symbols/X", strings.NewReader(tc.body))
			r, _ := http.DefaultClient.Do(req)
			defer r.Body.Close()
			if r.StatusCode != tc.want {
				t.Errorf("status=%d, want %d", r.StatusCode, tc.want)
			}
		})
	}
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

// 정책 검증 — 음수 마진은 400 + code=policy.
func TestAdmin_PricingTable_NegativeMargin(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	body := `{
		"version": 1,
		"hq_margin": [{"pair":"USD/KRW","tier":"VIP","bid_amount":-0.01,"ask_amount":0.02}]
	}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/pricing/table", strings.NewReader(body))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("음수 마진: status=%d, want 400", r.StatusCode)
	}
	var e map[string]string
	_ = json.NewDecoder(r.Body).Decode(&e)
	r.Body.Close()
	if e["error"] != "policy" {
		t.Errorf("error code = %q, want 'policy'", e["error"])
	}
}

// 정책 검증 — channel/site 동시 와일드카드는 400.
func TestAdmin_PricingTable_BroadWildcard(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	body := `{
		"version": 1,
		"site_margin": [{"pair":"USD/KRW","channel":"","site":"","bid_amount":0.1,"ask_amount":0.1}]
	}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/pricing/table", strings.NewReader(body))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("광범위 wildcard: status=%d, want 400", r.StatusCode)
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

func TestAdmin_QuoteIDEngines_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	ts := httptest.NewServer(newTestMux(cli))
	defer ts.Close()

	put := func(id, body string, wantStatus int) {
		req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/quoteid-engines/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, _ := http.DefaultClient.Do(req)
		defer r.Body.Close()
		if r.StatusCode != wantStatus {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("PUT %s: status=%d want=%d body=%s", id, r.StatusCode, wantStatus, b)
		}
	}

	// 풀 권한 (빈 body).
	put("matching-A", "", 200)
	// read-only + contact.
	put("audit-cli", `{"permissions":["validate"],"contact":"audit@bank.com"}`, 200)
	// 만료 시각 (future).
	put("debug-cli", `{"permissions":["validate"],"expires_at":"2099-01-01T00:00:00Z"}`, 200)

	// 잘못된 permission → 400.
	put("bad-perm", `{"permissions":["root"]}`, 400)
	// 잘못된 ExpiresAt → 400.
	put("bad-date", `{"expires_at":"yesterday"}`, 400)

	// LIST 3건.
	r, _ := http.Get(ts.URL + "/v1/admin/quoteid-engines")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var list struct {
		Engines []map[string]any `json:"engines"`
		Count   int              `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&list)
	r.Body.Close()
	if list.Count != 3 {
		t.Errorf("LIST count=%d, want 3", list.Count)
	}

	// GET audit-cli — permissions echo.
	r, _ = http.Get(ts.URL + "/v1/admin/quoteid-engines/audit-cli")
	if r.StatusCode != 200 {
		t.Fatalf("GET audit-cli: %d", r.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if got["contact"] != "audit@bank.com" {
		t.Errorf("contact echo: %v", got)
	}
	perms, _ := got["permissions"].([]any)
	if len(perms) != 1 || perms[0] != "validate" {
		t.Errorf("permissions echo: %v", perms)
	}

	// GET matching-A — 빈 meta (default 풀 권한).
	r, _ = http.Get(ts.URL + "/v1/admin/quoteid-engines/matching-A")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("matching-A GET: %d", r.StatusCode)
	}

	// DELETE.
	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/admin/quoteid-engines/audit-cli", nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("DELETE: %d", r.StatusCode)
	}
	// 재삭제 → 404.
	req, _ = http.NewRequest("DELETE", ts.URL+"/v1/admin/quoteid-engines/audit-cli", nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("재삭제: %d, want 404", r.StatusCode)
	}
}
