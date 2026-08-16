package krxshm

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// BondQuote — 채권 체결 SHM 적재값 (BSISE_T). 전일대비는 기준가(bPrc) 대비.
type BondQuote struct {
	Code                          string
	BasePrc, Last, Diff           float64
	Yield, OYield, HYield, LYield float64 // 체결/시/고/저 수익률
	PrevClose, PrevYield, Rate    float64
	AccVol                        uint64
	Sign                          string
}

// BondLevel / BondBook — 채권 호가(BHOGA_T, 수익률 동반).
type BondLevel struct {
	Prc float64
	Vol uint64
	Yld float64
}
type BondBook struct {
	Code           string
	AskTot, BidTot uint64
	Ask, Bid       []BondLevel
}

// BondWriter — mmap 된 MBSISE_T 영역에 채권을 bondCd 정렬 배치 + BSISE_T/BHOGA_T poke.
type BondWriter struct {
	buf  []byte
	slot map[string]int
}

// NewBondWriter — 버퍼(mmap 또는 테스트)로 생성.
func NewBondWriter(buf []byte) (*BondWriter, error) {
	if len(buf) < BondShmSize {
		return nil, fmt.Errorf("krxshm: 채권 버퍼 부족 %d < %d", len(buf), BondShmSize)
	}
	return &BondWriter{buf: buf, slot: map[string]int{}}, nil
}

// Layout — 채권코드 집합을 정렬 slot 배치 + bondCd/헤더 기록.
func (w *BondWriter) Layout(codes map[string]string) error { // code → 약명(ksNm)
	list := make([]string, 0, len(codes))
	for c := range codes {
		list = append(list, c)
	}
	sort.Strings(list)
	if len(list) > MaxBond {
		return fmt.Errorf("krxshm: 채권수 %d > MAX_BITEM %d", len(list), MaxBond)
	}
	w.slot = make(map[string]int, len(list))
	for i, c := range list {
		w.slot[c] = i
		e := bondEntryOff(i)
		w.putFixed(e+bOffCd, stdCdLen, c)
		w.putFixed(e+bOffShort, 40, codes[c])
	}
	binary.LittleEndian.PutUint32(w.buf[bOffMaxN:], uint32(MaxBond))
	binary.LittleEndian.PutUint32(w.buf[bOffUseN:], uint32(len(list)))
	return nil
}

// UpdateTrade — 채권 체결 → BSISE_T in-place.
func (w *BondWriter) UpdateTrade(q BondQuote) error {
	i, ok := w.slot[q.Code]
	if !ok {
		return fmt.Errorf("krxshm: 미배치 채권 %q", q.Code)
	}
	s := bondEntryOff(i) + bOffBsise
	w.putF64(s+bsBPrc, q.BasePrc)
	w.putF64(s+bsEPrc, q.Last)
	w.putF64(s+bsOYld, q.OYield)
	w.putF64(s+bsHYld, q.HYield)
	w.putF64(s+bsLYld, q.LYield)
	w.putF64(s+bsEYld, q.Yield)
	binary.LittleEndian.PutUint64(w.buf[s+bsAVol:], q.AccVol)
	w.putF64(s+bsYPrc, q.PrevClose)
	w.putF64(s+bsYYld, q.PrevYield)
	w.putF64(s+bsDiff, q.Diff)
	binary.LittleEndian.PutUint32(w.buf[s+bsRate:], math.Float32bits(float32(q.Rate)))
	w.putByte(s+bsSign, q.Sign)
	return nil
}

// UpdateBook — 채권 호가 → BHOGA_T in-place.
func (w *BondWriter) UpdateBook(b BondBook) error {
	i, ok := w.slot[b.Code]
	if !ok {
		return fmt.Errorf("krxshm: 미배치 채권 %q (호가)", b.Code)
	}
	h := bondEntryOff(i) + bOffBhoga
	binary.LittleEndian.PutUint64(w.buf[h+bhStVol:], b.AskTot)
	binary.LittleEndian.PutUint64(w.buf[h+bhBtVol:], b.BidTot)
	for j := 0; j < NBHoga; j++ {
		if j < len(b.Ask) {
			o := h + bhShoga + j*bhUStr
			w.putF64(o, b.Ask[j].Prc)
			binary.LittleEndian.PutUint64(w.buf[o+8:], b.Ask[j].Vol)
			w.putF64(o+16, b.Ask[j].Yld)
		}
		if j < len(b.Bid) {
			o := h + bhBhoga + j*bhUStr
			w.putF64(o, b.Bid[j].Prc)
			binary.LittleEndian.PutUint64(w.buf[o+8:], b.Bid[j].Vol)
			w.putF64(o+16, b.Bid[j].Yld)
		}
	}
	return nil
}

func (w *BondWriter) Has(code string) bool { _, ok := w.slot[code]; return ok }

func (w *BondWriter) putFixed(off, n int, s string) {
	for i := 0; i < n; i++ {
		if i < len(s) {
			w.buf[off+i] = s[i]
		} else {
			w.buf[off+i] = 0
		}
	}
}
func (w *BondWriter) putF64(off int, v float64) {
	binary.LittleEndian.PutUint64(w.buf[off:], math.Float64bits(v))
}
func (w *BondWriter) putByte(off int, s string) {
	if s == "" {
		w.buf[off] = 0x20
	} else {
		w.buf[off] = s[0]
	}
}
