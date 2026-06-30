package fix

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics.go — Prometheus metrics. 자체 registry 사용 (mci-edge-fix 의
// 다른 metrics 와 격리). /metrics endpoint 는 cmd/mci-edge-fix/stats.go 의
// stats listener 가 노출.
//
// counter:
//
//	mci_edge_fix_logon_total{result="ok|reject"}
//	mci_edge_fix_orders_received_total
//	mci_edge_fix_orders_forwarded_total
//	mci_edge_fix_orders_rejected_total
//	mci_edge_fix_exec_report_sent_total
//	mci_edge_fix_exec_report_rejected_total
//	mci_edge_fix_reload_total{result="ok|fail"}
//
// gauge:
//
//	mci_edge_fix_active_sessions

// metricsState — 전역 Prometheus state. once 로 한 번만 초기화 (test 의
// 다중 Server 생성 race 회피).
type metricsState struct {
	reg *prometheus.Registry

	logon           *prometheus.CounterVec
	ordersReceived  prometheus.Counter
	ordersForwarded prometheus.Counter
	ordersRejected  prometheus.Counter
	execSent        prometheus.Counter
	execRejected    prometheus.Counter
	reload          *prometheus.CounterVec
	activeSessions  prometheus.Gauge
}

var (
	metricsOnce   sync.Once
	metricsCached *metricsState
)

func getMetrics() *metricsState {
	metricsOnce.Do(func() {
		reg := prometheus.NewRegistry()
		s := &metricsState{reg: reg}

		s.logon = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mci_edge_fix_logon_total",
			Help: "FIX session Logon 누적 (result=ok|reject)",
		}, []string{"result"})
		s.ordersReceived = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mci_edge_fix_orders_received_total",
			Help: "수신한 FIX 주문 (NewOrderSingle/Cancel/Replace)",
		})
		s.ordersForwarded = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mci_edge_fix_orders_forwarded_total",
			Help: "/v1/tx forward 성공",
		})
		s.ordersRejected = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mci_edge_fix_orders_rejected_total",
			Help: "변환/forward 실패",
		})
		s.execSent = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mci_edge_fix_exec_report_sent_total",
			Help: "ExecutionReport (35=8) drop copy 송신 성공",
		})
		s.execRejected = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mci_edge_fix_exec_report_rejected_total",
			Help: "ExecutionReport 송신 실패 (session 미활성 / 변환 실패)",
		})
		s.reload = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mci_edge_fix_reload_total",
			Help: "SIGHUP / API reload 누적 (result=ok|fail)",
		}, []string{"result"})
		s.activeSessions = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mci_edge_fix_active_sessions",
			Help: "현재 Logon 통과한 session 수",
		})

		reg.MustRegister(s.logon, s.ordersReceived, s.ordersForwarded,
			s.ordersRejected, s.execSent, s.execRejected, s.reload, s.activeSessions)

		// CounterVec 는 label child 가 첫 inc 되기 전엔 노출 안 됨. 0 touch 로
		// 초기 노출 보장 — Prometheus dashboard 의 rate() 가 처음부터 잡힘.
		s.logon.WithLabelValues("ok").Add(0)
		s.logon.WithLabelValues("reject").Add(0)
		s.reload.WithLabelValues("ok").Add(0)
		s.reload.WithLabelValues("fail").Add(0)

		metricsCached = s
	})
	return metricsCached
}

// MetricsHandler — /metrics endpoint 의 http.Handler. cmd/mci-edge-fix 가
// stats listener 에 mount.
func MetricsHandler() http.Handler {
	m := getMetrics()
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// syncMetrics — Stats snapshot 의 값을 Prometheus gauge 와 동기화 (atomic
// counter 의 즉시 반영). counter 는 ++ 시점에 직접 inc 호출.
func syncMetrics(stats Stats) {
	m := getMetrics()
	m.activeSessions.Set(float64(stats.ActiveSessions))
}
