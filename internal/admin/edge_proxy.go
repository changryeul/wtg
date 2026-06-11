package admin

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// edge_proxy.go — mci-edge-price (8083) 의 진단 endpoint forward.
//
// admin UI 의 "연결" / "Customer 검색" 페이지가 same-origin 으로 호출 → 본
// 핸들러가 upstream(mci-edge-price) 호출 → JSON 그대로 반환. mci-edge-price
// 는 DevMode 면 CORS 허용하지만 운영은 사내망 backend 라 admin proxy 가 정석.
//
// 인증: admin 의 API auth (X-WTG-User / Bearer) 를 그대로 upstream 에
// forward — DevMode 에선 X-WTG-User 헤더가 필수.

// EdgeConnectionsProxy — GET /v1/admin/edge/connections (?customer_id=&profile=).
// query string 그대로 forward.
func EdgeConnectionsProxy(edgeURL string) http.HandlerFunc {
	base := strings.TrimSuffix(edgeURL, "/")
	if base == "" {
		base = "http://127.0.0.1:8083"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		target := base + "/v1/connections"
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "build_req", err.Error())
			return
		}
		// upstream 인증 통과 — admin 클라이언트의 헤더 그대로.
		if v := r.Header.Get("X-WTG-User"); v != "" {
			req.Header.Set("X-WTG-User", v)
		}
		if v := r.Header.Get("Authorization"); v != "" {
			req.Header.Set("Authorization", v)
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

// EdgePingProxy — GET /v1/admin/edge/ping — instance 살아있음 확인 (옵션).
func EdgePingProxy(edgeURL string) http.HandlerFunc {
	base := strings.TrimSuffix(edgeURL, "/")
	if base == "" {
		base = "http://127.0.0.1:8083"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		// URL parse — 안정성 (잘못된 priceURL 방어).
		u, err := url.Parse(base + "/v1/ping")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "bad_url", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "build_req", err.Error())
			return
		}
		if v := r.Header.Get("X-WTG-User"); v != "" {
			req.Header.Set("X-WTG-User", v)
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
