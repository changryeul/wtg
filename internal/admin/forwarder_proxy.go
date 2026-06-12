package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// ForwarderStatsProxy — GET /v1/admin/forwarder/stats → quote-forwarder /stats.
//
// admin UI 의 "forwarder 통계" 페이지가 same-origin 으로 호출. forwarder 는
// CORS 헤더 안 붙이므로 admin proxy 가 우회.
func ForwarderStatsProxy(fwdURL string) http.HandlerFunc {
	base := strings.TrimSuffix(fwdURL, "/")
	if base == "" {
		base = "http://127.0.0.1:9091"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stats", nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "build_req", err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream", err.Error())
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
