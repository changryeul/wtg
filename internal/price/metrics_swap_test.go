package price

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// scrapeMetricsSwap — Registry.Handler() 를 httptest 로 호출해 /metrics 응답 본문 반환.
func scrapeMetricsSwap(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestPrometheus_SwapLockMetrics_NameSeries(t *testing.T) {
	reg := metrics.NewRegistry()
	lock := &AtomicSwapLockMetrics{}
	if err := RegisterSwapMetrics(reg, lock, nil); err != nil {
		t.Fatal(err)
	}

	// 시나리오: 성공 1 + near 실패 1 + far 실패 + near revoke OK 1.
	lock.OnRequest()
	lock.OnSuccess()
	lock.OnRequest()
	lock.OnPartialFailure("near", "timeout")
	lock.OnRequest()
	lock.OnPartialFailure("far", "timeout")
	lock.OnRevoke("near", "ok")

	body := scrapeMetricsSwap(t, reg)
	// 메트릭명 + 레이블 시리즈 정확 매칭. alert yml 의 메트릭명과 정확히 일치해야.
	must := []string{
		`wtg_swap_lock_requests_total 3`,
		`wtg_swap_lock_successes_total 1`,
		`wtg_swap_lock_partial_failures_total{stage="near"} 1`,
		`wtg_swap_lock_partial_failures_total{stage="far"} 1`,
		`wtg_swap_lock_partial_failures_total{stage="swap_index"} 0`,
		`wtg_swap_lock_revoke_total{outcome="ok"} 1`,
		`wtg_swap_lock_revoke_total{outcome="fail"} 0`,
	}
	for _, m := range must {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics 에 %q 없음", m)
		}
	}
}

func TestPrometheus_SwapValidationMetrics_NameSeries(t *testing.T) {
	reg := metrics.NewRegistry()
	regStore := quoteid.NewMemoryRegistry(time.Second)
	t0 := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	regStore.SetNow(func() time.Time { return t0 })
	validator := NewQuoteValidationServer(regStore, nil)
	validator.now = func() time.Time { return t0 }
	validator.SetSwapIndex(regStore)
	if err := RegisterSwapMetrics(reg, nil, validator); err != nil {
		t.Fatal(err)
	}

	// 시나리오:
	//   - ValidateSwap(SW-1) OK (셋업)
	//   - ValidateSwap(SW-MISSING) NOT_FOUND
	//   - ConsumeSwap(SW-1) OK
	//   - ConsumeSwap(SW-1) ALREADY_CONSUMED (재호출)
	profile, _ := session.ParseProfileKey("WEB.BRANCH.VIP")
	mkRec := func(id string) quoteid.Record {
		return quoteid.Record{
			QuoteID: quoteid.QuoteID(id), Pair: "USD/KRW", Profile: profile, Tenor: "SPOT",
			Bid: 1, Ask: 1,
			IssuedAt: t0.UnixNano(), ValidUntil: t0.Add(500 * time.Millisecond).UnixNano(),
			Sequence: 1, Issuer: "T",
		}
	}
	_ = regStore.Put(context.Background(), mkRec("QN-1"))
	_ = regStore.Put(context.Background(), mkRec("QF-1"))
	_ = regStore.PutSwap(context.Background(), quoteid.SwapRecord{
		SwapID: "SW-1", NearID: "QN-1", FarID: "QF-1",
		IssuedAt: t0.UnixNano(), ValidUntil: t0.Add(500 * time.Millisecond).UnixNano(),
		Issuer: "T",
	})
	_, _ = validator.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{SwapId: "SW-1"})
	_, _ = validator.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{SwapId: "SW-MISSING"})
	_, _ = validator.ConsumeSwap(context.Background(), &wtgpb.ConsumeSwapRequest{SwapId: "SW-1", ConsumerId: "order-1"})
	_, _ = validator.ConsumeSwap(context.Background(), &wtgpb.ConsumeSwapRequest{SwapId: "SW-1", ConsumerId: "order-2"})

	body := scrapeMetricsSwap(t, reg)
	must := []string{
		`wtg_swap_validate_total{status="ok"} 1`,
		`wtg_swap_validate_total{status="not_found"} 1`,
		`wtg_swap_consume_total{status="ok"} 1`,
		`wtg_swap_consume_total{status="already_consumed"} 1`,
		// 두 번째 ConsumeSwap 은 사전검사에서 ALREADY 발견 → partial-race 카운터는 미증가.
		`wtg_consume_swap_partial_race_total 0`,
	}
	for _, m := range must {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics 에 %q 없음\n--- 발췌 ---\n%s", m, sampleSwapLines(body))
		}
	}
}

// sampleSwapLines — body 에서 swap 관련 라인만 추출 (디버그 출력 가독성).
func sampleSwapLines(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "wtg_swap_") || strings.Contains(line, "wtg_consume_swap_") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// 부분-race 발생 시 wtg_consume_swap_partial_race_total 증가 확인.
// near 만 사전 표시된 후 ConsumeSwap → 사전검사 단계에서 잡히지 않고 (즉시 MarkConsumed
// 전이만) 흐름을 만들려면 사전검사 시점은 OK, MarkConsumed 시점은 ALREADY 가 되어야.
// 본 단위에선 MemoryRegistry 가 단일 스레드 → 사전검사와 MarkConsumed 사이에 외부
// MarkConsumed 가 끼어들도록 직접 호출 시뮬레이션.
func TestPrometheus_PartialRace_Increment(t *testing.T) {
	t.Skip("partial-race 는 race 발생 시점이 사전검사와 MarkConsumed 사이라 단위 테스트로 결정적 재현이 어렵다 — 통합 테스트에서 다룬다 (S3-c 후속).")
}
