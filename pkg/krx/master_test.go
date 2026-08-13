package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildA006F() []byte {
	b := make([]byte, SZMaster)
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
	put(0, 5, "A006F")                              // datc+infc TR코드
	put(27, 12, "201S3000")                         // code (콜옵션 예)
	put(45, 1, "C")                                 // focd 콜옵션
	put(57, 9, "201S3000")                          // iscd 단축
	put(66, 80, "KOSPI200 콜 425.0 2026-09")         // klnm
	put(146, 40, "C 202609 425.0")                  // ksnm 약명
	put(309, 8, "20250901")                         // ltdt 상장일
	put(331, 11, fmt.Sprintf("%-11.02f", 15.20))    // upl1 상한
	put(364, 11, fmt.Sprintf("%-11.02f", 2.10))     // lpl1 하한
	put(397, 11, fmt.Sprintf("%-11.02f", 8.55))     // bprc 기준가
	put(408, 3, "101")                              // uaid 기초자산ID
	put(411, 1, "E")                                // recd 유럽형
	put(438, 8, "20260910")                         // tddt 최종거래일
	put(457, 8, "20260910")                         // exdt 만기
	put(465, 18, fmt.Sprintf("%-18.02f", 425.00))   // eprc 행사가
	put(484, 22, fmt.Sprintf("%-22.02f", 1.00))     // unit 거래단위
	put(506, 22, fmt.Sprintf("%-22.02f", 250000.0)) // mult 거래승수
	put(543, 12, "K2I00000000")                     // uacd 기초자산코드
	put(689, 1, "0")                                // halt 정상
	put(730, 1, "1")                                // atmc ATM
	put(748, 11, fmt.Sprintf("%-11.02f", 8.60))     // yprc 전일종가
	put(825, 12, fmt.Sprintf("%-12d", 45231))       // pdoi 전일미결제
	put(859, 11, fmt.Sprintf("%-11.04f", 0.1875))   // ipvl 내재변동성 (소수 4자리)
	return b
}

func TestDecodeA006F(t *testing.T) {
	m, err := DecodeA006F(buildA006F())
	if err != nil {
		t.Fatalf("DecodeA006F: %v", err)
	}
	if m.Kind != "fut.master" || m.Code != "201S3000" {
		t.Errorf("헤더: %+v", m)
	}
	if m.OptType != "C" || m.Strike != 425.00 || m.ExerciseType != "E" || m.AtmType != "1" {
		t.Errorf("옵션 필드: opt=%q strike=%v ex=%q atm=%q", m.OptType, m.Strike, m.ExerciseType, m.AtmType)
	}
	if m.Expiry != "20260910" || m.BasePrc != 8.55 || m.PrevClose != 8.60 {
		t.Errorf("만기/가격: exp=%q base=%v prev=%v", m.Expiry, m.BasePrc, m.PrevClose)
	}
	if m.UpLimit != 15.20 || m.DnLimit != 2.10 {
		t.Errorf("상하한: %v/%v", m.UpLimit, m.DnLimit)
	}
	if m.Mult != 250000 || m.PrevOI != 45231 || m.IV != 0.1875 {
		t.Errorf("승수/미결제/IV: %v/%v/%v", m.Mult, m.PrevOI, m.IV)
	}
	if m.Halt {
		t.Error("halt=0 인데 true")
	}
	if m.UnderlyingCd != "K2I00000000" {
		t.Errorf("기초자산코드=%q", m.UnderlyingCd)
	}
	js, _ := json.Marshal(m)
	t.Logf("JSON = %s", js)
}

func TestDecodeA006F_Guards(t *testing.T) {
	if _, err := DecodeA006F(make([]byte, 500)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildA006F()
	copy(b[0:5], "A306F")
	if _, err := DecodeA006F(b); err == nil {
		t.Error("A006F 아닌데 무에러")
	}
}
