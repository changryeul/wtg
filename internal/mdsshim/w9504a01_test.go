package mdsshim

import (
	"strings"
	"testing"
)

// fixedField 는 고정폭 ASCII 필드를 만든다 (좌측 정렬 + 공백 패딩).
func fixedField(v string, w int) string {
	return v + strings.Repeat(" ", w-len(v))
}

// buildW2006A01 은 trn W2006A01 이 보내는 고정폭 전문을 조립한다.
// 헤더 15B (regTp 1 + crncPairId 7 + mrgnTcd 1 + grid01_cnt 6) + 73B×n.
func buildW2006A01(regTp, pair, mrgnTcd string, recs [][9]string) []byte {
	var b strings.Builder
	b.WriteString(fixedField(regTp, 1))
	b.WriteString(fixedField(pair, 7))
	b.WriteString(fixedField(mrgnTcd, 1))
	b.WriteString(fixedField(itoa6(len(recs)), 6))
	widths := [9]int{3, 10, 10, 10, 10, 10, 10, 10, 10}
	for _, r := range recs {
		for i, v := range r {
			b.WriteString(fixedField(v, widths[i]))
		}
	}
	return []byte(b.String())
}

func itoa6(n int) string {
	s := []byte("000000")
	for i := 5; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}

func TestParseW2006A01(t *testing.T) {
	in := buildW2006A01("1", "USD/KRW", "1", [][9]string{
		{"M01", "0.15", "0.25", "0.01", "0.02", "0.03", "0.04", "0.001", "0.002"},
		{"SPT", "-1.5", "2.5", "", "", "", "", "", ""},
	})

	req, err := ParseW2006A01(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.RegTp != RegTpRegister {
		t.Fatalf("RegTp=%v, want 등록(1)", req.RegTp)
	}
	if req.Pair != "USD/KRW" {
		t.Fatalf("Pair=%q", req.Pair)
	}
	if len(req.Records) != 2 {
		t.Fatalf("records=%d, want 2", len(req.Records))
	}
	r0 := req.Records[0]
	if r0.Tenor != "M01" || r0.BidSwapPnt != 0.15 || r0.AskSwapPnt != 0.25 {
		t.Fatalf("rec0 파싱 불일치: %+v", r0)
	}
	r1 := req.Records[1]
	if r1.Tenor != "SPT" || r1.BidSwapPnt != -1.5 || r1.AskSwapPnt != 2.5 {
		t.Fatalf("rec1 파싱 불일치: %+v", r1)
	}
}

func TestParseW2006A01_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"헤더 미달", []byte("1USD/KRW")},
		{"레코드 본문이 중간에 잘림", buildW2006A01("1", "USD/KRW", "1", [][9]string{
			{"M01", "0.15", "0.25", "", "", "", "", "", ""},
		})[:15+10]},
		{"regTp 불량", buildW2006A01("9", "USD/KRW", "1", nil)},
	}
	// 레코드 2개 선언 + 1개만 실림
	short := buildW2006A01("1", "USD/KRW", "1", [][9]string{
		{"M01", "0.15", "0.25", "", "", "", "", "", ""},
	})
	short[10], short[14] = '0', '2' // grid01_cnt = "000002"
	cases = append(cases, struct {
		name string
		in   []byte
	}{"grid 카운트 > 실제 레코드", short})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseW2006A01(tc.in); err == nil {
				t.Fatalf("에러여야 함")
			}
		})
	}
}

func TestSwapUpdates(t *testing.T) {
	in := buildW2006A01("1", "USD/KRW", "1", [][9]string{
		{"M01", "15", "25", "", "", "", "", "", ""},
		{"W01", "1.5", "2.5", "", "", "", "", "", ""},
		{"ZZZ", "9", "9", "", "", "", "", "", ""}, // 미지의 tenor 는 skip
	})
	req, err := ParseW2006A01(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// zdiv=2 → 원시값 / 10^2 (mds W9504A01 의 p10[fold->zdiv] 스케일과 동일)
	ups, skipped := req.SwapUpdates(2)
	if len(ups) != 2 || skipped != 1 {
		t.Fatalf("updates=%d skipped=%d, want 2/1", len(ups), skipped)
	}
	if ups[0].Tenor != "1M" || ups[0].Bid != 0.15 || ups[0].Ask != 0.25 {
		t.Fatalf("M01 매핑 불일치: %+v", ups[0])
	}
	if ups[1].Tenor != "1W" || ups[1].Bid != 0.015 || ups[1].Ask != 0.025 {
		t.Fatalf("W01 매핑 불일치: %+v", ups[1])
	}
	if ups[0].Pair != "USD/KRW" {
		t.Fatalf("Pair 전파 안 됨: %+v", ups[0])
	}
}

func TestSwapUpdates_Unregister(t *testing.T) {
	// regTp=2(해제) 는 mds 와 동일하게 전 tenor 를 NaN(삭제) 처리 — Clear=true
	in := buildW2006A01("2", "USD/KRW", "1", nil)
	req, err := ParseW2006A01(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.RegTp != RegTpUnregister {
		t.Fatalf("RegTp=%v", req.RegTp)
	}
}

func TestBuildW9504A01Reply(t *testing.T) {
	// 응답 = crncPairId[8] 고정폭 (W9504A01_out_t)
	out := BuildW9504A01Reply("USD/KRW")
	if len(out) != 8 {
		t.Fatalf("len=%d, want 8", len(out))
	}
	if string(out) != "USD/KRW " {
		t.Fatalf("out=%q", out)
	}
}

func TestTenorMap(t *testing.T) {
	// mds 9개 tenor 전체가 매핑 대상인지 (W9504A01.c 의 tenor2index 목록)
	for _, mds := range []string{"SPT", "TOD", "TOM", "W01", "M01", "M02", "M03", "M06", "Y01"} {
		if _, ok := mdsTenorToWTG[mds]; !ok {
			t.Fatalf("tenor %s 매핑 누락", mds)
		}
	}
}
