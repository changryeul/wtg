package middleware

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// AccessLog 미들웨어는 모든 요청을 구조화 access log 로 출력한다.
// 응답 status code 와 처리 시간 포함.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rw, r)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Duration("dur", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
				slog.String("ua", r.UserAgent()),
				slog.String("rid", RequestIDFromContext(r.Context())),
			)
		})
	}
}

// statusRecorder 는 응답 status code 를 캡처하기 위한 wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write 는 WriteHeader 를 호출하지 않고 곧바로 본문을 보내는 핸들러를
// 위해 default 200 으로 표시한다 (표준 net/http 동작과 동일).
func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(p)
}

// Hijack 은 WebSocket upgrade 가 필요한 ResponseWriter wrapping 을 통과시킨다.
// 미구현 시 gorilla/websocket 의 Upgrader 가 "response does not implement
// http.Hijacker" 로 ws handshake 를 실패시킨다.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("statusRecorder: 하위 ResponseWriter 가 http.Hijacker 미지원")
	}
	return h.Hijack()
}
