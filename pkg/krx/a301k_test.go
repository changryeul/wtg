package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func putFrame(b []byte, off, n int, s string) {
	for i := 0; i < n; i++ {
		b[off+i] = ' '
	}
	if len(s) > n {
		s = s[:n]
	}
	copy(b[off:off+n], s)
}

func buildA301K() []byte {
	b := make([]byte, SZA301K)
	for i := range b {
		b[i] = ' '
	}
	putFrame(b, 0, 5, "A301K")
	putFrame(b, 17, 12, "KR1035020310")                   // code
	putFrame(b, 29, 12, "090005123456")                   // time
	putFrame(b, 41, 11, fmt.Sprintf("%11.2f", 10250.50))  // cprc
	putFrame(b, 52, 10, fmt.Sprintf("%10d", 500))         // cvol
	putFrame(b, 70, 22, fmt.Sprintf("%22.3f", 5125250.0)) // camt
	putFrame(b, 92, 13, fmt.Sprintf("%13.6f", 3.125))     // tyld 체결수익률
	putFrame(b, 105, 11, fmt.Sprintf("%11.2f", 10240.00)) // oprc
	putFrame(b, 116, 11, fmt.Sprintf("%11.2f", 10260.00)) // hprc
	putFrame(b, 127, 11, fmt.Sprintf("%11.2f", 10235.00)) // lprc
	putFrame(b, 138, 13, fmt.Sprintf("%13.6f", 3.140))    // oyld
	putFrame(b, 151, 13, fmt.Sprintf("%13.6f", 3.110))    // hyld
	putFrame(b, 164, 13, fmt.Sprintf("%13.6f", 3.150))    // lyld
	putFrame(b, 177, 15, fmt.Sprintf("%15d", 123456))     // tvol
	putFrame(b, 192, 22, fmt.Sprintf("%22.3f", 1.2e9))    // tamt
	return b
}

func TestDecodeA301K(t *testing.T) {
	bt, err := DecodeA301K(buildA301K())
	if err != nil {
		t.Fatalf("DecodeA301K: %v", err)
	}
	if bt.Kind != "bond.trade" || bt.Code != "KR1035020310" || bt.Time != "090005123456" {
		t.Errorf("헤더: %+v", bt)
	}
	if bt.Last != 10250.50 || bt.Cvol != 500 || bt.Yield != 3.125 {
		t.Errorf("체결: last=%v cvol=%v yld=%v", bt.Last, bt.Cvol, bt.Yield)
	}
	if bt.Open != 10240.00 || bt.High != 10260.00 || bt.Low != 10235.00 {
		t.Errorf("OHL: %v/%v/%v", bt.Open, bt.High, bt.Low)
	}
	if bt.OYield != 3.140 || bt.HYield != 3.110 || bt.LYield != 3.150 {
		t.Errorf("OHL yld: %v/%v/%v", bt.OYield, bt.HYield, bt.LYield)
	}
	if bt.Tvol != 123456 || bt.Tamt != 1.2e9 {
		t.Errorf("누적: tvol=%v tamt=%v", bt.Tvol, bt.Tamt)
	}
	// 직전대비/전일대비는 join 전이라 0.
	if bt.Diff != 0 || bt.YDiff != 0 {
		t.Errorf("join 전 대비가 0 아님: diff=%v yDiff=%v", bt.Diff, bt.YDiff)
	}
	js, _ := json.Marshal(bt)
	t.Logf("JSON = %s", js)
}

func TestDecodeA301K_Guards(t *testing.T) {
	if _, err := DecodeA301K(make([]byte, 100)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildA301K()
	copy(b[0:5], "B601K")
	if _, err := DecodeA301K(b); err == nil {
		t.Error("A301K 아닌데 무에러")
	}
}
