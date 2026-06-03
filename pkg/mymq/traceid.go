package mymq

import "encoding/hex"

// TraceIDFromHex — hex 문자열을 16 byte trace_id 로 변환. 짧으면 zero pad,
// 길면 truncate. 잘못된 hex 는 zero array 반환 (caller 가 사전 검증 권장).
//
// WTG 의 X-Request-ID 는 8 byte (16 hex char) — trcid[0..7] 에 들어가고
// trcid[8..15] 는 0. W3C tracecontext 의 trace-id 32 hex char 도 호환.
func TraceIDFromHex(s string) [TraceIDSize]byte {
	var out [TraceIDSize]byte
	if s == "" {
		return out
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return out
	}
	copy(out[:], b)
	return out
}

// TraceIDToHex — 16 byte trace_id 를 hex 문자열로. trailing 0 만 trim.
// 모두 0 이면 빈 string.
func TraceIDToHex(t [TraceIDSize]byte) string {
	n := TraceIDSize
	for n > 0 && t[n-1] == 0 {
		n--
	}
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(t[:n])
}
