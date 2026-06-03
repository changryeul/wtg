package mymq

import (
	"bytes"
	"testing"
)

func TestTraceIDRoundTrip_ShortHex(t *testing.T) {
	// WTG 의 X-Request-ID 가 8 byte (16 hex char) — trcid[0..7] 채워지고 나머지 0.
	rid := "0123456789abcdef"
	tid := TraceIDFromHex(rid)
	want := [TraceIDSize]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
	if tid != want {
		t.Errorf("got %x, want %x", tid, want)
	}
	if got := TraceIDToHex(tid); got != rid {
		t.Errorf("round trip: %q, want %q", got, rid)
	}
}

func TestTraceIDRoundTrip_FullW3C(t *testing.T) {
	// W3C tracecontext trace-id 는 32 hex char (16 byte).
	rid := "0af7651916cd43dd8448eb211c80319c"
	tid := TraceIDFromHex(rid)
	if got := TraceIDToHex(tid); got != rid {
		t.Errorf("W3C trace: %q, want %q", got, rid)
	}
}

func TestTraceID_EmptyAndZero(t *testing.T) {
	if got := TraceIDFromHex(""); got != [TraceIDSize]byte{} {
		t.Errorf("empty hex: %x", got)
	}
	if got := TraceIDToHex([TraceIDSize]byte{}); got != "" {
		t.Errorf("zero array: %q, want empty", got)
	}
}

func TestTraceID_InvalidHexReturnsZero(t *testing.T) {
	if got := TraceIDFromHex("not-hex"); got != [TraceIDSize]byte{} {
		t.Errorf("invalid hex: %x", got)
	}
}

func TestFrameEncodeIncludesTraceID(t *testing.T) {
	// FrameInput.TraceID → EncodeFrame → DecodeFrame 라운드트립.
	rid := "deadbeef00112233"
	in := &FrameInput{
		Func:    FCTran,
		Subc:    SubTranMsg,
		Dirf:    DirForward,
		Keyc:    KeySend,
		Xchg:    "ECHOSVC",
		Rkey:    "PING",
		Ckey:    42,
		TraceID: TraceIDFromHex(rid),
	}
	frame, err := EncodeFrame(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) < HdrSize {
		t.Fatalf("frame too short: %d", len(frame))
	}
	// wire 의 trace_id 영역 확인.
	wireTraceID := frame[offTraceID : offTraceID+TraceIDSize]
	if !bytes.Equal(wireTraceID, in.TraceID[:]) {
		t.Errorf("wire trace_id mismatch: %x vs %x", wireTraceID, in.TraceID)
	}
	// DecodeFrame 후에도 동일 값.
	dec, err := DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Header.TraceID != in.TraceID {
		t.Errorf("decode trace_id: %x, want %x", dec.Header.TraceID, in.TraceID)
	}
	if got := TraceIDToHex(dec.Header.TraceID); got != rid {
		t.Errorf("decode hex: %q, want %q", got, rid)
	}
}

func TestFrameHdrSizeIs100(t *testing.T) {
	if HdrSize != 100 {
		t.Errorf("HdrSize = %d, want 100 (mqhdr_t 확장 후)", HdrSize)
	}
}
