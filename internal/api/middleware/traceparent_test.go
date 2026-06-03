package middleware

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTraceParent_Valid(t *testing.T) {
	tp, ok := parseTraceParent("00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	if !ok {
		t.Fatal("valid header 파싱 실패")
	}
	wantTID, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	var wantTID16 [16]byte
	copy(wantTID16[:], wantTID)
	if tp.TraceID != wantTID16 {
		t.Errorf("TraceID = %x, want %x", tp.TraceID, wantTID16)
	}
	if tp.Flags != 0x01 {
		t.Errorf("Flags = %x, want 0x01", tp.Flags)
	}
}

func TestParseTraceParent_RejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"길이 부족":              "00-0af-b7ad6b7169203331-01",
		"잘못된 구분자":            "00x0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"trace_id all-zero":  "00-00000000000000000000000000000000-b7ad6b7169203331-01",
		"parent_id all-zero": "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01",
		"잘못된 hex":            "00-0az7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"version 미지원":        "ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}
	for name, h := range cases {
		if _, ok := parseTraceParent(h); ok {
			t.Errorf("%s: 거부 기대인데 통과 — %q", name, h)
		}
	}
}

func TestFormatTraceParent_RoundTrip(t *testing.T) {
	in := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	tp, ok := parseTraceParent(in)
	if !ok {
		t.Fatal("parse")
	}
	if got := formatTraceParent(tp); got != in {
		t.Errorf("round trip: %q, want %q", got, in)
	}
}

// 헤더 받으면 context 의 trace_id 가 동일 + 응답 헤더 echo.
func TestRequestID_TraceParentPropagation(t *testing.T) {
	mw := RequestID()
	var ctxTID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxTID = TraceIDHexFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if ctxTID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("ctx trace_id = %q, want 0af7651916cd43dd8448eb211c80319c", ctxTID)
	}
	if got := w.Header().Get("traceparent"); got != "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01" {
		t.Errorf("응답 traceparent = %q", got)
	}
	// X-Request-ID 는 trace_id 의 앞 8B = 0af7651916cd43dd
	if got := w.Header().Get("X-Request-ID"); got != "0af7651916cd43dd" {
		t.Errorf("X-Request-ID = %q, want 0af7651916cd43dd", got)
	}
}

// 헤더 없으면 새로 생성.
func TestRequestID_GeneratesTraceParent(t *testing.T) {
	mw := RequestID()
	var ctxTID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxTID = TraceIDHexFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if len(ctxTID) != 32 {
		t.Errorf("새로 생성된 trace_id len=%d, want 32", len(ctxTID))
	}
	tp := w.Header().Get("traceparent")
	if !strings.HasPrefix(tp, "00-") || !strings.HasSuffix(tp, "-00") {
		t.Errorf("응답 traceparent 형식 이상: %q", tp)
	}
	rid := w.Header().Get("X-Request-ID")
	if len(rid) != 16 {
		t.Errorf("X-Request-ID len=%d, want 16", len(rid))
	}
}

// 잘못된 traceparent 가 들어와도 새로 생성 + 정상 응답.
func TestRequestID_InvalidTraceParentFallback(t *testing.T) {
	mw := RequestID()
	var ctxTID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxTID = TraceIDHexFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "invalid-garbage")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if len(ctxTID) != 32 {
		t.Errorf("fallback trace_id len=%d, want 32", len(ctxTID))
	}
}
