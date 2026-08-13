package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildH306F() []byte {
	b := make([]byte, SZH306F)
	for i := range b {
		b[i] = ' '
	}
	put := func(off, n int, s string) {
		for i := 0; i < n; i++ {
			b[off+i] = ' '
		}
		if len(s) > n {
			s = s[:n]
		}
		copy(b[off:off+n], s)
	}
	put(0, 5, "H306F")                         // TR코드
	put(5, 12, "101V6000")                     // code
	put(23, 18, fmt.Sprintf("%18.2f", 265.30)) // sprc 정산가격
	put(41, 2, "11")                           // spcd 당일종가(실세)
	put(43, 8, fmt.Sprintf("%8.2f", 265.40))   // lspr 최종결제가격
	put(51, 1, "1")                            // lspc 기초자산 종가
	return b
}

func TestDecodeH306F(t *testing.T) {
	s, err := DecodeH306F(buildH306F())
	if err != nil {
		t.Fatalf("DecodeH306F: %v", err)
	}
	if s.Kind != "fut.settle" || s.Code != "101V6000" {
		t.Errorf("헤더: %+v", s)
	}
	if s.Settle != 265.30 || s.SettleCd != "11" {
		t.Errorf("정산가: settle=%v cd=%q", s.Settle, s.SettleCd)
	}
	if s.FinalSettle != 265.40 || s.FinalSettleCd != "1" {
		t.Errorf("최종결제: final=%v cd=%q", s.FinalSettle, s.FinalSettleCd)
	}
	js, _ := json.Marshal(s)
	t.Logf("JSON = %s", js)
}

func TestDecodeH306F_Guards(t *testing.T) {
	if _, err := DecodeH306F(make([]byte, 30)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildH306F()
	copy(b[0:5], "A306F")
	if _, err := DecodeH306F(b); err == nil {
		t.Error("H306F 아닌데 무에러")
	}
}
