package mdsshim

import (
	"strings"
	"testing"
)

func w9501s02Input(exnm, symb, pay, exp string) []byte {
	b := make([]byte, w9501s02InLen)
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:16], exnm)
	copy(b[16:32], symb)
	copy(b[32:48], pay)
	copy(b[48:64], exp)
	return b
}

func TestParseW9501S02(t *testing.T) {
	cases := []struct {
		name       string
		in         []byte
		wantErr    bool
		wantPair   string
		wantPayYmd string
	}{
		{"웹 표기 (슬래시)", w9501s02Input("BEST", "USD/KRW", "20260807", ""), false, "USD/KRW", "20260807"},
		{"C 채널 표기 (6자)", w9501s02Input("BEST", "USDKRW", "20260807", ""), false, "USD/KRW", "20260807"},
		{"symb 누락", w9501s02Input("BEST", "", "20260807", ""), true, "", ""},
		{"입력 미달", []byte("short"), true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseW9501S02(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("에러 기대, got %+v", req)
				}
				return
			}
			if err != nil {
				t.Fatalf("파싱 실패: %v", err)
			}
			if req.Pair != tc.wantPair {
				t.Errorf("Pair = %q, want %q", req.Pair, tc.wantPair)
			}
			if req.PayYmd != tc.wantPayYmd {
				t.Errorf("PayYmd = %q, want %q", req.PayYmd, tc.wantPayYmd)
			}
		})
	}
}

func TestBuildW9501S02Reply(t *testing.T) {
	req := &W9501S02Request{Exnm: "BEST", Symb: "USD/KRW", Pair: "USD/KRW", PayYmd: "20260807"}
	out := BuildW9501S02Reply(req, &SpotQuote{Bid: 1379.7528, Ask: 1380.3289, Source: "BEST"})
	if len(out) != w9501s02OutLen {
		t.Fatalf("out 길이 = %d, want %d", len(out), w9501s02OutLen)
	}
	fieldAt := func(idx int) string { return strings.TrimSpace(string(out[idx*16 : idx*16+16])) }
	// mds W9500.h 오프셋 규약: 17=bid, 19=ask, 28=bid_best, 29=ask_best.
	if got := fieldAt(17); got != "1379.75280" {
		t.Errorf("bid = %q", got)
	}
	if got := fieldAt(19); got != "1380.32890" {
		t.Errorf("ask = %q", got)
	}
	if got := fieldAt(28); got != "1379.75280" {
		t.Errorf("bid_best = %q", got)
	}
	if got := fieldAt(0); got != "BEST" {
		t.Errorf("exnm 에코 = %q", got)
	}
	if got := fieldAt(4); got != "20260807" {
		t.Errorf("pay_ymd 에코 = %q", got)
	}
	if out[480] != 'B' || out[481] != 'B' {
		t.Errorf("source = %q %q, want 'B'", out[480], out[481])
	}
}
