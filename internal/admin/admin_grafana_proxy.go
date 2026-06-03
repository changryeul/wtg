package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// GrafanaProxyDeps — Grafana alert rules proxy 핸들러 의존성.
//
// admin UI 의 "운영 모니터링" 페이지가 firing alert 를 직접 표시하기 위해
// admin 서비스가 Grafana 의 alert state API 를 proxy.
//
// 호출 path: /api/prometheus/grafana/api/v1/rules (read-only).
// 다른 path 노출 X — admin/datasources 같은 write 권한 차단.
type GrafanaProxyDeps struct {
	BaseURL  string // 예: "http://grafana:3000"
	Username string // 옵션 — Basic auth (예: "admin")
	Password string // 옵션
	Logger   *slog.Logger
	Client   *http.Client // nil 이면 default
}

func (d *GrafanaProxyDeps) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// GrafanaConfig — GET /v1/admin/grafana-config.
// UI 가 alert deep link 빌드용으로 base URL 만 알면 충분. 인증 정보는 노출 X.
func GrafanaConfig(deps *GrafanaProxyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"base_url": deps.BaseURL,
		})
	}
}

// GrafanaAlerts — GET /v1/admin/grafana-alerts.
// Grafana 의 alert rule 상태를 그대로 반환 (운영 UI 가 파싱).
func GrafanaAlerts(deps *GrafanaProxyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.BaseURL == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "no_grafana",
				"Grafana URL 미구성 (admin --grafana-url 또는 WTG_ADMIN_GRAFANA_URL)")
			return
		}
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method", "GET 만 허용")
			return
		}

		upstreamURL := strings.TrimRight(deps.BaseURL, "/") +
			"/api/prometheus/grafana/api/v1/rules"

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "request", err.Error())
			return
		}
		if deps.Username != "" {
			req.SetBasicAuth(deps.Username, deps.Password)
		}
		resp, err := deps.client().Do(req)
		if err != nil {
			deps.Logger.Warn("grafana 호출 실패",
				slog.String("url", upstreamURL), slog.Any("error", err))
			writeJSONError(w, http.StatusBadGateway, "upstream", err.Error())
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
