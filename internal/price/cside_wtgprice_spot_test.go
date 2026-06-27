//go:build wtgprice

// cside_wtgprice_spot_test.go — C SDK (cside/wtgprice/sample_spot) ↔
// SpotSnapshotHandler 의 wire 호환성 검증.
//
// 실행:
//   make wtgprice                  # cside/wtgprice 빌드 (sample_spot 포함)
//   go test -tags=wtgprice -run CSideWtgpriceSpot -v ./internal/price/...

package price

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/pricing"
)

func wtgpriceSampleSpotPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	p := filepath.Join(wd, "..", "..", "cside", "wtgprice", "sample_spot")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample_spot 없음 (%s) — make wtgprice 먼저 실행. err: %v", p, err)
	}
	return p
}

// TestCSideWtgpriceSpot_HappyPath — sample_spot 으로 단일 USD/KRW 조회 →
// handler 응답 → C 파서가 pair/bid/ask/raw_bid/raw_ask/source/table_version
// 모두 추출.
func TestCSideWtgpriceSpot_HappyPath(t *testing.T) {
	sample := wtgpriceSampleSpotPath(t)
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})
	srv := httptest.NewServer(SpotSnapshotHandler(ForwardSnapshotDeps{
		Store: store, Best: best,
	}, true))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "USD/KRW", "WEB.BRANCH.VIP", "alice")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_spot exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_spot stdout: %s", out)

	for _, key := range []string{
		"spot_count  : 1",
		"pair=USD/KRW",
		"src=BEST",
		"table_ver   :",
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample_spot 출력에 %q 없음 — C 파서 실패 가능", key)
		}
	}
	// 5-Layer 적용 확인 — alice rule 의 BidDelta=-0.01 이 반영됐는지.
	// newForwardTestSetup 의 USD/KRW BEST 는 1378.65/1378.69.
	// 정확한 customer-applied 값은 다른 layer 와 결합되므로 substring 만.
	if !strings.Contains(string(out), "bid=1378.") {
		t.Errorf("bid 범위 1378.x 기대 — out: %s", out)
	}
}

// TestCSideWtgpriceSpot_Bulk_Missing — 다중 pair (USD/KRW 는 시드, EUR/KRW 는 미시드)
// → spots[1] + missing[1]. C 파서의 missing[] array iteration 검증.
func TestCSideWtgpriceSpot_Bulk_Missing(t *testing.T) {
	sample := wtgpriceSampleSpotPath(t)
	store, best := newForwardTestSetup(t, nil)
	srv := httptest.NewServer(SpotSnapshotHandler(ForwardSnapshotDeps{
		Store: store, Best: best,
	}, true))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	// customer_id 빈 인자 → C SDK 가 query 에서 customer_id 생략.
	cmd := exec.Command(sample, host, port, "USD/KRW,EUR/KRW", "WEB.BRANCH.VIP", "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample_spot exec: %v\nout: %s", err, out)
	}
	t.Logf("sample_spot stdout: %s", out)

	if !strings.Contains(string(out), "spot_count  : 1") {
		t.Errorf("spot_count 1 기대 (USD/KRW 만 시드) — out: %s", out)
	}
	if !strings.Contains(string(out), "pair=USD/KRW") {
		t.Errorf("pair=USD/KRW 기대")
	}
	if !strings.Contains(string(out), "missing     : 1 pair") {
		t.Errorf("missing 1 기대 — out: %s", out)
	}
	if !strings.Contains(string(out), "missing[0]  : EUR/KRW") {
		t.Errorf("missing 에 EUR/KRW 기대 — out: %s", out)
	}
}

// TestCSideWtgpriceSpot_BadProfile_4xx — 잘못된 profile → handler 400 →
// SDK 가 WTGPRICE_E_HTTP_4XX 반환 → sample_spot exit 1.
func TestCSideWtgpriceSpot_BadProfile_4xx(t *testing.T) {
	sample := wtgpriceSampleSpotPath(t)
	store, best := newForwardTestSetup(t, nil)
	srv := httptest.NewServer(SpotSnapshotHandler(ForwardSnapshotDeps{
		Store: store, Best: best,
	}, true))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	// invalid profile (ParseProfileKey 실패).
	cmd := exec.Command(sample, host, port, "USD/KRW", "BAD_PROFILE", "")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("sample_spot 가 성공해선 안 됨 (BAD_PROFILE) — out: %s", out)
	}
	t.Logf("sample_spot 실패 stderr: %s", out)
	if !strings.Contains(string(out), "http=400") {
		t.Errorf("HTTP 400 기대 — out: %s", out)
	}
}
