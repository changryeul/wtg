package admin

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// wsmon_proxy.go — /v1/admin/wsmon/{svc}/... → 각 서비스 ws/HTTP reverse-proxy.
//
// WS 모니터는 과거 브라우저가 각 서비스 포트 (8081/8083/...) 로 직접 연결하는
// 구조라, 원격 운영 (사무실 → EC2) 에서는 포트마다 방화벽/SG 를 열어야 했다.
// admin(9090) 을 경유하면 이미 열린 관리 포트 하나로 모든 endpoint 모니터링이
// 가능하다. httputil.ReverseProxy 가 WebSocket Upgrade 를 그대로 통과시키므로
// ws 전용 코드는 불필요. 인증 query (x_wtg_user / access_token) 는 URL 에
// 실려 upstream 까지 전달된다 (admin 자체 인증은 BearerFromQuery/UserFromQuery).
//
// 대상은 --wsmon-targets ("name=baseURL,...") 로 재정의 가능 — 다중 호스트
// 배치에서 각 서비스가 다른 호스트에 있을 때 사용.

// defaultWsmonTargets — 단일 호스트 배치 기준. id 는 UI WSMON_ENDPOINTS 와 일치.
func defaultWsmonTargets() []MciTarget {
	return []MciTarget{
		{Name: "mci-push", URL: "http://127.0.0.1:8081"},
		{Name: "edge-price", URL: "http://127.0.0.1:8083"},
		{Name: "edge-push", URL: "http://127.0.0.1:8084"},
		{Name: "mci-chart", URL: "http://127.0.0.1:8086"},
		{Name: "edge-chart", URL: "http://127.0.0.1:8087"},
	}
}

// WsmonProxy — GET /v1/admin/wsmon/{svc}/{rest...} 핸들러.
// svc 가 대상 목록에 없으면 404. upstream 불가 시 502 (ErrorHandler).
func WsmonProxy(targetsSpec string, logger *slog.Logger) http.HandlerFunc {
	targets := parseMciTargets(targetsSpec)
	if len(targets) == 0 {
		targets = defaultWsmonTargets()
	}
	proxies := make(map[string]*httputil.ReverseProxy, len(targets))
	for _, t := range targets {
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			if logger != nil {
				logger.Warn("wsmon-proxy: 대상 URL 무시", slog.String("name", t.Name), slog.String("url", t.URL))
			}
			continue
		}
		name := t.Name
		p := httputil.NewSingleHostReverseProxy(u)
		origDirector := p.Director
		p.Director = func(r *http.Request) {
			origDirector(r)
			// /v1/admin/wsmon/<svc>/<rest> → /<rest> (query 는 그대로 보존).
			prefix := "/v1/admin/wsmon/" + name
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			r.Host = u.Host
		}
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if logger != nil {
				logger.Warn("wsmon-proxy: upstream 실패",
					slog.String("svc", name), slog.Any("err", err))
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream unreachable: " + name))
		}
		proxies[name] = p
	}
	return func(w http.ResponseWriter, r *http.Request) {
		svc := r.PathValue("svc")
		p, ok := proxies[svc]
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.ServeHTTP(w, r)
	}
}
