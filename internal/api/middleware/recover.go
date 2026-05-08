package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover 는 핸들러에서 발생한 panic 을 잡아서 500 으로 변환한다.
// stack trace 는 logger 에 ERROR 레벨로 기록된다.
//
// 가장 바깥쪽 미들웨어로 두는 것이 안전하다.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.ErrorContext(r.Context(), "panic 복구",
						slog.Any("panic", rec),
						slog.String("path", r.URL.Path),
						slog.String("method", r.Method),
						slog.String("stack", string(debug.Stack())),
					)
					http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
