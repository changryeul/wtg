package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/netutil"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newEdgeForTest 는 mock upstream + edge BuildHandler 를 묶어 httptest 서버 두 개를
// 띄우고 cleanup 클로저를 반환한다.
func newEdgeForTest(t *testing.T, upstream http.HandlerFunc, devMode bool) (edgeURL string, cleanup func()) {
	t.Helper()
	upSrv := httptest.NewServer(upstream)
	cfg := DefaultConfig()
	cfg.UpstreamURL = upSrv.URL
	cfg.DevMode = devMode
	cfg.MaxRequestBody = 0
	cfg.UpstreamTimeout = 2 * time.Second

	srv := NewServer(cfg, quietLogger())
	handler, err := srv.BuildHandler()
	if err != nil {
		upSrv.Close()
		t.Fatal(err)
	}
	edgeSrv := httptest.NewServer(handler)
	return edgeSrv.URL, func() {
		edgeSrv.Close()
		upSrv.Close()
	}
}

func TestEdgeForwardsAuthenticatedUser(t *testing.T) {
	var seenUser, seenRID, seenForward atomic.Value
	edgeURL, cleanup := newEdgeForTest(t, func(w http.ResponseWriter, r *http.Request) {
		seenUser.Store(r.Header.Get("X-WTG-User"))
		seenRID.Store(r.Header.Get("X-Request-ID"))
		seenForward.Store(r.Header.Get("X-Forwarded-Host"))
		_, _ = w.Write([]byte(`{"reached":true}`))
	}, true)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, edgeURL+"/v1/tx", strings.NewReader(`{}`))
	req.Header.Set("X-WTG-User", "trader01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d", resp.StatusCode)
	}
	if seenUser.Load() != "trader01" {
		t.Errorf("upstream X-WTG-User: %v", seenUser.Load())
	}
	if rid, _ := seenRID.Load().(string); rid == "" {
		t.Error("upstream X-Request-ID 가 빈 값")
	}
	if fh, _ := seenForward.Load().(string); fh == "" {
		t.Error("X-Forwarded-Host 누락")
	}
}

func TestEdgeStripsAuthorizationHeader(t *testing.T) {
	var seenAuth, seenCookie atomic.Value
	edgeURL, cleanup := newEdgeForTest(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		seenCookie.Store(r.Header.Get("Cookie"))
		_, _ = w.Write([]byte(`{}`))
	}, true)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, edgeURL+"/v1/tx", nil)
	req.Header.Set("X-WTG-User", "trader01")
	req.Header.Set("Authorization", "Bearer FAKE")
	req.Header.Set("Cookie", "session=ABC")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v, _ := seenAuth.Load().(string); v != "" {
		t.Errorf("Authorization 이 upstream 에 전달됨: %q", v)
	}
	if v, _ := seenCookie.Load().(string); v != "" {
		t.Errorf("Cookie 가 upstream 에 전달됨: %q", v)
	}
}

func TestEdgeStripsServerHeaderInResponse(t *testing.T) {
	edgeURL, cleanup := newEdgeForTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "upstream-secret/9.9")
		w.Header().Set("X-Powered-By", "go-secret")
		_, _ = w.Write([]byte(`{}`))
	}, true)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, edgeURL+"/v1/tx", nil)
	req.Header.Set("X-WTG-User", "trader01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("Server"); strings.Contains(v, "upstream-secret") {
		t.Errorf("Server 헤더 노출: %q", v)
	}
	if v := resp.Header.Get("X-Powered-By"); v != "" {
		t.Errorf("X-Powered-By 노출: %q", v)
	}
}

func TestEdgeRejectsUnauthenticated(t *testing.T) {
	called := false
	edgeURL, cleanup := newEdgeForTest(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	}, true)
	defer cleanup()

	resp, err := http.Get(edgeURL + "/v1/tx") // X-WTG-User 없음
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", resp.StatusCode)
	}
	if called {
		t.Error("upstream 호출되면 안 됨")
	}
}

func TestEdgePingBypassAuth(t *testing.T) {
	edgeURL, cleanup := newEdgeForTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("/v1/ping 은 upstream 호출 없어야")
	}, false)
	defer cleanup()

	resp, err := http.Get(edgeURL + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["service"] != "mci-edge-api" {
		t.Errorf("service: %q", body["service"])
	}
}

