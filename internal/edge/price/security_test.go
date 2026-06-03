package price

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winwaysystems/wtg/pkg/netutil"
	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

func TestEdgePrice_AllowCIDRs_BlocksDeniedIP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cidrs, err := netutil.ParseCIDRs("10.99.0.0/16")
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

func TestEdgePrice_AllowCIDRs_AllowsLoopback(t *testing.T) {
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

func TestEdgePrice_RateLimit_BurstExhausted(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
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
		t.Errorf("burst 첫 요청 통과 못함: 200=%d", got200)
	}
	if got429 < 1 {
		t.Errorf("burst 초과 후 429 없음: 429=%d, 200=%d", got429, got200)
	}
}
