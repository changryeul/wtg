package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildA001B() []byte {
	b := make([]byte, SZBondMaster)
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
	put(0, 5, "A001B")                                      // TR코드
	put(27, 12, "KR103501GC90")                             // code
	put(45, 2, "GA")                                        // typc 국채
	put(47, 40, "국고03500-2609(26-9)")                       // ksnm
	put(130, 1, "Y")                                        // bltc 상장
	put(137, 1, "5")                                        // gtcd 정부보증
	put(138, 2, "13")                                       // cpcd 고정-이표채
	put(140, 8, "20230910")                                 // ltdt 상장일
	put(148, 8, "20230910")                                 // isdt 발행일
	put(156, 8, "20260910")                                 // rddt 상환일(만기)
	put(172, 13, fmt.Sprintf("%-13.06f", 0.125000))         // isrt 발행율
	put(185, 14, fmt.Sprintf("%-14.06f", 3.500000))         // cprt 표면이자율
	put(208, 22, fmt.Sprintf("%-22.02f", 1000000000000.00)) // isam 발행금액
	put(230, 22, fmt.Sprintf("%-22.02f", 950000000000.00))  // ltam 상장금액
	put(275, 1, "0")                                        // halt 정상
	put(276, 8, "20260310")                                 // pcpd 전기이자
	put(284, 8, "20260910")                                 // ncpd 차기이자
	put(294, 11, fmt.Sprintf("%-11.02f", 10250.00))         // bprc 기준가
	return b
}

func TestDecodeA001B(t *testing.T) {
	m, err := DecodeA001B(buildA001B())
	if err != nil {
		t.Fatalf("DecodeA001B: %v", err)
	}
	if m.Kind != "bond.master" || m.Code != "KR103501GC90" {
		t.Errorf("헤더: %+v", m)
	}
	if m.BondType != "GA" || m.ListStatus != "Y" || m.Guarantee != "5" {
		t.Errorf("분류/상장/보증: %q/%q/%q", m.BondType, m.ListStatus, m.Guarantee)
	}
	if m.RedeemDate != "20260910" || m.CouponRate != 3.5 || m.IssueRate != 0.125 {
		t.Errorf("만기/표면이자/발행율: %q/%v/%v", m.RedeemDate, m.CouponRate, m.IssueRate)
	}
	if m.IssueAmt != 1000000000000.00 || m.ListAmt != 950000000000.00 {
		t.Errorf("발행/상장금액: %v/%v", m.IssueAmt, m.ListAmt)
	}
	if m.BasePrc != 10250.00 || m.NextCouponDate != "20260910" {
		t.Errorf("기준가/차기이자: %v/%q", m.BasePrc, m.NextCouponDate)
	}
	if m.Halt {
		t.Error("halt=0 인데 true")
	}
	js, _ := json.Marshal(m)
	t.Logf("JSON = %s", js)
}

func TestDecodeA001B_Guards(t *testing.T) {
	if _, err := DecodeA001B(make([]byte, 100)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildA001B()
	copy(b[0:5], "A301K")
	if _, err := DecodeA001B(b); err == nil {
		t.Error("A001B 아닌데 무에러")
	}
}
