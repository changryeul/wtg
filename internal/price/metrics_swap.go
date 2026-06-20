package price

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/winwaysystems/wtg/pkg/metrics"
)

// Phase S3-e 후속 — FX swap 관련 Prometheus 메트릭 노출.
//
// 세 가지 도메인:
//
//   1) swap_lock 발급 (AtomicSwapLockMetrics) — POST /v1/quote/swap/lock 의 핸들러
//   2) ValidateSwap (QuoteValidationServer.swapValidate*) — 매매 AP 가 호출
//   3) ConsumeSwap (QuoteValidationServer.swapConsume*) — 매매 AP 가 호출
//
// 모두 prometheus.Collector 직접 구현 — labels 가진 시리즈를 lazy scrape 로
// 노출. AtomicSwapLockMetrics / QuoteValidationServer 의 카운터는 atomic.Uint64
// 이므로 thread-safe Load.
//
// 메트릭 명 컨벤션은 etc/grafana/mci-price-swaplock-alerts.yml 의 alert rules
// 와 정확히 일치 — 룰 yml 을 단일 출처로 본다.

// swapLockCollector — swap_lock 발급 메트릭.
type swapLockCollector struct {
	src             *AtomicSwapLockMetrics
	requestsDesc    *prometheus.Desc
	successesDesc   *prometheus.Desc
	partialFailDesc *prometheus.Desc // labels: stage
	revokeDesc      *prometheus.Desc // labels: outcome
}

func newSwapLockCollector(src *AtomicSwapLockMetrics) *swapLockCollector {
	return &swapLockCollector{
		src: src,
		requestsDesc: prometheus.NewDesc(
			"wtg_swap_lock_requests_total",
			"swap/lock POST 요청 누적 (성공/실패 모두 포함)",
			nil, nil),
		successesDesc: prometheus.NewDesc(
			"wtg_swap_lock_successes_total",
			"swap/lock 성공 응답 누적 (200 OK)",
			nil, nil),
		partialFailDesc: prometheus.NewDesc(
			"wtg_swap_lock_partial_failures_total",
			"swap/lock 단계별 부분실패 누적. stage=near|far|swap_index",
			[]string{"stage"}, nil),
		revokeDesc: prometheus.NewDesc(
			"wtg_swap_lock_revoke_total",
			"부분실패 시 best-effort revoke 결과 누적. outcome=ok|fail",
			[]string{"outcome"}, nil),
	}
}

func (c *swapLockCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.requestsDesc
	ch <- c.successesDesc
	ch <- c.partialFailDesc
	ch <- c.revokeDesc
}

func (c *swapLockCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.src.Snapshot()
	ch <- prometheus.MustNewConstMetric(c.requestsDesc, prometheus.CounterValue, float64(s.Requests))
	ch <- prometheus.MustNewConstMetric(c.successesDesc, prometheus.CounterValue, float64(s.Successes))
	ch <- prometheus.MustNewConstMetric(c.partialFailDesc, prometheus.CounterValue, float64(s.FailNear), "near")
	ch <- prometheus.MustNewConstMetric(c.partialFailDesc, prometheus.CounterValue, float64(s.FailFar), "far")
	ch <- prometheus.MustNewConstMetric(c.partialFailDesc, prometheus.CounterValue, float64(s.FailSwapIndex), "swap_index")
	ch <- prometheus.MustNewConstMetric(c.revokeDesc, prometheus.CounterValue, float64(s.RevokeOK), "ok")
	ch <- prometheus.MustNewConstMetric(c.revokeDesc, prometheus.CounterValue, float64(s.RevokeFail), "fail")
}

// swapValidationCollector — ValidateSwap / ConsumeSwap RPC 의 status 별
// 누적 + ConsumeSwap partial-race 별도 카운터.
type swapValidationCollector struct {
	src                   *QuoteValidationServer
	validateDesc          *prometheus.Desc // labels: status
	consumeDesc           *prometheus.Desc // labels: status
	consumePartialRaceDesc *prometheus.Desc
}

func newSwapValidationCollector(src *QuoteValidationServer) *swapValidationCollector {
	return &swapValidationCollector{
		src: src,
		validateDesc: prometheus.NewDesc(
			"wtg_swap_validate_total",
			"ValidateSwap RPC status 별 누적. status=ok|not_found|expired|already_consumed|internal",
			[]string{"status"}, nil),
		consumeDesc: prometheus.NewDesc(
			"wtg_swap_consume_total",
			"ConsumeSwap RPC status 별 누적. status=ok|not_found|expired|already_consumed|internal",
			[]string{"status"}, nil),
		consumePartialRaceDesc: prometheus.NewDesc(
			"wtg_consume_swap_partial_race_total",
			"ConsumeSwap 의 2단계 race — 사전검사 OK 였는데 MarkConsumed 에서 한 leg ALREADY 발생",
			nil, nil),
	}
}

func (c *swapValidationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.validateDesc
	ch <- c.consumeDesc
	ch <- c.consumePartialRaceDesc
}

func (c *swapValidationCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.src
	emit := func(desc *prometheus.Desc, v uint64, status string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, float64(v), status)
	}
	// ValidateSwap.
	emit(c.validateDesc, s.swapValidateOK.Load(), "ok")
	emit(c.validateDesc, s.swapValidateNotFound.Load(), "not_found")
	emit(c.validateDesc, s.swapValidateExpired.Load(), "expired")
	emit(c.validateDesc, s.swapValidateConsumed.Load(), "already_consumed")
	emit(c.validateDesc, s.swapValidateInternal.Load(), "internal")
	// ConsumeSwap.
	emit(c.consumeDesc, s.swapConsumeOK.Load(), "ok")
	emit(c.consumeDesc, s.swapConsumeAlready.Load(), "already_consumed")
	emit(c.consumeDesc, s.swapConsumeNotFound.Load(), "not_found")
	emit(c.consumeDesc, s.swapConsumeExpired.Load(), "expired")
	emit(c.consumeDesc, s.swapConsumeInternal.Load(), "internal")
	ch <- prometheus.MustNewConstMetric(c.consumePartialRaceDesc, prometheus.CounterValue,
		float64(s.swapConsumePartialRace.Load()))
}

// RegisterSwapMetrics — swap_lock 발급 + ValidateSwap/ConsumeSwap 메트릭을
// Prometheus Registry 에 등록. src 가 nil 이면 그 그룹 skip — 부팅 시점에
// 컴포넌트 가용성에 따라 선택적 노출 (forward/lock 만 활성 환경 등).
func RegisterSwapMetrics(reg *metrics.Registry, lock *AtomicSwapLockMetrics, validator *QuoteValidationServer) error {
	if lock != nil {
		if err := reg.Register(newSwapLockCollector(lock)); err != nil {
			return err
		}
	}
	if validator != nil {
		if err := reg.Register(newSwapValidationCollector(validator)); err != nil {
			return err
		}
	}
	return nil
}
