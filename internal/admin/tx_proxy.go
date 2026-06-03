package admin

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// TxTestProxy 는 POST /v1/admin/tx-test 를 cfg.UpstreamAPIURL + /v1/tx 로 reverse-proxy 한다.
//
// 목적: WTG Control (mci-admin UI) 의 "API 테스터" 화면 안에서 매매 transaction 의
// echo round-trip (mci-api 의 /v1/tx) 을 직접 시도할 수 있게 한다. SPA 와 mci-api 가
// 다른 origin 이라 브라우저에서 직접 fetch 가 막히는 문제를 같은 origin proxy 로 우회.
//
// DevMode 에서만 의미 있는 우회 path 이므로 UpstreamAPIURL 이 비어있으면 503 으로 응답.
// 운영에선 별도 외부 노출 layer (mci-edge-api) 가 그 역할을 담당하므로 활성화 금지.
//
// 인증/IP 화이트리스트는 admin chain (apiChain) 이 이미 통과시킨 상태이며,
// X-WTG-User / Authorization 헤더는 reverse proxy 가 그대로 mci-api 로 전달한다 —
// mci-api 의 DevMode 미들웨어가 동일한 헤더 모델을 신뢰하므로 추가 변환 불필요.
func TxTestProxy(upstreamURL string, logger *slog.Logger) http.HandlerFunc {
	if upstreamURL == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"upstream_disabled","message":"--upstream-api 가 설정되지 않음. mci-admin 기동 시 --upstream-api=http://127.0.0.1:8080 추가 필요."}`))
		}
	}
	target, err := url.Parse(upstreamURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream_invalid"}`))
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		// /v1/admin/tx-test → /v1/tx (mci-api 의 transaction passthrough endpoint)
		req.URL.Path = "/v1/tx"
		req.URL.RawPath = ""
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("tx-test proxy 실패",
			slog.String("upstream", upstreamURL),
			slog.Any("err", err),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream_unreachable","message":"mci-api 에 도달할 수 없음"}`))
	}

	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}

// UpstreamProxy — generic mci-api reverse proxy. (path rewrite) 만 다르고
// TxTestProxy 와 동일 패턴. 추가 endpoint 들이 모두 같은 upstream/dev 정책을
// 공유하므로 별도 핸들러 만들 필요 X.
func UpstreamProxy(upstreamURL, upstreamPath string, logger *slog.Logger) http.HandlerFunc {
	if upstreamURL == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"upstream_disabled","message":"--upstream-api 미설정"}`))
		}
	}
	target, err := url.Parse(upstreamURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream_invalid"}`))
		}
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.URL.Path = upstreamPath
		req.URL.RawPath = ""
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("upstream proxy 실패",
			slog.String("upstream", upstreamURL),
			slog.String("path", upstreamPath),
			slog.Any("err", err))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream_unreachable"}`))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}
