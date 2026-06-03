package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newGrafanaDeps(t *testing.T, base, user, pass string) *GrafanaProxyDeps {
	t.Helper()
	return &GrafanaProxyDeps{
		BaseURL:  base,
		Username: user,
		Password: pass,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestGrafanaAlerts_NoBaseURL_503(t *testing.T) {
	h := GrafanaAlerts(newGrafanaDeps(t, "", "", ""))
	r := httptest.NewRequest("GET", "/v1/admin/grafana-alerts", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", w.Code)
	}
}

func TestGrafanaAlerts_RejectsPost(t *testing.T) {
	h := GrafanaAlerts(newGrafanaDeps(t, "http://example", "", ""))
	r := httptest.NewRequest("POST", "/v1/admin/grafana-alerts", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", w.Code)
	}
}

func TestGrafanaAlerts_ProxiesWithBasicAuth(t *testing.T) {
	var seenPath, seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
	}))
	defer upstream.Close()

	h := GrafanaAlerts(newGrafanaDeps(t, upstream.URL, "admin", "secret"))
	r := httptest.NewRequest("GET", "/v1/admin/grafana-alerts", nil)
	w := httptest.NewRecorder()
	h(w, r)

	if w.Code != 200 {
		t.Errorf("status=%d body=%s", w.Code, w.Body.String())
	}
	if seenPath != "/api/prometheus/grafana/api/v1/rules" {
		t.Errorf("upstream path=%q", seenPath)
	}
	if seenAuth == "" {
		t.Error("Basic auth 헤더 누락")
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content-type forward 누락")
	}
}

func TestGrafanaAlerts_NoAuth_NoBasicHeader(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := GrafanaAlerts(newGrafanaDeps(t, upstream.URL, "", ""))
	r := httptest.NewRequest("GET", "/v1/admin/grafana-alerts", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if seenAuth != "" {
		t.Errorf("username 비어있는데 Authorization 헤더 전송: %q", seenAuth)
	}
}
