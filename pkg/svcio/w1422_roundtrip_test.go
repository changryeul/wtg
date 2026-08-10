package svcio

import (
	"testing"

	"golang.org/x/text/encoding/korean"
)

// W1422S01 런타임 라운드트립 — 엔진이 보낼 법한 451B 출력(한글 포함)을 CP949 로
// 합성해 svcio Deserialize 가 BuyTrnAblYn(마지막 바이트) 을 정확히 읽는지 확인.
// 한글 필드(LssLmtOvrNm "한도초과")가 바이트 오프셋을 흔들지 않음을 검증 (item 1).
func TestW1422S01RoundtripLastField(t *testing.T) {
	spec, err := ParseFile("../../../nh/win/src/inc/trn/W1422S01.h")
	if err != nil {
		t.Skipf("nh 미러 헤더 없음 (CI/외부 환경) — skip: %v", err)
	}

	// 451B 출력 버퍼를 공백으로 채우고 필드별로 값 세팅.
	buf := make([]byte, 451)
	for i := range buf {
		buf[i] = ' '
	}
	enc := korean.EUCKR.NewEncoder()
	put := func(off int, size int, s string) {
		b := []byte(s)
		if cp, err := enc.Bytes([]byte(s)); err == nil {
			b = cp // CP949 인코딩 (한글 대응)
		}
		copy(buf[off:off+size], b)
	}
	put(171, 10, "한도초과") // LssLmtOvrNm — CP949 8바이트, 10B 필드
	put(315, 1, "N")     // SelTrnAblYn
	put(449, 1, "Y")     // BuyLmtAmtOvrYn
	put(450, 1, "Y")     // BuyTrnAblYn ← 마지막 바이트

	out, err := Deserialize(spec.Output, buf)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got := out["BuyTrnAblYn"]; got != "Y" {
		t.Errorf("BuyTrnAblYn=%q, want \"Y\" — 마지막 필드 정렬 깨짐", got)
	}
	if got := out["SelTrnAblYn"]; got != "N" {
		t.Errorf("SelTrnAblYn=%q, want \"N\"", got)
	}
	if got := out["LssLmtOvrNm"]; got != "한도초과" {
		t.Errorf("LssLmtOvrNm=%q, want \"한도초과\" (한글 디코드)", got)
	}
	t.Logf("BuyTrnAblYn=%q SelTrnAblYn=%q LssLmtOvrNm=%q",
		out["BuyTrnAblYn"], out["SelTrnAblYn"], out["LssLmtOvrNm"])
}
