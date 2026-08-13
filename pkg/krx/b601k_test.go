package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildB601K() []byte {
	b := make([]byte, SZB601K)
	for i := range b {
		b[i] = ' '
	}
	putFrame(b, 0, 5, "B601K")
	putFrame(b, 17, 12, "KR1035020310") // code
	putFrame(b, 29, 12, "090005123456") // time
	// hoga[5] @41, entry 78B — 단계별 가격/잔량/수익률을 조금씩 흔들어 배정.
	for i := 0; i < 5; i++ {
		o := 41 + i*78
		putFrame(b, o+0, 11, fmt.Sprintf("%11.2f", 10251.0+float64(i)))    // sprc
		putFrame(b, o+11, 11, fmt.Sprintf("%11.2f", 10250.0-float64(i)))   // bprc
		putFrame(b, o+22, 15, fmt.Sprintf("%15d", 100+i))                  // svol
		putFrame(b, o+37, 15, fmt.Sprintf("%15d", 200+i))                  // bvol
		putFrame(b, o+52, 13, fmt.Sprintf("%13.6f", 3.10+0.01*float64(i))) // syld
		putFrame(b, o+65, 13, fmt.Sprintf("%13.6f", 3.20+0.01*float64(i))) // byld
	}
	putFrame(b, 431, 15, fmt.Sprintf("%15d", 5000)) // stvl
	putFrame(b, 446, 15, fmt.Sprintf("%15d", 6000)) // btvl
	return b
}

func TestDecodeB601K(t *testing.T) {
	bb, err := DecodeB601K(buildB601K())
	if err != nil {
		t.Fatalf("DecodeB601K: %v", err)
	}
	if bb.Kind != "bond.book" || bb.Code != "KR1035020310" || bb.Time != "090005123456" {
		t.Errorf("헤더: %+v", bb)
	}
	if bb.AskTot != 5000 || bb.BidTot != 6000 {
		t.Errorf("총잔량: ask=%v bid=%v", bb.AskTot, bb.BidTot)
	}
	if len(bb.Ask) != 5 || len(bb.Bid) != 5 {
		t.Fatalf("호가단수: ask=%d bid=%d", len(bb.Ask), len(bb.Bid))
	}
	if bb.Ask[0].Prc != 10251.00 || bb.Ask[0].Vol != 100 || bb.Ask[0].Yld != 3.10 {
		t.Errorf("ask[0]: %+v", bb.Ask[0])
	}
	if bb.Bid[0].Prc != 10250.00 || bb.Bid[0].Vol != 200 || bb.Bid[0].Yld != 3.20 {
		t.Errorf("bid[0]: %+v", bb.Bid[0])
	}
	if bb.Ask[4].Prc != 10255.00 || bb.Bid[4].Prc != 10246.00 {
		t.Errorf("ask[4]/bid[4] 가격: %v/%v", bb.Ask[4].Prc, bb.Bid[4].Prc)
	}
	js, _ := json.Marshal(bb)
	t.Logf("JSON = %s", js)
}

func TestDecodeB601K_Guards(t *testing.T) {
	if _, err := DecodeB601K(make([]byte, 200)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildB601K()
	copy(b[0:5], "A301K")
	if _, err := DecodeB601K(b); err == nil {
		t.Error("B601K 아닌데 무에러")
	}
}
