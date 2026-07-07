//go:build wtgquery

// cside_wtgquery_test.go — C SDK (cside/wtgquery/sample) ↔ /v1/chart 의
// wire 호환성 검증. mds W9501S01 시그니처가 그대로 채워지는지 확인.
//
// 실행:
//   make wtgquery                  # cside/wtgquery 빌드
//   go test -tags=wtgquery -run CSideWtgquery -v ./internal/chart/...

package chart

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

func wtgquerySamplePath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// internal/chart → repo root → cside/wtgquery/sample.
	p := filepath.Join(wd, "..", "..", "cside", "wtgquery", "sample")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample 없음 (%s) — make wtgquery 먼저 실행. err: %v", p, err)
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

// TestCSideWtgquery_W9501S01_HappyPath — 1 일봉을 받아 W9501S01_out_t 로
// 채워지는지. mds 필드 모두 검증 — kymd 일치, bid/ask 5자리.
func TestCSideWtgquery_W9501S01_HappyPath(t *testing.T) {
	sample := wtgquerySamplePath(t)
	// 봉 1개 — OpenedAt 는 SDK 의 today-7d~now 범위 안 (어제 UTC midnight).
	openedAt := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	closedAt := openedAt.Add(24 * time.Hour)
	repo := &fakeRepository{
		bars: []quote.Bar{
			{
				Pair: session.Pair("USD/KRW"), TF: quote.TF1d,
				OpenedAt: openedAt, ClosedAt: closedAt,
				OpenBid: 1378.50, OpenAsk: 1378.65,
				HighBid: 1379.10, HighAsk: 1379.25,
				LowBid: 1378.20, LowAsk: 1378.35,
				CloseBid: 1378.80, CloseAsk: 1378.95,
				TickCount: 12345,
			},
		},
	}
	srv := newTestServer(repo)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "USDKRW")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample exec: %v\nout: %s", err, out)
	}
	t.Logf("sample stdout: %s", out)

	// mds 형태 필수 라인.
	expectedKymd := openedAt.Format("20060102")
	for _, key := range []string{
		"pdcd        : SPT",
		"symb        : USDKRW",
		"nrec        : 1",
		"kymd=" + expectedKymd,
		"bid  : open=1378.50000 high=1379.10000 low=1378.20000 last=1378.80000",
		"ask  : open=1378.65000 high=1379.25000 low=1378.35000 last=1378.95000",
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("sample 출력에 %q 없음 — wire mapping 실패 가능", key)
		}
	}

	// fake repo 가 SDK 의 query 파라미터를 받았는지.
	if len(repo.calls) != 1 {
		t.Fatalf("QueryBars 호출 횟수 = %d, want 1", len(repo.calls))
	}
	call := repo.calls[0]
	if call.pair != session.Pair("USD/KRW") {
		t.Errorf("pair=%q, want USD/KRW (symb→pair 변환 실패)", call.pair)
	}
	if call.tf != quote.TF1d {
		t.Errorf("tf=%q, want 1d (pdcd SPT→tf 매핑 실패)", call.tf)
	}
}

// TestCSideWtgquery_W9501S01_MultiBar — 3 일봉 → nrec=3 + 각 record 채워짐.
// C 측 array iteration 검증.
func TestCSideWtgquery_W9501S01_MultiBar(t *testing.T) {
	sample := wtgquerySamplePath(t)
	t0 := time.Now().UTC().Truncate(24 * time.Hour).Add(-3 * 24 * time.Hour)
	bars := make([]quote.Bar, 3)
	for i := range bars {
		op := t0.Add(time.Duration(i) * 24 * time.Hour)
		bars[i] = quote.Bar{
			Pair: session.Pair("USD/KRW"), TF: quote.TF1d,
			OpenedAt: op, ClosedAt: op.Add(24 * time.Hour),
			OpenBid: 1378.0 + float64(i), OpenAsk: 1378.1 + float64(i),
			HighBid: 1379.0 + float64(i), HighAsk: 1379.1 + float64(i),
			LowBid: 1377.5 + float64(i), LowAsk: 1377.6 + float64(i),
			CloseBid: 1378.5 + float64(i), CloseAsk: 1378.6 + float64(i),
			TickCount: 100,
		}
	}
	repo := &fakeRepository{bars: bars}
	srv := newTestServer(repo)
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "USDKRW")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample exec: %v\nout: %s", err, out)
	}
	t.Logf("sample stdout: %s", out)

	if !strings.Contains(string(out), "nrec        : 3") {
		t.Errorf("nrec=3 기대 — out: %s", out)
	}
	for i := 0; i < 3; i++ {
		kymd := bars[i].OpenedAt.Format("20060102")
		if !strings.Contains(string(out), "rec["+strconv.Itoa(i)+"]      : symb=USDKRW kymd="+kymd) {
			t.Errorf("rec[%d] kymd=%s 누락 — out: %s", i, kymd, out)
		}
	}
}

// TestCSideWtgquery_W9501S01_UnsupportedFWD — pdcd=FWD 는 PoC 미지원.
// (sample 이 pdcd 인자 미노출이라 t.Skip — 향후 sample 확장 시 활성.)
func TestCSideWtgquery_W9501S01_UnsupportedFWD(t *testing.T) {
	t.Skip("sample 이 pdcd 인자 미지원 (현재 SPT 고정) — phase 2 에서 sample 확장")
}
