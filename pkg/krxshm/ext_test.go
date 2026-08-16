package krxshm

import (
	"encoding/binary"
	"math"
	"testing"
)

func f64at(b []byte, o int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b[o:])) }
func u64at(b []byte, o int) uint64  { return binary.LittleEndian.Uint64(b[o:]) }

// TestFhoga — UpdateBook 가 FHOGA_T(파생 호가)에 정확히 기록되는지.
func TestFhoga(t *testing.T) {
	buf := make([]byte, ShmSize)
	w, _ := NewWriter(buf)
	if err := w.Layout(map[string]string{"101V6000": "1V6000"}); err != nil {
		t.Fatal(err)
	}
	err := w.UpdateBook(Book{
		Code: "101V6000", AskTot: 30, BidTot: 80, AskCnt: 5, BidCnt: 6, ExpPrc: 265.75, ExpVol: 50,
		Ask: []Level{{265.80, 10, 1}, {265.85, 8, 1}},
		Bid: []Level{{265.75, 20, 2}, {265.70, 18, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := entryOff(0) + offFhoga
	if u64at(buf, h+fhStVol) != 30 || u64at(buf, h+fhBtVol) != 80 {
		t.Errorf("총잔량 ask=%d bid=%d", u64at(buf, h+fhStVol), u64at(buf, h+fhBtVol))
	}
	if f64at(buf, h+fhExPrc) != 265.75 || u64at(buf, h+fhExVol) != 50 {
		t.Errorf("예상체결 %v/%d", f64at(buf, h+fhExPrc), u64at(buf, h+fhExVol))
	}
	// shoga[0]
	if f64at(buf, h+fhShoga) != 265.80 || u64at(buf, h+fhShoga+8) != 10 || u64at(buf, h+fhShoga+16) != 1 {
		t.Errorf("shoga[0] %v/%d/%d", f64at(buf, h+fhShoga), u64at(buf, h+fhShoga+8), u64at(buf, h+fhShoga+16))
	}
	// bhoga[1]
	o := h + fhBhoga + fhUStr
	if f64at(buf, o) != 265.70 || u64at(buf, o+8) != 18 {
		t.Errorf("bhoga[1] %v/%d", f64at(buf, o), u64at(buf, o+8))
	}
}

// TestBondSHM — 채권 Layout+UpdateTrade+UpdateBook 가 BSISE_T/BHOGA_T 에 기록되는지.
func TestBondSHM(t *testing.T) {
	buf := make([]byte, BondShmSize)
	w, err := NewBondWriter(buf)
	if err != nil {
		t.Fatal(err)
	}
	code := "KR1035020310"
	if err := w.Layout(map[string]string{code: "국고채03500"}); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(buf[bOffUseN:]) != 1 {
		t.Errorf("useN=%d want 1", binary.LittleEndian.Uint32(buf[bOffUseN:]))
	}
	if c := string(buf[bondEntryOff(0) : bondEntryOff(0)+12]); c != code {
		t.Errorf("bondCd=%q", c)
	}
	err = w.UpdateTrade(BondQuote{Code: code, Last: 10250.50, BasePrc: 10240.00, Diff: 10.50,
		Yield: 3.125, OYield: 3.14, PrevClose: 10240.00, Rate: 0.1025, AccVol: 500, Sign: "+"})
	if err != nil {
		t.Fatal(err)
	}
	s := bondEntryOff(0) + bOffBsise
	if f64at(buf, s+bsEPrc) != 10250.50 {
		t.Errorf("ePrc=%v want 10250.50", f64at(buf, s+bsEPrc))
	}
	if f64at(buf, s+bsBPrc) != 10240.00 {
		t.Errorf("bPrc=%v", f64at(buf, s+bsBPrc))
	}
	if f64at(buf, s+bsEYld) != 3.125 {
		t.Errorf("eYld=%v want 3.125", f64at(buf, s+bsEYld))
	}
	if d := f64at(buf, s+bsDiff); math.Abs(d-10.50) > 1e-9 {
		t.Errorf("diff=%v want 10.50", d)
	}
	if buf[s+bsSign] != '+' {
		t.Errorf("sign=%q", buf[s+bsSign])
	}
	// 호가
	if err := w.UpdateBook(BondBook{Code: code, AskTot: 180, BidTot: 350,
		Ask: []BondLevel{{10251, 100, 3.10}}, Bid: []BondLevel{{10250, 200, 3.20}}}); err != nil {
		t.Fatal(err)
	}
	h := bondEntryOff(0) + bOffBhoga
	if u64at(buf, h+bhStVol) != 180 {
		t.Errorf("bond askTot=%d want 180", u64at(buf, h+bhStVol))
	}
	if f64at(buf, h+bhShoga) != 10251 || f64at(buf, h+bhShoga+16) != 3.10 {
		t.Errorf("bond shoga[0] prc=%v yld=%v", f64at(buf, h+bhShoga), f64at(buf, h+bhShoga+16))
	}
}
