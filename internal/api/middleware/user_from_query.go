package middleware

import "net/http"

// UserFromQuery 는 URL 쿼리의 x_wtg_user 를 X-WTG-User 헤더로 옮긴다.
//
// 브라우저 WebSocket API 가 사용자 정의 헤더 주입을 지원하지 않아, DevMode 에서
// ws 연결할 때 X-WTG-User 헤더를 보낼 수 없다. 일반적 우회:
// ws://...?x_wtg_user=admin01.
//
// BearerFromQuery 와 짝이 되는 미들웨어 — DevMode 의 ws 연결을 위해 query 키를
// 헤더로 변환한다. 운영 (JWT) 모드에서는 BearerFromQuery 만 사용.
//
// 동작:
//   - X-WTG-User 헤더가 이미 채워져 있으면 무시 (헤더 우선)
//   - 그 외 ?x_wtg_user=xxx 가 있으면 X-WTG-User 헤더 주입
//   - access_log 에 user 가 노출되어도 무방하므로 query 에서 키를 제거하지 않는다
//     (BearerFromQuery 와 다른 점 — 토큰이 아닌 단순 식별자라 노출 위험 작음)
func UserFromQuery() Middleware {
	const param = "x_wtg_user"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(HeaderEdgeUser) == "" {
				if u := r.URL.Query().Get(param); u != "" {
					r.Header.Set(HeaderEdgeUser, u)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
