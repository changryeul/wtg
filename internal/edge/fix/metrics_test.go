package fix

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /metrics endpoint 가 prometheus exposition format 으로 노출되는지.
func TestMetricsHandler_Exposition(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", MetricsHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	r, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status=%d", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	text := string(body)

	for _, want := range []string{
		"mci_edge_fix_logon_total",
		"mci_edge_fix_orders_received_total",
		"mci_edge_fix_orders_forwarded_total",
		"mci_edge_fix_orders_rejected_total",
		"mci_edge_fix_exec_report_sent_total",
		"mci_edge_fix_exec_report_rejected_total",
		"mci_edge_fix_reload_total",
		"mci_edge_fix_active_sessions",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics 에 %q 없음", want)
		}
	}
}
