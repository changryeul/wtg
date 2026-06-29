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

// 고객별 ws 구독 허용 pair allowlist CRUD — embedded etcd 통합 테스트.
func TestAdmin_CustomerPairs_CRUD(t *testing.T) {
	cli := newEtcdClient(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := NewAuditRing(50)
	prefix := "test-cp/customers/"

	deps := &CustomerPairsDeps{Cli: cli, Prefix: prefix, Logger: logger, Audit: audit}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/customer-pairs", ListCustomerPairs(deps))
	mux.HandleFunc("GET /v1/admin/customer-pairs/{customer_id}", GetCustomerPairs(deps))
	mux.HandleFunc("PUT /v1/admin/customer-pairs/{customer_id}", PutCustomerPairs(deps))
	mux.HandleFunc("DELETE /v1/admin/customer-pairs/{customer_id}", DeleteCustomerPairs(deps))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1) 초기 LIST — 빈.
	r, _ := http.Get(ts.URL + "/v1/admin/customer-pairs")
	if r.StatusCode != 200 {
		t.Fatalf("LIST status=%d", r.StatusCode)
	}
	var lo struct {
		Count int `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&lo)
	r.Body.Close()
	if lo.Count != 0 {
		t.Errorf("초기 count=%d, want 0", lo.Count)
	}

	// 2) PUT alice — 2 pair.
	body := `{"pairs":["USD/KRW","EUR/USD","USD/KRW"," "]}` // dedup + trim
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/admin/customer-pairs/alice123", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("PUT alice status=%d", r.StatusCode)
	}
	var pe customerPairsEntry
	_ = json.NewDecoder(r.Body).Decode(&pe)
	r.Body.Close()
	if pe.CustomerID != "alice123" {
		t.Errorf("PUT customer_id=%q", pe.CustomerID)
	}
	if len(pe.Pairs) != 2 || pe.Pairs[0] != "EUR/USD" || pe.Pairs[1] != "USD/KRW" {
		t.Errorf("PUT pairs=%v, want sorted dedup [EUR/USD USD/KRW]", pe.Pairs)
	}

	// 3) GET alice — 단일.
	r, _ = http.Get(ts.URL + "/v1/admin/customer-pairs/alice123")
	if r.StatusCode != 200 {
		t.Fatalf("GET alice status=%d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&pe)
	r.Body.Close()
	if len(pe.Pairs) != 2 {
		t.Errorf("GET pairs len=%d", len(pe.Pairs))
	}

	// 4) PUT 빈 list — 전체 차단 의도.
	body = `{"pairs":[]}`
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/v1/admin/customer-pairs/blocked-user", strings.NewReader(body))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("PUT blocked-user status=%d", r.StatusCode)
	}
	_ = json.NewDecoder(r.Body).Decode(&pe)
	r.Body.Close()
	if len(pe.Pairs) != 0 {
		t.Errorf("빈 list 유지 안 됨: %v", pe.Pairs)
	}

	// 5) LIST — 2 customer.
	r, _ = http.Get(ts.URL + "/v1/admin/customer-pairs")
	var ll struct {
		Customers []customerPairsEntry `json:"customers"`
		Count     int                  `json:"count"`
	}
	_ = json.NewDecoder(r.Body).Decode(&ll)
	r.Body.Close()
	if ll.Count != 2 {
		t.Errorf("LIST count=%d, want 2", ll.Count)
	}

	// 6) DELETE — 1 customer 제거.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/admin/customer-pairs/alice123", nil)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("DELETE status=%d", r.StatusCode)
	}
	r.Body.Close()

	// 7) GET alice — 404.
	r, _ = http.Get(ts.URL + "/v1/admin/customer-pairs/alice123")
	if r.StatusCode != 404 {
		t.Errorf("DELETE 후 GET status=%d, want 404", r.StatusCode)
	}
	r.Body.Close()

	// 8) validation — slash 금지.
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/v1/admin/customer-pairs/has%2Fslash", strings.NewReader(`{"pairs":[]}`))
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 400 {
		t.Errorf("slash 포함 cid status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()

	// 9) audit 1+ 건 — PUT/DELETE 모두.
	if audit.Len() < 2 {
		t.Errorf("audit len=%d, want >= 2 (PUT+DELETE)", audit.Len())
	}
}
