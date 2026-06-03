// Package metrics 는 WTG 의 공용 Prometheus metrics 모음.
//
// 모든 mci-* 서비스가 공유하는 표준 메트릭 (HTTP 요청수, 지연, 에러율 등) 과
// 각 서비스가 추가 등록할 수 있는 helper 를 제공한다.
//
// 사용:
//
//	reg := metrics.NewRegistry()
//	mux.Handle("/metrics", reg.Handler())
//	mw := metrics.HTTPMiddleware(reg, "mci-api")
//	chain := middleware.Chain(mux, mw, ...)
package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry 는 prometheus.Registry wrapper + 표준 collectors.
type Registry struct {
	reg *prometheus.Registry

	httpReqTotal     *prometheus.CounterVec
	httpReqDuration  *prometheus.HistogramVec
	httpInFlight     *prometheus.GaugeVec
	brokerCallTotal  *prometheus.CounterVec
	brokerCallLat    *prometheus.HistogramVec
	subscriberCount  *prometheus.GaugeVec
	quoteIDOpTotal   *prometheus.CounterVec
	quoteIDOpLat     *prometheus.HistogramVec
	quoteIDBatchSize *prometheus.HistogramVec
	quoteIDBatchLat  *prometheus.HistogramVec

	// AsyncRegistry 카운터 — v1.19.
	asyncEnqueued *prometheus.CounterVec
	asyncDropped  *prometheus.CounterVec
	asyncWritten  *prometheus.CounterVec
	asyncFailed   *prometheus.CounterVec

	// Rate limit 카운터 — path-aware RuleSet 결과. v1.20.
	rateLimitAllowed *prometheus.CounterVec
	rateLimitDenied  *prometheus.CounterVec

	// Broker connection lifecycle — P7-B.
	brokerReconnects      *prometheus.CounterVec
	brokerReconnectDur    *prometheus.HistogramVec
	brokerInflightAborted *prometheus.CounterVec
	brokerHeartbeatTO     *prometheus.CounterVec
	brokerDisconnects     *prometheus.CounterVec

	customCollectors []prometheus.Collector
}

