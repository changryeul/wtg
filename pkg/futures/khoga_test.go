package futures

import (
	"encoding/json"
	"fmt"
	"testing"
)

// buildKB 는 결정적 KB 호가 전문(362B)을 합성 — C 피드 %-9.02f/%-12ld/%-5ld 포맷.
func buildKB() []byte {
	b := make([]byte, SZKHoga)
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
	put(0, 2, "KB")
	put(2, 12, "101V6000")
	put(14, 12, "090005123456")
	put(39, 12, fmt.Sprintf("%-12d", 1520))     // stvl askTot
	put(64, 12, fmt.Sprintf("%-12d", 1340))     // btvl bidTot
	put(76, 12, fmt.Sprintf("%-12d", 48))       // savl askCnt
	put(88, 12, fmt.Sprintf("%-12d", 41))       // bavl bidCnt
	put(101, 9, fmt.Sprintf("%-9.02f", 265.75)) // xprc
	put(110, 12, fmt.Sprintf("%-12d", 40))      // xvol
	// sell[5] @122 — 오름차순 매도호가 (265.80, 265.85, ...)
	for i := 0; i < nHogaLevel; i++ {
		so := 122 + i*hogaEntrySz
		put(so+1, 9, fmt.Sprintf("%-9.02f", 265.80+float64(i)*0.05))
		put(so+10, 9, fmt.Sprintf("%-9d", 300+i*10))
		put(so+19, 5, fmt.Sprintf("%-5d", 12+i))
	}
	// buy[5] @242 — 내림차순 매수호가 (265.75, 265.70, ...)
	buyOff := 122 + nHogaLevel*hogaEntrySz
	for i := 0; i < nHogaLevel; i++ {
		bo := buyOff + i*hogaEntrySz
		put(bo+1, 9, fmt.Sprintf("%-9.02f", 265.75-float64(i)*0.05))
		put(bo+10, 9, fmt.Sprintf("%-9d", 280+i*10))
		put(bo+19, 5, fmt.Sprintf("%-5d", 9+i))
	}
	return b
}

func TestDecodeKHoga(t *testing.T) {
	fb, err := DecodeKHoga(buildKB())
	if err != nil {
		t.Fatalf("DecodeKHoga: %v", err)
	}
	if fb.Kind != "fut.book" || fb.Code != "101V6000" || fb.Time != "090005123456" {
		t.Errorf("헤더 불일치: %+v", fb)
	}
	if fb.AskTot != 1520 || fb.BidTot != 1340 || fb.AskCnt != 48 || fb.BidCnt != 41 {
		t.Errorf("총잔량/건수 불일치: %+v", fb)
	}
	if fb.ExpPrc != 265.75 || fb.ExpVol != 40 {
		t.Errorf("예상체결 불일치: exp=%v/%v", fb.ExpPrc, fb.ExpVol)
	}
	if len(fb.Ask) != 5 || len(fb.Bid) != 5 {
		t.Fatalf("호가단계 수 %d/%d, want 5/5", len(fb.Ask), len(fb.Bid))
	}
	// 1단 검증
	if fb.Ask[0] != (BookLevel{Prc: 265.80, Vol: 300, Cnt: 12}) {
		t.Errorf("ask[0]=%+v, want {265.80,300,12}", fb.Ask[0])
	}
	if fb.Bid[0] != (BookLevel{Prc: 265.75, Vol: 280, Cnt: 9}) {
		t.Errorf("bid[0]=%+v, want {265.75,280,9}", fb.Bid[0])
	}
	// 5단 (마지막) 검증 — 오프셋 정합
	if fb.Ask[4] != (BookLevel{Prc: 266.00, Vol: 340, Cnt: 16}) {
		t.Errorf("ask[4]=%+v, want {266.00,340,16}", fb.Ask[4])
	}
	if fb.Bid[4] != (BookLevel{Prc: 265.55, Vol: 320, Cnt: 13}) {
		t.Errorf("bid[4]=%+v, want {265.55,320,13}", fb.Bid[4])
	}

	js, _ := json.Marshal(fb)
	t.Logf("JSON = %s", js)
}

func TestDecodeKHoga_Guards(t *testing.T) {
	if _, err := DecodeKHoga(make([]byte, 100)); err == nil {
		t.Error("길이 미달인데 에러 없음")
	}
	b := buildKB()
	copy(b[0:2], "KA")
	if _, err := DecodeKHoga(b); err == nil {
		t.Error("KB 아닌데 에러 없음")
	}
}
