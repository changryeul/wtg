package krx

import (
	"encoding/json"
	"fmt"
	"testing"
)

func buildB606F() []byte {
	b := make([]byte, SZB606F)
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
	put(0, 5, "B606F")
	put(17, 12, "101V6000")
	put(35, 12, "090005123456")
	// hoga[5]: 매도 오름차순(265.80..), 매수 내림차순(265.75..)
	for i := 0; i < nHogaLevel; i++ {
		o := 47 + i*b606fHogaSz
		put(o+0, 9, fmt.Sprintf("%9.2f", 265.80+float64(i)*0.05)) // sprc
		put(o+9, 9, fmt.Sprintf("%9.2f", 265.75-float64(i)*0.05)) // bprc
		put(o+18, 9, fmt.Sprintf("%9d", 300+i*10))                // svol
		put(o+27, 9, fmt.Sprintf("%9d", 280+i*10))                // bvol
		put(o+36, 5, fmt.Sprintf("%5d", 12+i))                    // scnt
		put(o+41, 5, fmt.Sprintf("%5d", 9+i))                     // bcnt
	}
	put(277, 9, fmt.Sprintf("%9d", 1520))     // stvl
	put(286, 9, fmt.Sprintf("%9d", 1340))     // btvl
	put(295, 5, fmt.Sprintf("%5d", 48))       // apvc
	put(300, 5, fmt.Sprintf("%5d", 41))       // bpvc
	put(305, 9, fmt.Sprintf("%9.2f", 265.75)) // etpr
	put(314, 9, fmt.Sprintf("%9d", 40))       // etvl
	return b
}

func TestDecodeB606F(t *testing.T) {
	fb, err := DecodeB606F(buildB606F())
	if err != nil {
		t.Fatalf("DecodeB606F: %v", err)
	}
	if fb.Kind != "fut.book" || fb.Code != "101V6000" {
		t.Errorf("헤더: %+v", fb)
	}
	if fb.AskTot != 1520 || fb.BidTot != 1340 || fb.AskCnt != 48 || fb.BidCnt != 41 {
		t.Errorf("총잔량/건수: %+v", fb)
	}
	if fb.ExpPrc != 265.75 || fb.ExpVol != 40 {
		t.Errorf("예상체결: %v/%v", fb.ExpPrc, fb.ExpVol)
	}
	if fb.Ask[0] != (BookLevel{Prc: 265.80, Vol: 300, Cnt: 12}) {
		t.Errorf("ask[0]=%+v", fb.Ask[0])
	}
	if fb.Bid[0] != (BookLevel{Prc: 265.75, Vol: 280, Cnt: 9}) {
		t.Errorf("bid[0]=%+v", fb.Bid[0])
	}
	if fb.Ask[4] != (BookLevel{Prc: 266.00, Vol: 340, Cnt: 16}) {
		t.Errorf("ask[4]=%+v (interleave 오프셋)", fb.Ask[4])
	}
	if fb.Bid[4] != (BookLevel{Prc: 265.55, Vol: 320, Cnt: 13}) {
		t.Errorf("bid[4]=%+v", fb.Bid[4])
	}
	js, _ := json.Marshal(fb)
	t.Logf("JSON = %s", js)
}

func TestDecodeB606F_Guards(t *testing.T) {
	if _, err := DecodeB606F(make([]byte, 50)); err == nil {
		t.Error("길이 미달 무에러")
	}
	b := buildB606F()
	copy(b[0:5], "A306F")
	if _, err := DecodeB606F(b); err == nil {
		t.Error("B606F 아닌데 무에러")
	}
}
