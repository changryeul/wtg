package admin

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// tcp_gw.go — GET /v1/admin/tcp-gw/stats → mci-edge-tcp 의 stats HTTP proxy.
//
// UI "TCP 게이트웨이" 페이지가 소비. 브라우저가 edge-tcp stats 포트 (5022)
// 에 직접 붙게 하면 원격 운영에서 포트마다 방화벽/SG 를 열어야 하는 함정
// (WS 모니터에서 겪음) 이 있어 admin 경유로 통일한다.

// TcpGwStats — statsURL 은 edge-tcp 의 stats base (예: http://127.0.0.1:5022).
func TcpGwStats(statsURL string, logger *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: 3 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		if statsURL == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "disabled",
				"--tcp-gw-stats 미설정 — mci-edge-tcp stats 주소를 지정해야 함")
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, statsURL+"/stats", nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "bad_url", err.Error())
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			if logger != nil {
				logger.Warn("tcp-gw stats 조회 실패", slog.Any("error", err))
			}
			writeJSONError(w, http.StatusBadGateway, "unreachable",
				"mci-edge-tcp stats 도달 불가 — 게이트웨이 미기동이거나 --tcp-gw-stats 주소 확인")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
