package middleware

import (
	"net/http"
)

// BearerFromQuery 는 URL 쿼리의 access_token 을 Authorization 헤더로 옮긴다.
//
// WebSocket 클라이언트는 브라우저 WebSocket API 가 setRequestHeader 를 지원하지
// 않아 Authorization 헤더를 보낼 수 없다. 일반적 우회: ws://...?access_token=xxx.
//
// 이 미들웨어는 Auth 보다 먼저 적용되어, Auth 미들웨어는 그대로 헤더만 본다 —
// 인증 경로 분리를 깨지 않는다.
//
// 동작:
//   - Authorization 헤더가 이미 채워져 있으면 무시 (헤더 우선)
//   - 그 외 ?access_token=xxx 가 있으면 "Bearer xxx" 헤더 주입
//   - 보안: 토큰이 access_log / referer 등에 새는 것을 막기 위해, 헤더 주입 후
//     URL 쿼리에서 access_token 키를 제거 (다음 핸들러는 토큰을 URL 에서 보지 못함)
func BearerFromQuery() Middleware {
	const param = "access_token"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "" {
				if tok := r.URL.Query().Get(param); tok != "" {
					r.Header.Set("Authorization", "Bearer "+tok)
					// 쿼리에서 토큰 제거 (URL 노출 최소화).
					q := r.URL.Query()
					q.Del(param)
					r.URL.RawQuery = q.Encode()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
