package chart

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/netutil"
)

// newFakeUpstream — Internal mci-chart 모사. 받은 헤더를 callback 으로 캡처.
func newFakeUpstream(t *testing.T, onReq func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onReq != nil {
			onReq(r)
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>chart UI</html>"))
		case "/v1/chart":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"bars":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newEdgeServer(t *testing.T, upstream string, dev bool) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream
	cfg.DevMode = dev
	cfg.IPRatePerSec = 0
	return NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// UI 경로 (/) 는 인증 없이 통과.
func TestEdgeChart_UIUnauthenticated(t *testing.T) {
	up := newFakeUpstream(t, nil)
	s := newEdgeServer(t, up.URL, false)
	h, err := s.BuildHandler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("UI status=%d, want 200 (인증 없이 통과)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "<html>chart UI</html>" {
		t.Errorf("UI body mismatch: %q", body)
	}
}

// /v1/* 는 JWT/Dev 둘 다 없으면 통과 (사내망 default).
func TestEdgeChart_API_NoAuthConfigured(t *testing.T) {
	up := newFakeUpstream(t, nil)
	s := newEdgeServer(t, up.URL, false) // DevMode 도 off, JWT 도 nil
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/chart")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("미설정 인증: status=%d, want 200 (사내망 default)", resp.StatusCode)
	}
}

// /v1/* DevMode → X-WTG-User 헤더 필요.
func TestEdgeChart_DevMode_RequiresUserHeader(t *testing.T) {
	up := newFakeUpstream(t, nil)
	s := newEdgeServer(t, up.URL, true)
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 헤더 없이 → 401.
	resp, _ := http.Get(ts.URL + "/v1/chart")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("헤더 없이 status=%d, want 401", resp.StatusCode)
	}

	// 헤더 있으면 통과.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/chart", nil)
	req.Header.Set(middleware.HeaderEdgeUser, "tester")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("DevMode + 헤더: status=%d, want 200", resp.StatusCode)
	}
}

// 외부 X-WTG-User 헤더는 strip — 사용자가 다른 사람으로 위장 불가.
func TestEdgeChart_StripsIngressHeaders(t *testing.T) {
	var captured http.Header
	up := newFakeUpstream(t, func(r *http.Request) {
		captured = r.Header.Clone()
	})
	s := newEdgeServer(t, up.URL, true)
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 클라이언트가 X-WTG-User 두 개 보냄 (DevMode 자기 헤더 + 스푸핑 시도).
	// edge 는 X-WTG-User 만 DevMode 검증 후 Principal 로 변환, 그 외 X-WTG-* 는 strip.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/chart", nil)
	req.Header.Set(middleware.HeaderEdgeUser, "tester")
	req.Header.Set(middleware.HeaderEdgeSID, "FORGED-SID")
	req.Header.Set(middleware.HeaderEdgeSite, "HQ")
	req.Header.Set(middleware.HeaderEdgeTier, "VIP")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// upstream 이 받은 헤더: SID 는 edge 가 Principal 의 SessionID 에서만 채움
	// (DevMode 라 SessionID 빈값) — 스푸핑된 FORGED-SID 가 통과되면 안 됨.
	if got := captured.Get(middleware.HeaderEdgeSID); got == "FORGED-SID" {
		t.Errorf("스푸핑된 SID 가 upstream 까지 도달: %q", got)
	}
	// edge 가 검증한 user 만 전파.
	if got := captured.Get(middleware.HeaderEdgeUser); got != "tester" {
		t.Errorf("X-WTG-User 전파 mismatch: %q", got)
	}
}

// JWT 검증기 활성 시 — 유효한 토큰만 /v1/* 통과.
func TestEdgeChart_JWT_RejectsInvalid(t *testing.T) {
	up := newFakeUpstream(t, nil)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	verifier, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &key.PublicKey}})

	s := newEdgeServer(t, up.URL, false)
	s.SetJWTVerifier(verifier)
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 토큰 없이 → 401.
	resp, _ := http.Get(ts.URL + "/v1/chart")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("토큰 없이: status=%d, want 401", resp.StatusCode)
	}

	// 잘못된 토큰 → 401.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/chart", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("invalid token: status=%d, want 401", resp.StatusCode)
	}

	// 유효한 토큰 → 200.
	issuer, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: key})
	token, _ := issuer.Sign(auth.Claims{
		Usid: "tester", Chan: "WEB",
		EXP: time.Now().Add(time.Hour).Unix(),
	})
	req, _ = http.NewRequest("GET", ts.URL+"/v1/chart", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("유효한 토큰: status=%d, want 200", resp.StatusCode)
	}
}

// IP allowlist — 의도적으로 loopback 을 *제외* 한 CIDR 만 허용 → 모든 요청 403.
func TestEdgeChart_AllowCIDRs_BlocksDeniedIP(t *testing.T) {
	up := newFakeUpstream(t, nil)
	cfg := DefaultConfig()
	cfg.UpstreamURL = up.URL
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	// loopback 비포함 — httptest 는 127.0.0.1 에서 dial 하므로 403.
	cidrs, err := netutil.ParseCIDRs("10.99.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AllowCIDRs = cidrs

	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, err := s.BuildHandler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// healthz 는 IP allowlist 적용 대상 — middleware chain 위쪽에서 차단.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied IP 인데 403 아님: %d", resp.StatusCode)
	}
}

// IP allowlist — loopback 포함 시 정상 통과.
func TestEdgeChart_AllowCIDRs_AllowsLoopback(t *testing.T) {
	up := newFakeUpstream(t, nil)
	cfg := DefaultConfig()
	cfg.UpstreamURL = up.URL
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cidrs, _ := netutil.ParseCIDRs("127.0.0.0/8")
	cfg.AllowCIDRs = cidrs

	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback 허용 CIDR 인데 status=%d, want 200", resp.StatusCode)
	}
}

// IP rate-limit — burst 이상으로 요청 시 429.
func TestEdgeChart_RateLimit_BurstExhausted(t *testing.T) {
	up := newFakeUpstream(t, nil)
	cfg := DefaultConfig()
	cfg.UpstreamURL = up.URL
	cfg.DevMode = true
	cfg.IPRatePerSec = 1 // 1 TPS, burst 작게
	cfg.IPBurst = 2      // 첫 2 요청은 burst 로 통과, 그 이상은 429
	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	var got200, got429 int
	for i := 0; i < 10; i++ {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			got200++
		case http.StatusTooManyRequests:
			got429++
		}
	}
	if got200 < 1 {
		t.Errorf("burst 첫 요청도 통과 못함: 200=%d", got200)
	}
	if got429 < 1 {
		t.Errorf("burst 초과 후 429 안 나옴: 429=%d, 200=%d", got429, got200)
	}
}

// /healthz 는 항상 통과 (upstream 호출 안 함).
func TestEdgeChart_HealthzNoAuth(t *testing.T) {
	up := newFakeUpstream(t, nil)
	s := newEdgeServer(t, up.URL, true)
	h, _ := s.BuildHandler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz status=%d, want 200", resp.StatusCode)
	}
}
