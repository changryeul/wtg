package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryHandlerExposesStandardMetrics(t *testing.T) {
	reg := NewRegistry()
	// 한 번씩 사용 — Prometheus 는 첫 observation 후에야 metric 노출.
	reg.ObserveBrokerCall("svc", 1, "ok", time.Millisecond)
	reg.SetSubscriberCount("svc", "ws", 0)
	mw := HTTPMiddleware(reg, "svc")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reg.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	for _, want := range []string{
		"wtg_http_requests_total",
		"wtg_http_request_duration_seconds",
		"wtg_broker_call_total",
		"wtg_subscribers",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 출력에 %q 없음", want)
		}
	}
}

func TestHTTPMiddlewareIncrementsCounter(t *testing.T) {
	reg := NewRegistry()
	mw := HTTPMiddleware(reg, "test-svc")

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	h.ServeHTTP(rr, req)

	// /metrics 출력에 카운터가 잡혀야 함.
	mr := httptest.NewRecorder()
	mreq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reg.Handler().ServeHTTP(mr, mreq)

	body := mr.Body.String()
	if !strings.Contains(body, `wtg_http_requests_total{method="POST",path="/v1/x",service="test-svc",status="418"} 1`) {
		t.Errorf("counter 라인 없음:\n%s", body)
	}
}

func TestHTTPMiddlewareDefaultStatus200(t *testing.T) {
	reg := NewRegistry()
	mw := HTTPMiddleware(reg, "test-svc")
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/y", nil))

	mr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mr.Body.String(), `status="200"`) {
		t.Error("기본 200 status 미기록")
	}
}

func TestObserveBrokerCall(t *testing.T) {
	reg := NewRegistry()
	reg.ObserveBrokerCall("mci-api", 150, "ok", 5*time.Millisecond)
	reg.ObserveBrokerCall("mci-api", 150, "error", 2*time.Millisecond)

	mr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mr.Body.String()
	if !strings.Contains(body, `wtg_broker_call_total{result="ok",service="mci-api",subc="150"} 1`) {
		t.Errorf("broker_call_total ok 미기록")
	}
	if !strings.Contains(body, `wtg_broker_call_total{result="error",service="mci-api",subc="150"} 1`) {
		t.Errorf("broker_call_total error 미기록")
	}
}

func TestSetSubscriberCount(t *testing.T) {
	reg := NewRegistry()
	reg.SetSubscriberCount("mci-edge-push", "ws", 42)

	mr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mr.Body.String(), `wtg_subscribers{kind="ws",service="mci-edge-push"} 42`) {
		t.Error("subscribers 게이지 미반영")
	}
}