// NewRegistry 는 표준 collectors 를 등록한 Registry 를 만든다.
//
// 자동 등록 메트릭:
//   - go_* (Go runtime)
//   - process_* (process collector)
//   - wtg_http_requests_total{service,method,path,status}
//   - wtg_http_request_duration_seconds{service,method,path}
//   - wtg_http_inflight{service}
//   - wtg_broker_call_total{service,subc,result}
//   - wtg_broker_call_duration_seconds{service,subc}
//   - wtg_subscribers{service,kind}
func NewRegistry() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}

	r.reg.MustRegister(collectors.NewGoCollector())
	r.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	r.httpReqTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_http_requests_total",
			Help: "HTTP 요청 총 건수.",
		},
		[]string{"service", "method", "path", "status"},
	)
	r.httpReqDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_http_request_duration_seconds",
			Help:    "HTTP 요청 처리 시간 (초).",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms..16s
		},
		[]string{"service", "method", "path"},
	)
	r.httpInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wtg_http_inflight",
			Help: "현재 처리 중인 HTTP 요청 수.",
		},
		[]string{"service"},
	)
	r.brokerCallTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_broker_call_total",
			Help: "MyMQ broker call 총 건수.",
		},
		[]string{"service", "subc", "result"},
	)
	r.brokerCallLat = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_broker_call_duration_seconds",
			Help:    "MyMQ broker call 처리 시간 (초).",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
		},
		[]string{"service", "subc"},
	)
	r.subscriberCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wtg_subscribers",
			Help: "활성 구독자 수 (ws / gRPC).",
		},
		[]string{"service", "kind"},
	)

	// QuoteID RPC metrics — op ∈ {validate, batch_validate, mark_consumed,
	// batch_mark_consumed}. batch_* 만 batch_size / batch_duration 채워짐.
	r.quoteIDOpTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_quoteid_op_total",
			Help: "QuoteID RPC 총 건수 (per-item; batch 의 N 항목은 N 건).",
		},
		[]string{"service", "op", "status"},
	)
	r.quoteIDOpLat = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_quoteid_op_duration_seconds",
			Help:    "QuoteID 단일 RPC (or batch 의 wallclock) 처리 시간.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 16), // 100us..3.3s
		},
		[]string{"service", "op"},
	)
	r.quoteIDBatchSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_quoteid_batch_size",
			Help:    "Batch RPC 의 항목 수.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		},
		[]string{"service", "op"},
	)
	r.quoteIDBatchLat = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_quoteid_batch_duration_seconds",
			Help:    "Batch RPC wallclock — 전체 batch 처리 시간.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 16),
		},
		[]string{"service", "op"},
	)

	// AsyncRegistry 카운터 — Put 비동기 batch path.
	r.asyncEnqueued = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_quoteid_async_enqueued_total",
			Help: "AsyncRegistry: Put 이 채널에 enqueue 성공한 횟수.",
		},
		[]string{"service"},
	)
	r.asyncDropped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_quoteid_async_dropped_total",
			Help: "AsyncRegistry: queue full 로 drop 된 record 수.",
		},
		[]string{"service"},
	)
	r.asyncWritten = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_quoteid_async_written_total",
			Help: "AsyncRegistry: worker 가 inner.PutMany 성공한 record 수.",
		},
		[]string{"service"},
	)
	r.asyncFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_quoteid_async_failed_total",
			Help: "AsyncRegistry: worker PutMany 실패 record 수.",
		},
		[]string{"service"},
	)

	// Rate limit — kind ∈ {ip, user}, rule = pattern 또는 "default".
	r.rateLimitAllowed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_ratelimit_allowed_total",
			Help: "Rate limit: 룰 매칭 통과한 요청 수.",
		},
		[]string{"service", "kind", "rule"},
	)
	r.rateLimitDenied = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_ratelimit_denied_total",
			Help: "Rate limit: 룰 초과로 거부된 요청 수 (429).",
		},
		[]string{"service", "kind", "rule"},
	)

	// Broker connection lifecycle — P7-B.
	r.brokerReconnects = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_broker_reconnects_total",
			Help: "Broker connection 재연결 성공 누적.",
		},
		[]string{"service"},
	)
	r.brokerReconnectDur = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wtg_broker_reconnect_duration_seconds",
			Help:    "Broker 끊김부터 재연결 성공까지 소요시간.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 12), // 100ms..410s
		},
		[]string{"service"},
	)
	r.brokerInflightAborted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_broker_inflight_aborted_total",
			Help: "Broker 끊김 시 failPending 으로 ErrBroker 통보된 pending RPC 수.",
		},
		[]string{"service"},
	)
	r.brokerHeartbeatTO = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_broker_heartbeat_timeout_total",
			Help: "Heartbeat watchdog 으로 connection 사망 판정 (2*interval 무소식).",
		},
		[]string{"service"},
	)
	r.brokerDisconnects = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wtg_broker_disconnects_total",
			Help: "Broker connection 끊김 누적 (재연결 시도 전 트리거).",
		},
		[]string{"service"},
	)

	r.reg.MustRegister(
		r.httpReqTotal,
		r.httpReqDuration,
		r.httpInFlight,
		r.brokerCallTotal,
		r.brokerCallLat,
		r.subscriberCount,
		r.quoteIDOpTotal,
		r.quoteIDOpLat,
		r.quoteIDBatchSize,
		r.quoteIDBatchLat,
		r.asyncEnqueued,
		r.asyncDropped,
		r.asyncWritten,
		r.asyncFailed,
		r.rateLimitAllowed,
		r.rateLimitDenied,
		r.brokerReconnects,
		r.brokerReconnectDur,
		r.brokerInflightAborted,
		r.brokerHeartbeatTO,
		r.brokerDisconnects,
	)
	return r
}

// IncRateLimit — RuleSet 매칭 결과 카운터. allowed/denied 분기.
// kind 는 "ip" 또는 "user", rule 은 룰 패턴 또는 "default".
func (r *Registry) IncRateLimit(service, kind, rule string, allowed bool) {
	if allowed {
		r.rateLimitAllowed.WithLabelValues(service, kind, rule).Inc()
		return
	}
	r.rateLimitDenied.WithLabelValues(service, kind, rule).Inc()
}

// IncBrokerDisconnect — broker connection 끊김 (재연결 직전).
func (r *Registry) IncBrokerDisconnect(service string) {
	r.brokerDisconnects.WithLabelValues(service).Inc()
}

// IncBrokerReconnect — 재연결 성공. duration 은 끊김 ~ 성공까지 wallclock.
func (r *Registry) IncBrokerReconnect(service string, duration time.Duration) {
	r.brokerReconnects.WithLabelValues(service).Inc()
	r.brokerReconnectDur.WithLabelValues(service).Observe(duration.Seconds())
}

// IncBrokerInflightAborted — failPending 으로 통보된 pending RPC 수.
func (r *Registry) IncBrokerInflightAborted(service string, count int) {
	r.brokerInflightAborted.WithLabelValues(service).Add(float64(count))
}

// IncBrokerHeartbeatTimeout — heartbeat watchdog 발화 (2*interval 무소식).
func (r *Registry) IncBrokerHeartbeatTimeout(service string) {
	r.brokerHeartbeatTO.WithLabelValues(service).Inc()
}

