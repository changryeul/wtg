package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// price-stats / best-stats proxy — admin UI 의 "시세 통계" 페이지가 same-origin
// 으로 mci-price (:8082) 의 monitoring endpoint 를 조회할 수 있도록 forward.
//
// 운영도 동일 path — admin 인스턴스가 mci-price 와 사내망에서 통신.
// mci-price 가 CORS 헤더를 안 붙이므로 직접 fetch 불가 → admin proxy 가 우회.

// pricePathAllowlist — 안전한 GET endpoint 만 forward (write/control 차단).
var pricePathAllowlist = map[string]string{
	"price-stats":  "/v1/price-stats",
	"best-stats":   "/v1/best-stats",
	"subscribers":  "/v1/subscribers",      // gRPC stream 카탈로그 (UI 통합 N4)
	"customers":    "/v1/customers",        // 등록된 customer digest
	"backpressure": "/v1/backpressure",     // backpressure WARN history (N7)
	"swap-stats":   "/v1/quote/swap/stats", // FX swap 잠금 카운터 (S3-e)
}

// PriceStatsProxy — GET /v1/admin/price/{kind} — kind ∈ {price-stats, best-stats}.
//
// admin UI 가 fetch("/v1/admin/price/price-stats") 로 호출 → 본 핸들러가
// upstream(mci-price) 의 해당 endpoint 호출 → JSON 그대로 반환.
func PriceStatsProxy(priceURL string) http.HandlerFunc {
	base := strings.TrimSuffix(priceURL, "/")
	if base == "" {
		base = "http://127.0.0.1:8082"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		kind := r.PathValue("kind")
		path, ok := pricePathAllowlist[kind]
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "unknown_kind",
				"kind 는 price-stats 또는 best-stats 만 허용")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
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

// PriceCustomerLookupProxy — GET /v1/admin/price/customers/{customerID}
// → mci-price /v1/customers/{customerID}. path param 만 검증, query 그대로.
func PriceCustomerLookupProxy(priceURL string) http.HandlerFunc {
	base := strings.TrimSuffix(priceURL, "/")
	if base == "" {
		base = "http://127.0.0.1:8082"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		cid := r.PathValue("customerID")
		if cid == "" || strings.ContainsAny(cid, "/?#") {
			writeJSONError(w, http.StatusBadRequest, "invalid_customer_id",
				"customer_id 는 비어있지 않은 단일 path 세그먼트")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/customers/"+cid, nil)
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
