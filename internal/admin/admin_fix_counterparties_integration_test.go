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

// FIX 카운터파티 CRUD — embedded etcd 통합 테스트.
func TestAdmin_FixCounterparties_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := NewAuditRing(50)
	prefix := "test-fc/fix/counterparties/"

	deps := &FixCounterpartiesDeps{Cli: cli, Prefix: prefix, Logger: logger, Audit: audit}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/fix-counterparties", ListFixCounterparties(deps))
	mux.HandleFunc("GET /v1/admin/fix-counterparties/{cid}", GetFixCounterparty(deps))
	mux.HandleFunc("PUT /v1/admin/fix-counterparties/{cid}", PutFixCounterparty(deps))
	mux.HandleFunc("DELETE /v1/admin/fix-counterparties/{cid}", DeleteFixCounterparty(deps))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1) LIST 초기 빈.
	r, _ := http.Get(ts.URL + "/v1/admin/fix-counterparties")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var l struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&l)
	r.Body.Close()
	if l.Count != 0 {
		t.Errorf("초기 count=%d, want 0", l.Count)
	}

	// 2) PUT — ECN_DEUTSCHE.
	body := `{"password":"deutsche-pw","channel":"FIX","site":"HQ","tier":"VIP","usid":"ECN_DEUTSCHE_01"}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/admin/fix-counterparties/ECN_DEUTSCHE_01", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("PUT status=%d", r.StatusCode)
	}
	var pe fixCpEntry
	_ = json.NewDecoder(r.Body).Decode(&pe)
	r.Body.Close()
	if pe.SenderCompID != "ECN_DEUTSCHE_01" || pe.Site != "HQ" || pe.Tier != "VIP" {
		t.Errorf("PUT 응답: %+v", pe)
	}
	if pe.Password != "***" {
		t.Errorf("PUT 응답에 password 마스킹 안 됨: %q", pe.Password)
	}

	// 3) GET 단일 — password 전체 노출.
	r, _ = http.Get(ts.URL + "/v1/admin/fix-counterparties/ECN_DEUTSCHE_01")
	_ = json.NewDecoder(r.Body).Decode(&pe)
	r.Body.Close()
	if pe.Password != "deutsche-pw" {
		t.Errorf("GET 단일 password=%q, want 'deutsche-pw'", pe.Password)
	}

	// 4) LIST — 1건.
	r, _ = http.Get(ts.URL + "/v1/admin/fix-counterparties")
	var ll struct {
		Counterparties []fixCpEntry `json:"counterparties"`
		Count          int          `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&ll)
	r.Body.Close()
	if ll.Count != 1 || ll.Counterparties[0].Password != "***" {
		t.Errorf("LIST password 마스킹 안 됨: %+v", ll)
	}

	// 5) DELETE.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/admin/fix-counterparties/ECN_DEUTSCHE_01", nil)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("DELETE status=%d", r.StatusCode)
	}
	r.Body.Close()

	// 6) 404.
	r, _ = http.Get(ts.URL + "/v1/admin/fix-counterparties/ECN_DEUTSCHE_01")
	if r.StatusCode != 404 {
		t.Errorf("DELETE 후 GET status=%d, want 404", r.StatusCode)
	}
	r.Body.Close()

	// 7) validation — slash 포함 cid → 400.
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/v1/admin/fix-counterparties/has%2Fslash", strings.NewReader(`{}`))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("slash 포함 cid status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()

	// 8) audit 2건 (PUT + DELETE).
	if audit.Len() < 2 {
		t.Errorf("audit len=%d, want ≥2", audit.Len())
	}
}
