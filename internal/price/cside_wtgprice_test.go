//go:build wtgprice

// cside_wtgprice_test.go — C SDK (cside/wtgprice/sample) ↔ SwapLockHandler 의
// wire 호환성 검증.
//
// 실행:
//   make wtgprice                  # cside/wtgprice 빌드
//   go test -tags=wtgprice -run CSideWtgprice -v ./internal/price/...
//
// build tag 로 분리 — 기본 CI test 에선 skip (C 빌드 의존 X).

package price

import (
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quoteid"
)

func wtgpriceSamplePath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// internal/price → repo root → cside/wtgprice/sample.
	p := filepath.Join(wd, "..", "..", "cside", "wtgprice", "sample")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample 없음 (%s) — make wtgprice 먼저 실행. err: %v", p, err)
	}
	return p
}

func parseHostPort(t *testing.T, raw string) (string, string) {
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

// TestCSideWtgprice_HappyPath — sample 으로 SPOT/1M swap 발급 → handler 응답
// → C 파서가 모든 필수 필드 추출.
func TestCSideWtgprice_HappyPath(t *testing.T) {
	sample := wtgpriceSamplePath(t)
	store, best := newForwardTestSetup(t, []pricing.CustomerRule{
		{CustomerID: "alice", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add", Priority: 100},
	})
	reg := quoteid.NewMemoryRegistry(5 * time.Second)
	gen := quoteid.NewGenerator("T")
	deps := SwapLockDeps{
		Store: store, Best: best, Gen: gen, Reg: reg, Idx: reg,
		Validity: 500 * time.Millisecond, PutTimeout: 200 * time.Millisecond,
		SpotDays: 2, Metrics: &AtomicSwapLockMetrics{},
	}
	srv := httptest.NewServer(SwapLockHandler(deps, true))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample exec: %v\nout: %s", err, out)
	}
	t.Logf("sample stdout: %s", out)

	// sample 출력에 필수 필드들이 있어야 — C 파서가 정상 동작.
	for _, key := range []string{"swap_id     :", "near        : qid=T-", "far         : qid=T-",
		"pair        : USD/KRW", "valid_remain:"} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample 출력에 %q 없음 — C 파서 실패 가능", key)
		}
	}
}

// TestCSideWtgprice_BadProfile_4xx — 잘못된 profile → handler 400 → sample 1.
func TestCSideWtgprice_BadProfile_4xx(t *testing.T) {
	// 본 테스트는 sample 이 profile 을 인자로 받지 못해 skip — 향후 sample
	// 확장 시 활성. 현재 sample 은 profile 고정.
	t.Skip("sample 이 profile 인자 미지원 (현재 기본 WEB.BRANCH.VIP 고정)")
}
