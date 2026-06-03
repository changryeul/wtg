package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newPromDeps(t *testing.T, baseURL string) *PromProxyDeps {
	t.Helper()
	return &PromProxyDeps{
		BaseURL: baseURL,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPromQuery_NoBaseURL_503(t *testing.T) {
	h := PromQuery(newPromDeps(t, ""))
	r := httptest.NewRequest("GET", "/v1/admin/prom-query?path=query&query=up", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", w.Code)
	}
}

func TestPromQuery_RejectsBadPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream 호출되면 안 됨: %s", r.URL.Path)
	}))
	defer upstream.Close()
	h := PromQuery(newPromDeps(t, upstream.URL))

	cases := []string{"", "admin/tsdb", "../admin", "labels", "metadata"}
	for _, p := range cases {
		r := httptest.NewRequest("GET", "/v1/admin/prom-query?path="+p+"&query=up", nil)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("path=%q: status=%d, want 400", p, w.Code)
		}
	}
}

func TestPromQuery_RejectsEmptyQuery(t *testing.T) {
	h := PromQuery(newPromDeps(t, "http://example"))
	r := httptest.NewRequest("GET", "/v1/admin/prom-query?path=query&query=", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPromQuery_RejectsPost(t *testing.T) {
	h := PromQuery(newPromDeps(t, "http://example"))
	r := httptest.NewRequest("POST", "/v1/admin/prom-query?path=query&query=up", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status=%d, want 405", w.Code)
	}
}

func TestPromQuery_ProxiesQueryAndQueryRange(t *testing.T) {
	var hits []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer upstream.Close()

	h := PromQuery(newPromDeps(t, upstream.URL))

	// instant query.
	r := httptest.NewRequest("GET", "/v1/admin/prom-query?path=query&query=sum(rate(wtg_http_requests_total%5B1m%5D))", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 200 {
		t.Errorf("instant: status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content-type forward 누락: %q", w.Header().Get("Content-Type"))
	}

	// range query — step/start/end 전달.
	r = httptest.NewRequest("GET", "/v1/admin/prom-query?path=query_range&query=up&start=100&end=200&step=15s", nil)
	w = httptest.NewRecorder()
	h(w, r)
	if w.Code != 200 {
		t.Errorf("range: status=%d", w.Code)
	}

	if len(hits) != 2 {
		t.Fatalf("upstream hits=%d, want 2", len(hits))
	}
	if got := hits[0]; !contains(got, "/api/v1/query?") || !contains(got, "query=") {
		t.Errorf("instant upstream URL 잘못됨: %s", got)
	}
	if got := hits[1]; !contains(got, "/api/v1/query_range?") || !contains(got, "step=15s") {
		t.Errorf("range upstream URL 잘못됨: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