// IncQuoteIDAsync — AsyncRegistry 이벤트 카운터 증가.
// kind: "enqueued" / "dropped" / "written" / "failed". n 은 written/failed 시
// batch 항목 수, enqueued/dropped 는 보통 1.
func (r *Registry) IncQuoteIDAsync(service, kind string, n uint64) {
	if n == 0 {
		return
	}
	v := float64(n)
	switch kind {
	case "enqueued":
		r.asyncEnqueued.WithLabelValues(service).Add(v)
	case "dropped":
		r.asyncDropped.WithLabelValues(service).Add(v)
	case "written":
		r.asyncWritten.WithLabelValues(service).Add(v)
	case "failed":
		r.asyncFailed.WithLabelValues(service).Add(v)
	}
}

// RegisterAsyncQueueGauge — 호출 시점마다 queue 잔여를 보고하는 GaugeFunc 등록.
// 운영자가 wtg_quoteid_async_queue_len{service} 로 현재 backlog 모니터링.
// callback 은 scrape 마다 호출 — atomic.Load 같은 cheap operation 만 권장.
func (r *Registry) RegisterAsyncQueueGauge(service string, fn func() float64) error {
	g := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name:        "wtg_quoteid_async_queue_len",
			Help:        "AsyncRegistry 현재 채널 잔여 (scrape 시점).",
			ConstLabels: prometheus.Labels{"service": service},
		},
		fn,
	)
	return r.reg.Register(g)
}

// ObserveQuoteIDOp — 단일 RPC 또는 batch 안의 per-item 결과 카운터.
// status: "ok" / "not_found" / "expired" / "already_consumed" / "denied" /
//
//	"internal" / "consume_ok" / "consume_already" ...
func (r *Registry) ObserveQuoteIDOp(service, op, status string, latency time.Duration) {
	r.quoteIDOpTotal.WithLabelValues(service, op, status).Inc()
	if latency > 0 {
		r.quoteIDOpLat.WithLabelValues(service, op).Observe(latency.Seconds())
	}
}

// ObserveQuoteIDBatch — Batch RPC wallclock + 항목 수.
func (r *Registry) ObserveQuoteIDBatch(service, op string, size int, latency time.Duration) {
	r.quoteIDBatchSize.WithLabelValues(service, op).Observe(float64(size))
	r.quoteIDBatchLat.WithLabelValues(service, op).Observe(latency.Seconds())
}

// Register 는 외부 collector 를 추가 등록.
func (r *Registry) Register(c prometheus.Collector) error {
	r.customCollectors = append(r.customCollectors, c)
	return r.reg.Register(c)
}

// Handler 는 /metrics endpoint 용 http.Handler.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		Registry: r.reg,
	})
}

// ObserveBrokerCall 은 broker.Call 후 호출. result 는 "ok" / "error" / 등.
func (r *Registry) ObserveBrokerCall(service string, subc uint8, result string, dur time.Duration) {
	subcStr := strconv.Itoa(int(subc))
	r.brokerCallTotal.WithLabelValues(service, subcStr, result).Inc()
	r.brokerCallLat.WithLabelValues(service, subcStr).Observe(dur.Seconds())
}

// SetSubscriberCount 는 ws/gRPC 구독자 수 업데이트.
func (r *Registry) SetSubscriberCount(service, kind string, n int) {
	r.subscriberCount.WithLabelValues(service, kind).Set(float64(n))
}

// HTTPMiddleware 는 모든 HTTP 요청에 대해 표준 메트릭을 기록한다.
//
// path label 은 cardinality 폭증을 막기 위해 r.Pattern (Go 1.22+) 또는
// 정해진 화이트리스트만 사용. 1차 prototype 은 r.URL.Path 그대로 사용 —
// 운영에서 cardinality 모니터링 후 필요 시 매핑 추가.
func HTTPMiddleware(reg *Registry, service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reg.httpInFlight.WithLabelValues(service).Inc()
			defer reg.httpInFlight.WithLabelValues(service).Dec()

			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rw, r)

			reg.httpReqTotal.WithLabelValues(
				service, r.Method, r.URL.Path, strconv.Itoa(rw.status),
			).Inc()
			reg.httpReqDuration.WithLabelValues(
				service, r.Method, r.URL.Path,
			).Observe(time.Since(start).Seconds())
		})
	}
}

// statusRecorder 는 응답 status 캡처용.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	return r.ResponseWriter.Write(p)
}

// Hijack 은 WebSocket upgrade 가 필요한 ResponseWriter wrapping 을 통과시킨다.
// 미구현 시 gorilla/websocket Upgrader 가 ws handshake 를 실패시킨다.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics statusRecorder: 하위 ResponseWriter 가 http.Hijacker 미지원")
	}
	return h.Hijack()
}
