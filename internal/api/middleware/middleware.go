// Package middleware 는 mci-api 의 HTTP 미들웨어 모음.
//
// 표준 net/http handler chain 패턴을 따른다:
//
//	handler := middleware.Chain(mux, mw1, mw2, mw3)
//	// mw3 → mw2 → mw1 → mux 순으로 실행 (가장 마지막이 가장 바깥쪽).
package middleware

import "net/http"

// Middleware 는 표준 http.Handler 데코레이터 시그니처.
type Middleware func(http.Handler) http.Handler

// Chain 은 다수의 Middleware 를 inner handler 에 적용한다.
// 마지막 인자(가장 오른쪽)가 가장 바깥(외부 요청에 가까운) middleware 이다.
//
// 사용 예:
//
//	Chain(mux, authMW, accessLog, requestID, recover)
//	→ recover(requestID(accessLog(authMW(mux))))
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for _, mw := range mws {
		h = mw(h)
	}
	return h
}
