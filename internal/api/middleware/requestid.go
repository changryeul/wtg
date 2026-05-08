package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// 요청별 trace id (X-Request-ID) 를 context 에 주입.
// 클라이언트가 보낸 X-Request-ID 가 있으면 사용, 없으면 자동 생성.

type ctxKey int

const requestIDKey ctxKey = 1

// RequestIDFromContext 는 context 에 주입된 request id 를 반환 (없으면 빈 문자열).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// RequestID 미들웨어는 요청마다 X-Request-ID 헤더를 검사하고 (없으면 생성)
// context 에 저장한다. 응답 헤더에도 동일 값을 echo back 한다.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = newID()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// newID 는 16 hex char (8바이트) 의 랜덤 ID 를 생성한다.
// crypto/rand 사용 — 충돌 가능성 사실상 0.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
