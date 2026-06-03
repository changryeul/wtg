package push

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

// IPAllowList — denied CIDR 의 클라이언트는 모든 경로 (인증 면제 ping 포함) 가
// chain 상위 미들웨어에서 403 으로 차단되어야 한다.
func TestEdgePush_AllowCIDRs_BlocksDeniedIP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cidrs, err := netutil.ParseCIDRs("10.99.0.0/16") // loopback 비포함
	if err != nil {
		t.Fatal(err)
	}
	cfg.AllowCIDRs = cidrs

	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied IP /v1/ping: status=%d, want 403", resp.StatusCode)
	}
}

func TestEdgePush_AllowCIDRs_AllowsLoopback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cidrs, _ := netutil.ParseCIDRs("127.0.0.0/8")
	cfg.AllowCIDRs = cidrs

	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback 허용 인데 status=%d, want 200", resp.StatusCode)
	}
}

// IP rate-limit — burst 이상으로 요청 시 429.
func TestEdgePush_RateLimit_BurstExhausted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	// path-aware default 의 /v1/ping 은 1000/s — 테스트 위해 명시 룰셋으로 override.
	cfg.IPRatePerSec = 0
	cfg.RateLimitRules = []ratelimit.Rule{
		{Pattern: "GET /v1/ping", Rate: 1, Burst: 2},
	}

	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	var got200, got429 int
	for i := 0; i < 10; i++ {
		resp, err := http.Get(ts.URL + "/v1/ping")
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
		t.Errorf("burst 초과 후 429 없음: 429=%d, 200=%d", got429, got200)
	}
}