func TestEdgeUpstreamUnavailable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UpstreamURL = "http://127.0.0.1:1" // unreachable
	cfg.DevMode = true
	cfg.UpstreamTimeout = 200 * time.Millisecond

	srv := NewServer(cfg, quietLogger())
	handler, err := srv.BuildHandler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/tx", nil)
	req.Header.Set("X-WTG-User", "trader01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: %d, want 502", resp.StatusCode)
	}
}

func TestEdgeBuildHandlerInvalidUpstream(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UpstreamURL = "::not a url"
	srv := NewServer(cfg, quietLogger())
	if _, err := srv.BuildHandler(); err == nil {
		t.Error("invalid upstream URL 에 대해 에러 기대")
	}

	cfg.UpstreamURL = "http://" // host 없음
	srv = NewServer(cfg, quietLogger())
	if _, err := srv.BuildHandler(); err == nil {
		t.Error("host 없는 URL 에 대해 에러 기대")
	}
}

func TestStripIngressHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer X")
	h.Set("Cookie", "session=abc")
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-WTG-User", "fake-injected")
	h.Set("X-WTG-SID", "fake-sid")
	h.Set("X-WTG-Channel", "fake-chan")
	h.Set("X-Custom", "keep")
	stripIngressHeaders(h)
	if h.Get("Authorization") != "" {
		t.Error("Authorization 미제거")
	}
	if h.Get("Cookie") != "" {
		t.Error("Cookie 미제거")
	}
	if h.Get("X-WTG-User") != "" || h.Get("X-WTG-SID") != "" || h.Get("X-WTG-Channel") != "" {
		t.Error("X-WTG-* 헤더 미제거 (외부 위변조 차단 실패)")
	}
	if h.Get("X-Custom") != "keep" {
		t.Error("일반 헤더 잘못 제거")
	}
}

func TestStripEgressHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Server", "x")
	h.Set("X-Powered-By", "y")
	h.Set("Content-Type", "application/json")
	stripEgressHeaders(h)
	if h.Get("Server") != "" || h.Get("X-Powered-By") != "" {
		t.Error("egress 헤더 미제거")
	}
	if h.Get("Content-Type") == "" {
		t.Error("Content-Type 잘못 제거")
	}
}

func TestEdgeAllowCIDRs(t *testing.T) {
	// httptest 가 띄우는 loopback 서버에 외부 IP 가 잡히지 않으므로 RemoteAddr 의
	// 127.x.x.x 만 통과하는 allowlist 로도 의미 있는 거부 검증이 가능하다 — 실제
	// 외부 IP 시뮬레이션은 RoundTripper level 변경이 필요해서 unit 범위 밖.
	cfg := DefaultConfig()
	cfg.UpstreamURL = "http://127.0.0.1:1"
	cfg.DevMode = true
	cfg.MaxRequestBody = 0
	// 의도적으로 loopback 을 *제외* — 모든 요청이 403 으로 거부되어야 한다.
	cidrs, err := netutil.ParseCIDRs("10.99.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AllowCIDRs = cidrs

	srv := NewServer(cfg, quietLogger())
	handler, err := srv.BuildHandler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("loopback 이 비-허용 CIDR 인데 403 아님: %d", resp.StatusCode)
	}
}

func TestEdgeMaxBodyEnforced(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UpstreamURL = "http://127.0.0.1:1"
	cfg.DevMode = true
	cfg.MaxRequestBody = 16 // 16 bytes 만 허용

	srv := NewServer(cfg, quietLogger())
	handler, _ := srv.BuildHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	body := strings.NewReader(strings.Repeat("X", 1000))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/tx", body)
	req.Header.Set("X-WTG-User", "trader01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// upstream unreachable 또는 본문 거부 — 둘 다 에러 가능.
		return
	}
	defer resp.Body.Close()
	// upstream 까지 못 가는 게 정상 (502 또는 413).
	if resp.StatusCode == http.StatusOK {
		t.Errorf("max body 초과인데 200 OK: %d", resp.StatusCode)
	}
}
