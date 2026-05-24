package price

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/metrics"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// scrapeMetrics — Registry.Handler() 를 호출해서 /metrics 본문 문자열 반환.
func scrapeMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestQuoteValidation_Metrics_Validate(t *testing.T) {
	reg := metrics.NewRegistry()
	srv, regStore := mkValidationServer(t)
	srv.SetMetrics(reg)
	_ = regStore.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// 1 OK + 1 NOT_FOUND.
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-nope"})

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `wtg_quoteid_op_total{op="validate",service="mci-price",status="ok"} 1`) {
		t.Errorf("validate OK 카운터 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_op_total{op="validate",service="mci-price",status="not_found"} 1`) {
		t.Errorf("validate NOT_FOUND 카운터 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_op_duration_seconds_count{op="validate",service="mci-price"} 2`) {
		t.Errorf("validate duration 히스토그램 누락:\n%s", body)
	}
}

func TestQuoteValidation_Metrics_BatchValidate(t *testing.T) {
	reg := metrics.NewRegistry()
	srv, regStore := mkValidationServer(t)
	srv.SetMetrics(reg)
	_ = regStore.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))
	_ = regStore.Put(context.Background(), mkRegRecord("A-2", time.Now(), time.Hour))

	_, _ = srv.BatchValidate(context.Background(), &wtgpb.BatchValidateRequest{
		QuoteIds: []string{"A-1", "A-2", "A-nope"},
	})

	body := scrapeMetrics(t, reg)
	// per-item 카운터.
	if !strings.Contains(body, `wtg_quoteid_op_total{op="batch_validate",service="mci-price",status="ok"} 2`) {
		t.Errorf("batch_validate OK x2 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_op_total{op="batch_validate",service="mci-price",status="not_found"} 1`) {
		t.Errorf("batch_validate NOT_FOUND x1 누락:\n%s", body)
	}
	// batch wallclock + size 히스토그램 (1회 호출).
	if !strings.Contains(body, `wtg_quoteid_batch_size_count{op="batch_validate",service="mci-price"} 1`) {
		t.Errorf("batch_size 카운트 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_batch_size_sum{op="batch_validate",service="mci-price"} 3`) {
		t.Errorf("batch_size 합 3 (= 항목수) 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_batch_duration_seconds_count{op="batch_validate",service="mci-price"} 1`) {
		t.Errorf("batch_duration 카운트 누락:\n%s", body)
	}
}

func TestQuoteValidation_Metrics_MarkConsumed(t *testing.T) {
	reg := metrics.NewRegistry()
	srv, regStore := mkValidationServer(t)
	srv.SetMetrics(reg)
	_ = regStore.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// 첫 호출 = consume_ok, 두번째 = consume_already.
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-1", ConsumerId: "order-1",
	})
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-1", ConsumerId: "order-2",
	})

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `wtg_quoteid_op_total{op="mark_consumed",service="mci-price",status="consume_ok"} 1`) {
		t.Errorf("consume_ok 누락:\n%s", body)
	}
	if !strings.Contains(body, `wtg_quoteid_op_total{op="mark_consumed",service="mci-price",status="consume_already"} 1`) {
		t.Errorf("consume_already 누락:\n%s", body)
	}
}

func TestQuoteValidation_Metrics_RBACDenied(t *testing.T) {
	reg := metrics.NewRegistry()
	srv, _ := mkValidationServer(t)
	srv.SetMetrics(reg)
	srv.SetEngineAllowlist([]string{"engine-A"})

	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "evil",
	})

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `wtg_quoteid_op_total{op="validate",service="mci-price",status="denied"} 1`) {
		t.Errorf("denied 카운터 누락:\n%s", body)
	}
}

func TestQuoteValidation_Metrics_NilSafe(t *testing.T) {
	// SetMetrics 호출 안 함 → 메트릭 비활성. RPC 가 정상 동작해야.
	srv, regStore := mkValidationServer(t)
	_ = regStore.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})
	if err != nil {
		t.Fatalf("Validate w/o metrics: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("status=%v, want OK", resp.GetStatus())
	}
}
