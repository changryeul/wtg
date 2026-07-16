package quote

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecodeJSONEnvelope_Minimal(t *testing.T) {
	body := []byte(`{"sym":"USDKRW","bid":1399.50,"ask":1399.60,"ts":"2026-05-23T03:21:45.123Z"}`)
	env, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Sym != "USDKRW" {
		t.Errorf("Sym = %q", env.Sym)
	}
	if env.Bid != 1399.50 || env.Ask != 1399.60 {
		t.Errorf("bid/ask = %v / %v", env.Bid, env.Ask)
	}
	wantTS := time.Date(2026, 5, 23, 3, 21, 45, 123_000_000, time.UTC)
	if !env.TS.Equal(wantTS) {
		t.Errorf("TS = %v, want %v", env.TS, wantTS)
	}
}

func TestDecodeJSONEnvelope_AuditFields(t *testing.T) {
	body := []byte(`{
		"sym":"EURKRW","bid":1500.10,"ask":1500.25,
		"ts":"2026-05-23T03:21:45.456Z",
		"src":"COOKER","seq":7891234
	}`)
	env, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Src != "COOKER" || env.Seq != 7891234 {
		t.Errorf("audit 필드 누락: %+v", env)
	}
}

// last (시장 체결가) 필드 — bid/ask 와 함께 오면 보존 (mds fillprc 대응).
func TestJSONEnvelope_LastField(t *testing.T) {
	body := []byte(`{
		"sym":"USDKRW","bid":1385.20,"ask":1385.60,
		"ts":"2026-07-16T03:21:45.456Z",
		"last":1385.40,"last_qty":1000000
	}`)
	env, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Last != 1385.40 {
		t.Errorf("last=%v, want 1385.40", env.Last)
	}
	if env.LastQty != 1000000 {
		t.Errorf("last_qty=%v, want 1000000", env.LastQty)
	}
	// 왕복 — encode 후 재 decode 해도 last 보존.
	out, err := EncodeJSONEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	env2, err := DecodeJSONEnvelope(out)
	if err != nil {
		t.Fatal(err)
	}
	if env2.Last != 1385.40 || env2.LastQty != 1000000 {
		t.Errorf("왕복 last 불일치: %+v", env2)
	}
}

// last 없는 envelope 은 omitempty 로 직렬화에서 생략 (기존 호환).
func TestJSONEnvelope_LastOmittedWhenZero(t *testing.T) {
	env := JSONEnvelope{Sym: "USDKRW", Bid: 1, Ask: 2}
	out, err := EncodeJSONEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContains(out, "last") {
		t.Errorf("last=0 인데 직렬화에 포함됨: %s", out)
	}
}

func bytesContains(b []byte, s string) bool {
	return len(b) >= len(s) && (func() bool {
		for i := 0; i+len(s) <= len(b); i++ {
			if string(b[i:i+len(s)]) == s {
				return true
			}
		}
		return false
	})()
}

func TestDecodeJSONEnvelope_Reject_InvalidBidAsk(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"ask < bid", `{"sym":"USDKRW","bid":1400,"ask":1399,"ts":"2026-05-23T00:00:00Z"}`},
		{"negative bid", `{"sym":"USDKRW","bid":-1,"ask":1,"ts":"2026-05-23T00:00:00Z"}`},
		{"zero ask", `{"sym":"USDKRW","bid":1,"ask":0,"ts":"2026-05-23T00:00:00Z"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeJSONEnvelope([]byte(tc.body))
			if !errors.Is(err, ErrEnvelopeInvalidBidAsk) {
				t.Errorf("err = %v, want ErrEnvelopeInvalidBidAsk", err)
			}
		})
	}
}

func TestDecodeJSONEnvelope_Reject_MissingSym(t *testing.T) {
	body := []byte(`{"bid":1,"ask":2,"ts":"2026-05-23T00:00:00Z"}`)
	_, err := DecodeJSONEnvelope(body)
	if !errors.Is(err, ErrEnvelopeMissingSym) {
		t.Errorf("err = %v, want ErrEnvelopeMissingSym", err)
	}
}

func TestDecodeJSONEnvelope_Empty(t *testing.T) {
	if _, err := DecodeJSONEnvelope(nil); !errors.Is(err, ErrEnvelopeEmpty) {
		t.Errorf("nil body: err = %v", err)
	}
	if _, err := DecodeJSONEnvelope([]byte("")); !errors.Is(err, ErrEnvelopeEmpty) {
		t.Errorf("빈 body: err = %v", err)
	}
	if _, err := DecodeJSONEnvelope([]byte("   \x00\x00")); !errors.Is(err, ErrEnvelopeEmpty) {
		t.Errorf("NUL/공백 only: err = %v", err)
	}
}

func TestDecodeJSONEnvelope_NULPadding(t *testing.T) {
	// pushdata.msgb 는 1512 bytes fixed buffer — NUL padding 흔함.
	core := `{"sym":"USDKRW","bid":1399.50,"ask":1399.60,"ts":"2026-05-23T00:00:00Z"}`
	body := append([]byte(core), make([]byte, 100)...) // NUL padding
	env, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatalf("NUL-padded decode: %v", err)
	}
	if env.Sym != "USDKRW" {
		t.Errorf("Sym = %q", env.Sym)
	}
}

func TestDecodeJSONEnvelope_BadJSON(t *testing.T) {
	_, err := DecodeJSONEnvelope([]byte("not json"))
	if err == nil {
		t.Error("malformed JSON 이 통과")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("err 메시지에 'JSON' 없음: %v", err)
	}
}

func TestEncodeJSONEnvelope_RoundTrip(t *testing.T) {
	want := JSONEnvelope{
		Sym: "USDKRW",
		Bid: 1399.50,
		Ask: 1399.60,
		TS:  time.Date(2026, 5, 23, 3, 21, 45, 123_000_000, time.UTC),
		Src: "COOKER",
		Seq: 999,
	}
	body, err := EncodeJSONEnvelope(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSONEnvelope(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sym != want.Sym || got.Bid != want.Bid || got.Ask != want.Ask ||
		got.Src != want.Src || got.Seq != want.Seq {
		t.Errorf("round-trip mismatch: got=%+v want=%+v", got, want)
	}
	if !got.TS.Equal(want.TS) {
		t.Errorf("TS round-trip: got=%v want=%v", got.TS, want.TS)
	}
}
