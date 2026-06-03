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

// RequestID 미들웨어는 요청마다 X-Request-ID + W3C traceparent 헤더 양쪽을
// 처리한다 — context 에 둘 다 주입, 응답 헤더 양쪽에 echo.
//
// 우선순위:
//   - traceparent 헤더 있고 valid → trace_id 16B 그대로
//   - 없으면 16B random 생성 + 응답으로 traceparent 발행 (parent_id 도 새로)
//   - X-Request-ID = trace_id 의 앞 8B hex (호환). 헤더 입력 X-Request-ID
//     가 있어도 traceparent 의 trace_id 가 truth.
//
// docs/broker-tracing.md 와 호환 — context 의 traceID 가 mqhdr.trcid 로
// 그대로 전달되어 broker / 매매 AP / WTG 의 cross-service correlation.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var tp TraceParent
			var hadTP bool
			if h := r.Header.Get("traceparent"); h != "" {
				tp, hadTP = parseTraceParent(h)
			}
			// 입력 X-Request-ID 그대로 보존 (자유 형식 — "rid-abc-123" 같은 사용자
			// 친화 ID 호환). 16 hex char (8B) 이면 trace_id 앞 8B 로도 사용.
			rid := r.Header.Get("X-Request-ID")
			if !hadTP {
				if seed, err := hex.DecodeString(rid); err == nil && len(seed) == 8 {
					copy(tp.TraceID[:8], seed)
					_, _ = rand.Read(tp.TraceID[8:])
				} else {
					_, _ = rand.Read(tp.TraceID[:])
				}
				_, _ = rand.Read(tp.ParentID[:])
			}
			if rid == "" {
				rid = hex.EncodeToString(tp.TraceID[:8])
			}

			w.Header().Set("X-Request-ID", rid)
			w.Header().Set("traceparent", formatTraceParent(tp))

			ctx := context.WithValue(r.Context(), requestIDKey, rid)
			ctx = context.WithValue(ctx, traceParentKey, tp)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
