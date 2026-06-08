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
// 노출 메트릭 (총수 + 사유별 + send 실패):
//
//	mci_push_dispatcher_received_total       broker 에서 받은 unsolicited 총수
//	mci_push_dispatcher_delivered_total      ws Send 까지 도달한 fan-out 합
//	mci_push_dispatcher_dropped_total        sent=0 인 fan-out 총합 (아래 4 사유 합)
//	mci_push_dispatcher_drop_unsupp_total    Func 가 Cast/Push/Signal 아님
//	mci_push_dispatcher_drop_envelope_total  json marshal 실패
//	mci_push_dispatcher_drop_unknown_user_total  LogonID 명시인데 conn 없음
//	mci_push_dispatcher_drop_no_broadcast_total  LogonID 빈값 + conn 0
//	mci_push_dispatcher_send_failed_total    fan-out 안 일부 conn send 실패
//	mci_push_dispatcher_recv_broker_total    broker subscribe 로 받은 unsolicited 수 (Phase 2.5)
//	mci_push_dispatcher_recv_http_total      POST /v1/internal/push (HTTP) 로 받은 수 (Phase 2.5)
//	mci_push_dispatcher_drop_inject_full_total  HTTP inject channel full → drop (Phase 2.5)
func registerDispatcherMetrics(reg *metrics.Registry, d *Dispatcher) {
	if reg == nil || d == nil {
		return
	}
	mk := func(name, help string, fn func() float64) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: name, Help: help}, fn,
		)
	}
	register := func(name, help string, fn func(Stats) uint64) {
		_ = reg.Register(mk(name, help, func() float64 { return float64(fn(d.Stats())) }))
	}
	register("mci_push_dispatcher_received_total", "broker 에서 받은 unsolicited 총수",
		func(s Stats) uint64 { return s.Received })
	register("mci_push_dispatcher_delivered_total", "ws Send 까지 도달한 fan-out 합",
		func(s Stats) uint64 { return s.Delivered })
	register("mci_push_dispatcher_dropped_total", "sent=0 인 fan-out 총합 (사유별 분기 = 아래 4 합)",
		func(s Stats) uint64 { return s.Dropped })
	register("mci_push_dispatcher_drop_unsupp_total", "Func 가 FCCast/FCPush/FCSignal 아님",
		func(s Stats) uint64 { return s.DropUnsupp })
	register("mci_push_dispatcher_drop_envelope_total", "envelope JSON marshal 실패",
		func(s Stats) uint64 { return s.DropEnvelope })
	register("mci_push_dispatcher_drop_unknown_user_total", "LogonID 명시 됐는데 user 의 conn 없음",
		func(s Stats) uint64 { return s.DropUnknownUser })
	register("mci_push_dispatcher_drop_no_broadcast_total", "LogonID 빈값인데 등록된 conn 0",
		func(s Stats) uint64 { return s.DropNoBroadcast })
	register("mci_push_dispatcher_send_failed_total", "fan-out 안 일부 conn send 실패 (slow/closed)",
		func(s Stats) uint64 { return s.SendFailed })
	// Phase 2.5 — source 비교 + HTTP inject 백프레셔 가시화.
	register("mci_push_dispatcher_recv_broker_total", "broker subscribe 로 받은 unsolicited 수",
		func(s Stats) uint64 { return s.RecvBroker })
	register("mci_push_dispatcher_recv_http_total", "POST /v1/internal/push (HTTP) 로 받은 수",
		func(s Stats) uint64 { return s.RecvHTTP })
	register("mci_push_dispatcher_drop_inject_full_total", "HTTP inject channel full 로 drop 된 수",
		func(s Stats) uint64 { return s.DropInjectFull })
}
