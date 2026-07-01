package price

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/winwaysystems/wtg/pkg/metrics"
	"github.com/winwaysystems/wtg/pkg/pricing"
)

// P6MetricsOpts — RegisterP6Metrics 의존성. nil 인 필드는 그 그룹 메트릭 skip.
type P6MetricsOpts struct {
	Cross    *CrossRateConsumer
	Pricing  *PricingConsumer
	Currency *pricing.CurrencyMaster
	Pair     *pricing.PairMaster
	Quote    *QuoteValidationServer // quoteid validation stats (옵션)
	Best     *BestConsumer          // best dedup 카운터 (옵션)
}

// RegisterP6Metrics — P6 인프라 (cross-rate / 마진 5L / master) 의 Prometheus
// 메트릭을 등록한다. GaugeFunc 으로 lazy scrape — Prometheus 가 /metrics 요청
// 시점에 Stats() 호출. 누적 counter 도 GaugeFunc 로 expose (단조 증가하면
// PromQL rate() 정상 동작).
//
// 메트릭 prefix 는 wtg_ — 운영 환경의 다른 시스템과 충돌 회피.
func RegisterP6Metrics(reg *metrics.Registry, opts P6MetricsOpts) error {
	// Cross-rate consumer.
	if opts.Cross != nil {
		c := opts.Cross
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_emits_total",
			Help: "CrossRateConsumer 가 emit 한 합성 cross tick 누적 수",
		}, func() float64 { return float64(c.Stats().EmitsTotal) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_skipped_debounce_total",
			Help: "debounce window 안 중복 emit 시도 누적",
		}, func() float64 { return float64(c.Stats().SkippedDebounce) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_skipped_stale_total",
			Help: "leg 중 하나가 stale 이라 emit skip 누적",
		}, func() float64 { return float64(c.Stats().SkippedStale) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_skipped_missing_leg_total",
			Help: "한 쪽 leg 가 cache 에 없어 emit skip 누적",
		}, func() float64 { return float64(c.Stats().SkippedMissingLeg) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_skipped_unknown_pair_total",
			Help: "Symbol → Pair lookup 실패 누적 (SymbolMap/PairMaster 갱신 지연)",
		}, func() float64 { return float64(c.Stats().SkippedUnknownPair) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_errors_total",
			Help: "ComputeCross 산술 실패 누적 (invalid op / negative input 등)",
		}, func() float64 { return float64(c.Stats().Errors) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_cross_formulas_count",
			Help: "현재 등록된 cross 산식 수 (PairMaster.CrossFormulas)",
		}, func() float64 { return float64(c.Stats().FormulaCount) })); err != nil {
			return err
		}
	}

	// PricingConsumer (P3~P4a customer fan-out).
	if opts.Pricing != nil {
		pc := opts.Pricing
		gauges := []struct {
			name, help string
			fn         func() float64
		}{
			{"wtg_pricing_ticks_in_total", "PricingConsumer 가 받은 tick 누적",
				func() float64 { return float64(pc.Stats().TicksIn) }},
			{"wtg_pricing_ticks_dropped_total", "디코딩/심볼/active 실패 drop 누적",
				func() float64 { return float64(pc.Stats().TicksDropped) }},
			{"wtg_pricing_quotes_published_total", "Profile-only customer quote publish 누적",
				func() float64 { return float64(pc.Stats().QuotesPublished) }},
			{"wtg_pricing_publish_errors_total", "publish 실패 누적",
				func() float64 { return float64(pc.Stats().PublishErrors) }},
			{"wtg_pricing_profiles_skipped_total", "구독자 0 으로 pruned 한 Profile publish 누적",
				func() float64 { return float64(pc.Stats().ProfilesSkipped) }},
			{"wtg_pricing_customer_quotes_published_total", "5-Layer customer-specific quote publish 누적",
				func() float64 { return float64(pc.Stats().CustomerQuotesPublished) }},
			{"wtg_pricing_customer_publish_errors_total", "customer publish 실패 누적",
				func() float64 { return float64(pc.Stats().CustomerPublishErrors) }},
			{"wtg_pricing_customers_registered", "현재 등록된 customer 수 (lazy fan-out 대상)",
				func() float64 { return float64(pc.Stats().CustomersRegistered) }},
			{"wtg_pricing_quote_register_errors_total", "QuoteID Registry.Put 실패 누적",
				func() float64 { return float64(pc.Stats().QuoteRegErrors) }},
		}
		for _, g := range gauges {
			if err := reg.Register(prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{Name: g.name, Help: g.help}, g.fn,
			)); err != nil {
				return err
			}
		}
	}

	// Currency / Pair master sizes.
	if opts.Currency != nil {
		cm := opts.Currency
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_master_currency_count",
			Help: "CurrencyMaster 에 등록된 통화 수 (fx-sync 미러)",
		}, func() float64 { return float64(cm.Size()) })); err != nil {
			return err
		}
	}
	if opts.Pair != nil {
		pm := opts.Pair
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_master_pair_count",
			Help: "PairMaster 에 등록된 통화쌍 수 (direct + cross, active+inactive)",
		}, func() float64 { return float64(pm.Size()) })); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_master_pair_active_count",
			Help: "PairMaster 의 active=true pair 수",
		}, func() float64 {
			n := 0
			for _, p := range pm.List() {
				if p.Active {
					n++
				}
			}
			return float64(n)
		})); err != nil {
			return err
		}
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_master_pair_cross_count",
			Help: "PairMaster 의 active cross pair 수",
		}, func() float64 {
			n := 0
			for _, p := range pm.List() {
				if p.Active && p.Kind == "cross" {
					n++
				}
			}
			return float64(n)
		})); err != nil {
			return err
		}
	}

	// BestConsumer — dedup 카운터 (same-price / below-tick / emitted).
	if opts.Best != nil {
		bc := opts.Best
		bestGauges := []struct {
			name, help string
			fn         func() float64
		}{
			{"wtg_best_emitted_total", "BestConsumer 가 downstream 으로 fan-out 한 tick 누적",
				func() float64 { return float64(bc.Stats().Dedup.Emitted) }},
			{"wtg_best_dedup_dropped_same_price_total", "이전 emit 과 bid+ask 완전 동일이라 skip 한 tick 누적",
				func() float64 { return float64(bc.Stats().Dedup.DroppedSamePrice) }},
			{"wtg_best_dedup_dropped_below_tick_total", "bid/ask 변화가 tick_size 미만이라 skip 한 tick 누적",
				func() float64 { return float64(bc.Stats().Dedup.DroppedBelowTick) }},
			{"wtg_best_rejected_quotes_total", "invariant 위반 (bid<=0/ask<=0/bid>ask) 으로 reject 한 raw tick 누적",
				func() float64 { return float64(bc.Stats().RejectedQuotes) }},
		}
		for _, g := range bestGauges {
			if err := reg.Register(prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{Name: g.name, Help: g.help}, g.fn,
			)); err != nil {
				return err
			}
		}
		// Enabled 플래그도 gauge 로 노출 (dashboard 에서 필터 편의).
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "wtg_best_dedup_enabled",
			Help: "BestConsumer dedup 활성 여부 (1=on, 0=off)",
		}, func() float64 {
			if bc.Stats().Dedup.Enabled {
				return 1
			}
			return 0
		})); err != nil {
			return err
		}
	}

	return nil
}
