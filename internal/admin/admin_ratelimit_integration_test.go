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
)

func TestAdmin_RateLimit_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := NewAuditRing(50)
	prefix := "test-rl/ratelimit/"

	deps := &RateLimitDeps{Cli: cli, Prefix: prefix, Logger: logger, Audit: audit}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/ratelimit", ListRateLimitPolicies(deps))
	mux.HandleFunc("GET /v1/admin/ratelimit/{service}", GetRateLimitPolicy(deps))
	mux.HandleFunc("PUT /v1/admin/ratelimit/{service}", PutRateLimitPolicy(deps))
	mux.HandleFunc("DELETE /v1/admin/ratelimit/{service}", DeleteRateLimitPolicy(deps))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1) 초기 LIST — 빈.
	r, _ := http.Get(ts.URL + "/v1/admin/ratelimit")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var lo struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&lo)
	r.Body.Close()
	if lo.Count != 0 {
		t.Errorf("초기 count=%d", lo.Count)
	}

	// 2) PUT — 정상 doc.
	body := `{
		"rules": [
			{"pattern":"POST /v1/login","rate":5,"burst":10},
			{"pattern":"POST /v1/tx","rate":50,"burst":100}
		],
		"fallback": {"rate":100,"burst":200}
	}`
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/admin/ratelimit/edge-api", strings.NewReader(body))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("PUT status=%d body=%s", r.StatusCode, b)
	}
	var doc struct {
		Version int64 `json:"version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if doc.Version != 1 {
		t.Errorf("초기 PUT version=%d, want 1", doc.Version)
	}

	// 3) GET 후 PUT 으로 version 증가.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/ratelimit/edge-api",
		strings.NewReader(`{"rules":[{"pattern":"POST /v1/tx","rate":1,"burst":1}]}`))
	r, _ = http.DefaultClient.Do(req)
	_ = json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	if doc.Version != 2 {
		t.Errorf("두번째 PUT version=%d, want 2 (auto increment)", doc.Version)
	}

	// 4) 잘못된 룰 — 400.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/ratelimit/edge-api",
		strings.NewReader(`{"rules":[{"pattern":"","rate":1,"burst":1}]}`))
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("빈 pattern: status=%d, want 400", r.StatusCode)
	}

	// 5) 음수 burst — 400.
	req, _ = http.NewRequest("PUT", ts.URL+"/v1/admin/ratelimit/edge-api",
		strings.NewReader(`{"rules":[{"pattern":"POST /v1/x","rate":1,"burst":-1}]}`))
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("음수 burst: status=%d, want 400", r.StatusCode)
	}

	// 6) DELETE.
	req, _ = http.NewRequest("DELETE", ts.URL+"/v1/admin/ratelimit/edge-api", nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("DELETE status=%d", r.StatusCode)
	}
	// 재 DELETE → 404.
	req, _ = http.NewRequest("DELETE", ts.URL+"/v1/admin/ratelimit/edge-api", nil)
	r, _ = http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Errorf("재 DELETE status=%d, want 404", r.StatusCode)
	}

	// 7) audit 기록 확인.
	all := audit.List(0)
	hasPut, hasDel := false, false
	for _, e := range all {
		if e.Resource == "ratelimit" && e.Action == "PUT_RATELIMIT" {
			hasPut = true
		}
		if e.Resource == "ratelimit" && e.Action == "DELETE_RATELIMIT" {
			hasDel = true
		}
	}
	if !hasPut || !hasDel {
		t.Errorf("audit 누락: PUT=%v DELETE=%v", hasPut, hasDel)
	}
}
