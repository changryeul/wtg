package middleware

import (
	"context"
	"encoding/hex"
)

// W3C tracecontext (https://www.w3.org/TR/trace-context/) 의 traceparent 헤더
// 처리. RequestID 미들웨어와 통합되어 동일 미들웨어가 둘 다 채움.
//
// Wire format:
//
//	traceparent: 00-<trace-id 32 hex>-<parent-id 16 hex>-<flags 2 hex>
//
// 예: traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01

const traceParentKey ctxKey = 2

// TraceParent — W3C tracecontext 의 단일 traceparent.
type TraceParent struct {
	Version  byte
	TraceID  [16]byte
	ParentID [8]byte
	Flags    byte
}

// TraceParentFromContext — context 에서 추출. RequestID 미들웨어가 채움.
func TraceParentFromContext(ctx context.Context) (TraceParent, bool) {
	if v, ok := ctx.Value(traceParentKey).(TraceParent); ok {
		return v, true
	}
	return TraceParent{}, false
}

// TraceIDHexFromContext — trace_id 의 32 hex char. 미설정 시 빈 문자열.
// BuildFrame 등에서 mymq.TraceIDFromHex 로 mqhdr.trcid 채우기 위해.
func TraceIDHexFromContext(ctx context.Context) string {
	tp, ok := TraceParentFromContext(ctx)
	if !ok {
		return ""
	}
	var zero [16]byte
	if tp.TraceID == zero {
		return ""
	}
	return hex.EncodeToString(tp.TraceID[:])
}

// parseTraceParent — wire format 파싱. 잘못된 형식이면 ok=false.
//
// 허용:
//   - version 00 만 (W3C 명시: future version 은 모르면 ignore)
//   - flags 임의 (00 = sampled X, 01 = sampled)
func parseTraceParent(s string) (TraceParent, bool) {
	// 길이 = 2+1+32+1+16+1+2 = 55
	if len(s) != 55 {
		return TraceParent{}, false
	}
	if s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return TraceParent{}, false
	}
	var tp TraceParent
	ver, err := hex.DecodeString(s[0:2])
	if err != nil {
		return TraceParent{}, false
	}
	tp.Version = ver[0]
	if tp.Version != 0x00 {
		return TraceParent{}, false
	}
	tid, err := hex.DecodeString(s[3:35])
	if err != nil {
		return TraceParent{}, false
	}
	copy(tp.TraceID[:], tid)
	// trace_id all-zero invalid (W3C §3.2.2.5).
	var zeroTID [16]byte
	if tp.TraceID == zeroTID {
		return TraceParent{}, false
	}
	pid, err := hex.DecodeString(s[36:52])
	if err != nil {
		return TraceParent{}, false
	}
	copy(tp.ParentID[:], pid)
	var zeroPID [8]byte
	if tp.ParentID == zeroPID {
		return TraceParent{}, false
	}
	flags, err := hex.DecodeString(s[53:55])
	if err != nil {
		return TraceParent{}, false
	}
	tp.Flags = flags[0]
	return tp, true
}

// FormatTraceParent — wire format 으로 직렬화 (외부 export — edge proxy 등이
// upstream 으로 forward 시 사용).
func FormatTraceParent(tp TraceParent) string {
	return formatTraceParent(tp)
}

// formatTraceParent — wire format 으로 직렬화.
func formatTraceParent(tp TraceParent) string {
	buf := make([]byte, 55)
	hex.Encode(buf[0:2], []byte{tp.Version})
	buf[2] = '-'
	hex.Encode(buf[3:35], tp.TraceID[:])
	buf[35] = '-'
	hex.Encode(buf[36:52], tp.ParentID[:])
	buf[52] = '-'
	hex.Encode(buf[53:55], []byte{tp.Flags})
	return string(buf)
}
