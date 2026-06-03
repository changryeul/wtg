package admin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PromProxyDeps — Prometheus 쿼리 proxy 핸들러 공통 의존성.
//
// admin UI 의 "운영 모니터링" 페이지가 직접 Prometheus 에 호출하지 못하므로
// (CORS + 인증 + 네트워크 분리), admin 서비스가 단일 endpoint 로 proxy.
// 호출 가능 path 는 /api/v1/query, /api/v1/query_range 두 개만.
type PromProxyDeps struct {
	BaseURL string // 예: "http://prometheus:9090". 비면 503
	Logger  *slog.Logger
	Client  *http.Client // nil 이면 default
}

func (d *PromProxyDeps) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// PromQuery — GET /v1/admin/prom-query?path=query&query=<promql>[&time=...]
// 또는 path=query_range 의 경우 step + start + end.
//
// path 화이트리스트: query, query_range. 그 외는 400.
// PromURL 미설정 시 503 (운영자가 --prom-url 채우라는 안내).
func PromQuery(deps *PromProxyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.BaseURL == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "no_prom",
				"Prometheus URL 미구성 (admin --prom-url 또는 WTG_ADMIN_PROM_URL)")
			return
		}
		q := r.URL.Query()
		path := q.Get("path")
		switch path {
		case "query", "query_range":
		default:
			writeJSONError(w, http.StatusBadRequest, "bad_path",
				"path 는 query 또는 query_range 만 허용 (받은: "+path+")")
			return
		}
		promQuery := strings.TrimSpace(q.Get("query"))
		if promQuery == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "query 비어있음")
			return
		}
		// 안전 추가 — POST 거부 (write 차단). Prometheus 의 /api/v1/admin/* 도
		// 우리 path 화이트리스트로 못 통과.
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method", "GET 만 허용")
			return
		}

		// upstream URL 빌드.
		upstreamURL, err := buildPromURL(deps.BaseURL, path, q)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "url", err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "request", err.Error())
			return
		}
		resp, err := deps.client().Do(req)
		if err != nil {
			deps.Logger.Warn("prometheus 호출 실패",
				slog.String("url", upstreamURL), slog.Any("error", err))
			writeJSONError(w, http.StatusBadGateway, "upstream", err.Error())
			return
		}
		defer resp.Body.Close()
		// content-type + body 그대로 전달.
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// buildPromURL — base + /api/v1/<path>?<query+time/start/end/step>.
// query 파라미터는 그대로 전달 (Prometheus 의 query 파싱이 한 단계 더).
func buildPromURL(base, path string, q url.Values) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("base parse: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/" + path

	out := url.Values{}
	out.Set("query", q.Get("query"))
	// 시간 옵션 — Prometheus 가 빈 값 받으면 instant query 처리.
	for _, k := range []string{"time", "start", "end", "step"} {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	u.RawQuery = out.Encode()
	return u.String(), nil
}
