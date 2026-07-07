//go:build wtgquery

// cside_wtgquery_test.go — C SDK (cside/wtgquery/sample_s02 + sample_s03)
// ↔ /v1/best-stats 의 wire 호환성. mds W9501S02/S03 시그니처가 그대로
// 채워지는지 검증.
//
// 실행:
//   make wtgquery
//   go test -tags=wtgquery -run CSideWtgquery -v ./internal/price/...

package price

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func wtgquerySampleS02Path(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	p := filepath.Join(wd, "..", "..", "cside", "wtgquery", "sample_s02")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample_s02 없음 (%s) — make wtgquery 먼저 실행. err: %v", p, err)
	}
	return p
}

func wtgquerySampleS03Path(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	p := filepath.Join(wd, "..", "..", "cside", "wtgquery", "sample_s03")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample_s03 없음 (%s) — make wtgquery 먼저 실행. err: %v", p, err)
	}
	return p
}

func parseHostPortQuery(t *testing.T, raw string) (string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url parse: %v", err)
	}
	host := u.Hostname()
	port := u.Port()
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("port atoi: %v", err)
	}
	return host, port
}

// bestStatsTestServer — /v1/best-stats 만 노출하는 httptest 서버 (실제
// mci-price server.go 의 wiring 과 동일).
func bestStatsTestServer(t *testing.T, bc *BestConsumer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/best-stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bc.Stats())
	})
	return httptest.NewServer(mux)
}

// TestCSideWtgquery_W9501S02_BEST — exnm "BEST" 는 BestBid/BestAsk 사용.
func TestCSideWtgquery_W9501S02_BEST(t *testing.T) {
	sample := wtgquerySampleS02Path(t)
	bc := NewBestConsumer(BestOptions{}, &collector{})
	bc.OnTick(buildRaw("USDKRW", "SMB", 1378.65, 1378.69))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1378.60, 1378.72))
	srv := bestStatsTestServer(t, bc)
	defer srv.Close()

	host, port := parseHostPortQuery(t, srv.URL)
	cmd := exec.Command(sample, host, port, "BEST", "USDKRW")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_s02 exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_s02 stdout: %s", out)

	for _, key := range []string{
		"exnm        : BEST",
		"symb        : USDKRW",
		"bid         : 1378.65000 (source=B)", // BEST = max(bid) = SMB
		"ask         : 1378.69000 (source=B)", // BEST = min(ask) = SMB
		"bid_best    : 1378.65000",
		"ask_best    : 1378.69000",
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample_s02 출력에 %q 없음 — out: %s", key, out)
		}
	}
}

// TestCSideWtgquery_W9501S02_BySource — exnm "SMB" / "KMB" 는 per-source
// 값을 그대로 반환. SourceQuotes 매핑 검증.
func TestCSideWtgquery_W9501S02_BySource(t *testing.T) {
	sample := wtgquerySampleS02Path(t)
	bc := NewBestConsumer(BestOptions{}, &collector{})
	bc.OnTick(buildRaw("USDKRW", "SMB", 1378.65, 1378.69))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1378.60, 1378.72))
	srv := bestStatsTestServer(t, bc)
	defer srv.Close()

	host, port := parseHostPortQuery(t, srv.URL)
	cmd := exec.Command(sample, host, port, "KMB", "USDKRW")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_s02 exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_s02 stdout: %s", out)

	for _, key := range []string{
		"exnm        : KMB",
		"bid         : 1378.60000 (source=K)",
		"ask         : 1378.72000 (source=K)",
		"bid_best    : 1378.65000", // BEST 는 SMB
		"ask_best    : 1378.69000",
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample_s02 출력에 %q 없음 — out: %s", key, out)
		}
	}
}

// TestCSideWtgquery_W9501S02_UnknownExnm — exnm 가 active source 가
// 아니면 bid/ask 0, src=' ', best 만 채워짐.
func TestCSideWtgquery_W9501S02_UnknownExnm(t *testing.T) {
	sample := wtgquerySampleS02Path(t)
	bc := NewBestConsumer(BestOptions{}, &collector{})
	bc.OnTick(buildRaw("USDKRW", "SMB", 1378.65, 1378.69))
	srv := bestStatsTestServer(t, bc)
	defer srv.Close()

	host, port := parseHostPortQuery(t, srv.URL)
	cmd := exec.Command(sample, host, port, "EBS", "USDKRW")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_s02 exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_s02 stdout: %s", out)

	if !strings.Contains(string(out), "bid         : 0.00000 (source= )") {
		t.Errorf("EBS miss 시 bid 0.0 / src=' ' 기대 — out: %s", out)
	}
	if !strings.Contains(string(out), "bid_best    : 1378.65000") {
		t.Errorf("best 는 항상 채워져야 — out: %s", out)
	}
}

// TestCSideWtgquery_W9501S03_Bulk — 3 pair (BEST/SMB/KMB) bulk 호출 →
// 1회 best-stats fetch 후 N pair fill. mds wire 그대로 출력.
func TestCSideWtgquery_W9501S03_Bulk(t *testing.T) {
	sample := wtgquerySampleS03Path(t)
	bc := NewBestConsumer(BestOptions{}, &collector{})
	bc.OnTick(buildRaw("USDKRW", "SMB", 1378.65, 1378.69))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1378.60, 1378.72))
	srv := bestStatsTestServer(t, bc)
	defer srv.Close()

	host, port := parseHostPortQuery(t, srv.URL)
	cmd := exec.Command(sample, host, port)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_s03 exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_s03 stdout: %s", out)

	for _, key := range []string{
		"nrec        : 3",
		"rec[0]      : exnm=BEST",
		"rec[0] bid  : 1378.65000 (src=B)",
		"rec[1]      : exnm=SMB",
		"rec[1] bid  : 1378.65000 (src=S)",
		"rec[2]      : exnm=KMB",
		"rec[2] bid  : 1378.60000 (src=K)",
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample_s03 출력에 %q 없음 — out: %s", key, out)
		}
	}
}
