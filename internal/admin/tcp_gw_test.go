package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 정상 proxy — edge-tcp stats JSON 이 그대로 전달된다.
func TestTcpGwStatsProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"active_conns":2}`))
	}))
	defer upstream.Close()

	rr := httptest.NewRecorder()
	TcpGwStats(upstream.URL, quietLogger())(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/tcp-gw/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var body struct {
		Active int `json:"active_conns"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Active != 2 {
		t.Errorf("active=%d", body.Active)
	}
}

// 게이트웨이 미기동 → 502, 미설정 → 503.
func TestTcpGwStatsErrors(t *testing.T) {
	dead := httptest.NewServer(nil)
	dead.Close()

	rr := httptest.NewRecorder()
	TcpGwStats(dead.URL, quietLogger())(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("dead: %d", rr.Code)
	}

	rr2 := httptest.NewRecorder()
	TcpGwStats("", quietLogger())(rr2, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled: %d", rr2.Code)
	}
}
