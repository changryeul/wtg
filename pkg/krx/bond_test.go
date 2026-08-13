package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func putf(b []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		b[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(b[off:off+n], s)
}

func buildBA() []byte {
	b := make([]byte, SZBACheg)
	for i := range b {
		b[i] = ' '
	}
	putf(b, 0, 2, "BA")
	putf(b, 2, 12, "KR103501GC90")
	putf(b, 14, 12, "090005123456")
	putf(b, 26, 1, "-")
	putf(b, 27, 11, fmt.Sprintf("%-11.02f", -0.15)) // ydif
	putf(b, 38, 6, fmt.Sprintf("%-6.02f", -0.14))   // yrat
	putf(b, 44, 1, "+")
	putf(b, 45, 11, fmt.Sprintf("%-11.02f", 10250.50))       // cprc last
	putf(b, 56, 13, fmt.Sprintf("%-13.06f", 3.125000))       // cyld yield
	putf(b, 69, 11, fmt.Sprintf("%-11.02f", 0.10))           // diff
	putf(b, 80, 6, fmt.Sprintf("%-6.02f", 0.01))             // rate
	putf(b, 86, 10, fmt.Sprintf("%-10d", 5000))              // cvol
	putf(b, 96, 22, fmt.Sprintf("%-22.3f", 51252500.000))    // camt
	putf(b, 119, 11, fmt.Sprintf("%-11.02f", 10250.00))      // oprc
	putf(b, 131, 11, fmt.Sprintf("%-11.02f", 10260.00))      // hprc
	putf(b, 143, 11, fmt.Sprintf("%-11.02f", 10245.00))      // lprc
	putf(b, 154, 13, fmt.Sprintf("%-13.06f", 3.130000))      // oyld
	putf(b, 167, 13, fmt.Sprintf("%-13.06f", 3.120000))      // hyld
	putf(b, 180, 13, fmt.Sprintf("%-13.06f", 3.135000))      // lyld
	putf(b, 193, 15, fmt.Sprintf("%-15d", 123456))           // tvol
	putf(b, 208, 22, fmt.Sprintf("%-22.3f", 1265432100.000)) // tamt
	return b
}

func TestDecodeBACheg(t *testing.T) {
	bt, err := DecodeBACheg(buildBA())
	if err != nil {
		t.Fatalf("DecodeBACheg: %v", err)
	}
	if bt.Kind != "bond.trade" || bt.Code != "KR103501GC90" {
		t.Errorf("헤더 불일치: %+v", bt)
	}
	if bt.Last != 10250.50 || bt.Yield != 3.125 {
		t.Errorf("체결가/수익률: last=%v yield=%v", bt.Last, bt.Yield)
	}
	if bt.YDiff != -0.15 || bt.YSign != "-" {
		t.Errorf("전일대비: ydiff=%v ysign=%q", bt.YDiff, bt.YSign)
	}
	if bt.Open != 10250.00 || bt.High != 10260.00 || bt.Low != 10245.00 {
		t.Errorf("OHL: %v/%v/%v", bt.Open, bt.High, bt.Low)
	}
	if bt.Cvol != 5000 || bt.Tvol != 123456 {
		t.Errorf("cvol/tvol: %v/%v", bt.Cvol, bt.Tvol)
	}
	js, _ := json.Marshal(bt)
	t.Logf("JSON = %s", js)
}

func buildBB() []byte {
	b := make([]byte, SZBBHoga)
	for i := range b {
		b[i] = ' '
	}
	putf(b, 0, 2, "BB")
	putf(b, 2, 12, "KR103501GC90")
	putf(b, 14, 8, "20260813")
	putf(b, 22, 12, "090005123456")
	putf(b, 50, 15, fmt.Sprintf("%-15d", 8000)) // stvl askTot
	putf(b, 81, 15, fmt.Sprintf("%-15d", 7500)) // btvl bidTot
	for i := 0; i < nHogaLevel; i++ {
		so := 140 + i*bondEntrySz
		putf(b, so+1, 11, fmt.Sprintf("%-11.02f", 10251.00+float64(i)))
		putf(b, so+12, 15, fmt.Sprintf("%-15d", 100+i*10))
		putf(b, so+27, 13, fmt.Sprintf("%-13.06f", 3.120000+float64(i)*0.001))
	}
	buyOff := 140 + nHogaLevel*bondEntrySz
	for i := 0; i < nHogaLevel; i++ {
		bo := buyOff + i*bondEntrySz
		putf(b, bo+1, 11, fmt.Sprintf("%-11.02f", 10250.00-float64(i)))
		putf(b, bo+12, 15, fmt.Sprintf("%-15d", 90+i*10))
		putf(b, bo+27, 13, fmt.Sprintf("%-13.06f", 3.125000+float64(i)*0.001))
	}
	return b
}

func TestDecodeBBHoga(t *testing.T) {
	bb, err := DecodeBBHoga(buildBB())
	if err != nil {
		t.Fatalf("DecodeBBHoga: %v", err)
	}
	if bb.Kind != "bond.book" || bb.Code != "KR103501GC90" || bb.Date != "20260813" {
		t.Errorf("헤더: %+v", bb)
	}
	if bb.AskTot != 8000 || bb.BidTot != 7500 {
		t.Errorf("총잔량: %v/%v", bb.AskTot, bb.BidTot)
	}
	if len(bb.Ask) != 5 || len(bb.Bid) != 5 {
		t.Fatalf("호가단계: %d/%d", len(bb.Ask), len(bb.Bid))
	}
	if bb.Ask[0] != (BondLevel{Prc: 10251.00, Vol: 100, Yld: 3.120}) {
		t.Errorf("ask[0]=%+v", bb.Ask[0])
	}
	if bb.Bid[4] != (BondLevel{Prc: 10246.00, Vol: 130, Yld: 3.129}) {
		t.Errorf("bid[4]=%+v (5단 오프셋)", bb.Bid[4])
	}
	js, _ := json.Marshal(bb)
	t.Logf("JSON = %s", js)
}

func TestBond_Guards(t *testing.T) {
	if _, err := DecodeBACheg(make([]byte, 50)); err == nil {
		t.Error("BA 길이 미달 무에러")
	}
	if _, err := DecodeBBHoga(make([]byte, 50)); err == nil {
		t.Error("BB 길이 미달 무에러")
	}
}
