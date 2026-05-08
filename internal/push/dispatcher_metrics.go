package push

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/winwaysystems/wtg/pkg/metrics"
)

// registerDispatcherMetrics 는 dispatcher 의 누적 카운터를 Prometheus gauge 로
// 노출한다. counter 가 아닌 gauge 인 이유 — dispatcher 의 atomic counter 가
// process lifetime 에 대해 단조 증가하지만, prometheus client 는 gauge 로 읽으면
// 그 값을 어떤 시점이든 노출 가능하기 때문 (counter 는 prometheus 클라이언트가
// 직접 Inc 해야 하는데, dispatcher 가 이미 atomic 으로 관리해서 중복 카운트를
// 피하려고 GaugeFunc 사용).
//
// 노출 메트릭:
//
//	mci_push_dispatcher_received_total   broker 에서 받은 unsolicited 총수
//	mci_push_dispatcher_delivered_total  ws Send 까지 도달한 fan-out 합
//	mci_push_dispatcher_dropped_total    LogonID 없거나 미등록 user drop
func registerDispatcherMetrics(reg *metrics.Registry, d *Dispatcher) {
	if reg == nil || d == nil {
		return
	}
	mk := func(name, help string, fn func() float64) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: name, Help: help}, fn,
		)
	}
	_ = reg.Register(mk(
		"mci_push_dispatcher_received_total",
		"broker 에서 받은 unsolicited 총수",
		func() float64 { return float64(d.Stats().Received) },
	))
	_ = reg.Register(mk(
		"mci_push_dispatcher_delivered_total",
		"ws Send 까지 도달한 fan-out 합 (사용자별)",
		func() float64 { return float64(d.Stats().Delivered) },
	))
	_ = reg.Register(mk(
		"mci_push_dispatcher_dropped_total",
		"LogonID 없거나 미등록 user 로 drop 된 수",
		func() float64 { return float64(d.Stats().Dropped) },
	))
}
