package md

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsHandler — /metrics endpoint. Phase A 는 default registerer 사용 —
// edge-fix 는 자체 registry 를 씀 (다른 프로세스라 충돌 없음).
func MetricsHandler() http.Handler {
	_ = getMetrics() // ensure lazy init
	return promhttp.Handler()
}

// Prometheus metric — service-scoped 단일 인스턴스. lazy 초기화 (테스트 병렬 안전).
type mdMetrics struct {
	logon           *prometheus.CounterVec // labels: result=ok|reject
	activeSessions  prometheus.Gauge
	mdrReceived     prometheus.Counter
	mdrRejected     *prometheus.CounterVec // labels: reason
	snapshotSent    *prometheus.CounterVec // labels: symbol
	symbolMissing   *prometheus.CounterVec // labels: symbol (요청은 왔지만 static provider 에 없음)
	reload          *prometheus.CounterVec // labels: result=ok|fail
}

var (
	metricsOnce sync.Once
	metrics     *mdMetrics
)

func getMetrics() *mdMetrics {
	metricsOnce.Do(func() {
		metrics = &mdMetrics{
			logon: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "mci_edge_md_logon_total",
				Help: "MD FIX Logon 결과 누적 (result=ok|reject)",
			}, []string{"result"}),
			activeSessions: promauto.NewGauge(prometheus.GaugeOpts{
				Name: "mci_edge_md_active_sessions",
				Help: "MD FIX 로그온 통과 세션 수",
			}),
			mdrReceived: promauto.NewCounter(prometheus.CounterOpts{
				Name: "mci_edge_md_mdr_received_total",
				Help: "MarketDataRequest (35=V) 수신 누적",
			}),
			mdrRejected: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "mci_edge_md_mdr_rejected_total",
				Help: "MarketDataRequest 거부 누적 (reason)",
			}, []string{"reason"}),
			snapshotSent: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "mci_edge_md_snapshot_sent_total",
				Help: "MarketDataSnapshotFullRefresh (35=W) 송신 누적 (symbol)",
			}, []string{"symbol"}),
			symbolMissing: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "mci_edge_md_symbol_missing_total",
				Help: "요청 symbol 이 static quote provider 에 없어 skip 된 횟수",
			}, []string{"symbol"}),
			reload: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "mci_edge_md_reload_total",
				Help: "SIGHUP reload 결과 (result=ok|fail)",
			}, []string{"result"}),
		}
	})
	return metrics
}
