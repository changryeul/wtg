package price

import (
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/pricing"
)

// 모든 nil 옵션 — 등록 자체는 에러 없이 통과 (해당 group 메트릭 skip).
func TestRegisterP6Metrics_AllNil(t *testing.T) {
	reg := metrics.NewRegistry()
	if err := RegisterP6Metrics(reg, P6MetricsOpts{}); err != nil {
		t.Fatalf("all nil: %v", err)
	}
}

// CrossRateConsumer 만 등록 — 메트릭이 /metrics 출력에 포함되는지.
func TestRegisterP6Metrics_CrossExposed(t *testing.T) {
	reg := metrics.NewRegistry()
	cr := NewCrossRateConsumer(CrossRateOptions{})
	if err := RegisterP6Metrics(reg, P6MetricsOpts{Cross: cr}); err != nil {
		t.Fatal(err)
	}
	body := scrapeMetrics(t, reg)
	expects := []string{
		"wtg_cross_emits_total",
		"wtg_cross_skipped_debounce_total",
		"wtg_cross_skipped_stale_total",
		"wtg_cross_skipped_missing_leg_total",
		"wtg_cross_skipped_unknown_pair_total",
		"wtg_cross_errors_total",
		"wtg_cross_formulas_count",
	}
	for _, m := range expects {
		if !strings.Contains(body, m) {
			t.Errorf("metric 누락: %s", m)
		}
	}
}

// CurrencyMaster + PairMaster 등록.
func TestRegisterP6Metrics_MasterExposed(t *testing.T) {
	reg := metrics.NewRegistry()
	cm := pricing.NewCurrencyMaster()
	cm.Replace([]pricing.Currency{
		{Code: "USD", Active: true}, {Code: "KRW", Active: true},
	})
	pm := pricing.NewPairMaster()
	pm.Replace([]pricing.Pair{
		{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Active: true},
		{ID: "EURKRW", Base: "EUR", Quote: "KRW", Kind: "cross", Active: true,
			Cross: &pricing.Cross{LegA: "EUR/USD", OpA: "mul", LegB: "USD/KRW", OpB: "mul"}},
		{ID: "HKDKRW", Base: "HKD", Quote: "KRW", Kind: "cross", Active: false},
	})
	if err := RegisterP6Metrics(reg, P6MetricsOpts{Currency: cm, Pair: pm}); err != nil {
		t.Fatal(err)
	}
	body := scrapeMetrics(t, reg)
	// 값까지 검증 — Currency=2 / Pair=3 / Active=2 / Cross=1.
	checks := []struct {
		metric string
		want   string
	}{
		{"wtg_master_currency_count", "wtg_master_currency_count 2"},
		{"wtg_master_pair_count", "wtg_master_pair_count 3"},
		{"wtg_master_pair_active_count", "wtg_master_pair_active_count 2"},
		{"wtg_master_pair_cross_count", "wtg_master_pair_cross_count 1"},
	}
	for _, c := range checks {
		if !strings.Contains(body, c.want) {
			t.Errorf("metric %s 값 불일치 — expected line %q", c.metric, c.want)
		}
	}
}

// 같은 reg 에 두 번 등록은 prom 이 duplicate 에러.
func TestRegisterP6Metrics_NoDoubleRegister(t *testing.T) {
	reg := metrics.NewRegistry()
	cr := NewCrossRateConsumer(CrossRateOptions{})
	if err := RegisterP6Metrics(reg, P6MetricsOpts{Cross: cr}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterP6Metrics(reg, P6MetricsOpts{Cross: cr}); err == nil {
		t.Error("두 번째 등록이 성공함 — duplicate 에러 기대")
	}
}

// scrapeMetrics 는 quote_validation_metrics_test.go 에 정의됨 — 재사용.
