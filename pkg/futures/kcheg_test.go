package futures

import (
	"encoding/json"
	"fmt"
	"testing"
)

// putFixed 는 원 피드의 sprintf("%-<n>...") 처럼 좌측정렬 + 우측 공백패딩으로 값을 박는다.
func putFixed(buf []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		buf[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(buf[off:off+n], s)
}

// buildKA 는 결정적 KA 전문(234B)을 합성한다 — C 피드의 %-9.02f 포맷과 동일.
func buildKA() []byte {
	b := make([]byte, SZKCheg)
	for i := range b {
		b[i] = ' '
	}
	putFixed(b, 0, 2, "KA")
	putFixed(b, 2, 12, "101V6000")
	putFixed(b, 14, 9, fmt.Sprintf("%-9.02f", 265.00))          // bprc
	putFixed(b, 24, 9, fmt.Sprintf("%-9.02f", 265.50))          // oprc open
	putFixed(b, 34, 9, fmt.Sprintf("%-9.02f", 265.75))          // hprc high
	putFixed(b, 44, 9, fmt.Sprintf("%-9.02f", 265.00))          // lprc low
	putFixed(b, 54, 9, fmt.Sprintf("%-9.02f", 265.75))          // eprc last
	putFixed(b, 63, 9, fmt.Sprintf("%-9.02f", 265.50))          // yprc prevClose
	putFixed(b, 72, 9, fmt.Sprintf("%-9.02f", 0.25))            // diff
	putFixed(b, 81, 9, fmt.Sprintf("%-9.02f", 265.60))          // sprc settle
	putFixed(b, 99, 6, fmt.Sprintf("%-6.02f", 0.09))            // rate
	putFixed(b, 107, 1, "+")                                    // sign
	putFixed(b, 108, 12, "090005123456")                        // time
	putFixed(b, 120, 1, "2")                                    // bscd
	putFixed(b, 121, 12, fmt.Sprintf("%-12d", 12345))           // tvol
	putFixed(b, 133, 22, fmt.Sprintf("%-22.2f", 3271500000.00)) // tamt
	putFixed(b, 155, 9, fmt.Sprintf("%-9.02f", 265.75))         // cprc
	putFixed(b, 164, 9, fmt.Sprintf("%-9d", 3))                 // cvol
	putFixed(b, 173, 9, fmt.Sprintf("%-9.02f", 265.75))         // nprc near
	putFixed(b, 182, 9, fmt.Sprintf("%-9.02f", 266.10))         // fprc far
	putFixed(b, 191, 9, fmt.Sprintf("%-9.02f", 291.00))         // uprc upLimit
	putFixed(b, 200, 9, fmt.Sprintf("%-9.02f", 240.00))         // dprc dnLimit
	return b
}

func TestDecodeKChe(t *testing.T) {
	got, err := DecodeKChe(buildKA())
	if err != nil {
		t.Fatalf("DecodeKChe: %v", err)
	}
	want := &FutTrade{
		Kind: "fut.trade", Code: "101V6000", Time: "090005123456",
		BasePrc: 265.00, Open: 265.50, High: 265.75, Low: 265.00,
		Last: 265.75, PrevClose: 265.50, Diff: 0.25, Settle: 265.60,
		Rate: 0.09, Sign: "+", Bs: "2", Tvol: 12345, Tamt: 3271500000.00,
		Cprc: 265.75, Cvol: 3, NearPrc: 265.75, FarPrc: 266.10,
		UpLimit: 291.00, DnLimit: 240.00,
	}
	if *got != *want {
		t.Errorf("디코드 불일치:\n got=%+v\nwant=%+v", *got, *want)
	}

	// JSON 직렬화 확인 (web 계약)
	js, _ := json.Marshal(got)
	t.Logf("JSON = %s", js)
}

// 길이 미달 / 타입 불일치 방어.
func TestDecodeKChe_Guards(t *testing.T) {
	if _, err := DecodeKChe(make([]byte, 100)); err == nil {
		t.Error("길이 미달인데 에러 없음")
	}
	b := buildKA()
	putFixed(b, 0, 2, "KB")
	if _, err := DecodeKChe(b); err == nil {
		t.Error("KA 아닌데 에러 없음")
	}
}

// 오프셋 정합 회귀 — 부호 있는 하락(diff<0) 및 공백 필드 처리.
func TestDecodeKChe_NegativeAndBlank(t *testing.T) {
	b := buildKA()
	putFixed(b, 72, 9, fmt.Sprintf("%-9.02f", -1.50)) // diff 하락
	putFixed(b, 107, 1, "-")                          // sign
	putFixed(b, 191, 9, "")                           // uprc 공백 → 0
	got, err := DecodeKChe(b)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Diff != -1.50 {
		t.Errorf("diff=%v, want -1.50", got.Diff)
	}
	if got.Sign != "-" {
		t.Errorf("sign=%q, want -", got.Sign)
	}
	if got.UpLimit != 0 {
		t.Errorf("공백 uprc=%v, want 0", got.UpLimit)
	}
}
