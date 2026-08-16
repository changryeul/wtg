package krxshm

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Quote 는 한 종목의 SHM 적재 값 (FSISE_T 로 poke 되는 필드만). aAmt(long double)는
// Go 표현 불가라 미기록(0=유효 0.0L). trn AP 가 읽는 현재가/등락/정산 중심.
type Quote struct {
	Code                                 string // 표준종목코드 (12)
	ShortCode                            string // 단축코드 (9)
	BasePrc, Open, High, Low, Last       float64
	PrevClose, Diff, Settle, FinalSettle float64
	Rate                                 float64 // float32 로 저장
	AccVol                               uint64
	Sign, SettleCd                       string // 부호(1)/정산구분(2)
	Halt                                 bool
}

// Writer 는 mmap 된 MFSISE_T 영역([]byte)에 종목을 slot 배치(futCd 정렬)하고 시세를
// in-place 갱신한다. l_mfread 가 futCd 로 bsearch 하므로 slot 은 정렬 유지.
type Writer struct {
	buf  []byte         // mmap 영역 (len == ShmSize)
	slot map[string]int // code → slot index
}

// NewWriter — mmap 버퍼로 Writer 생성 (버퍼는 writer_linux.go 가 제공).
func NewWriter(buf []byte) (*Writer, error) {
	if len(buf) < ShmSize {
		return nil, fmt.Errorf("krxshm: 버퍼 크기 부족 %d < %d", len(buf), ShmSize)
	}
	return &Writer{buf: buf, slot: map[string]int{}}, nil
}

// Layout 은 종목코드 집합을 futCd 정렬해 slot 에 배치하고 futCd/단축코드/헤더(useN,maxN)를
// 기록한다. 마스터(A006F) 수신 후 1회 호출; 이후 시세는 Update 로 in-place.
func (w *Writer) Layout(codes map[string]string) error { // code → shortCode
	list := make([]string, 0, len(codes))
	for c := range codes {
		list = append(list, c)
	}
	sort.Strings(list) // strncmp 정렬과 동일 (ASCII)
	if len(list) > MaxItem {
		return fmt.Errorf("krxshm: 종목수 %d > MAX_FITEM %d", len(list), MaxItem)
	}
	w.slot = make(map[string]int, len(list))
	for i, c := range list {
		w.slot[c] = i
		e := entryOff(i)
		w.putFixed(e+offFutCd, stdCdLen, c)
		w.putFixed(e+offShrtCd, shtCdLen, codes[c])
	}
	binary.LittleEndian.PutUint32(w.buf[offMaxN:], uint32(MaxItem))
	binary.LittleEndian.PutUint32(w.buf[offUseN:], uint32(len(list)))
	return nil
}

// Update 는 이미 Layout 된 종목의 FSISE_T 필드를 in-place 갱신 (정렬 불변).
func (w *Writer) Update(q Quote) error {
	i, ok := w.slot[q.Code]
	if !ok {
		return fmt.Errorf("krxshm: 미배치 종목 %q (Layout 선행 필요)", q.Code)
	}
	f := entryOff(i) + offFsise
	w.putF64(f+fsBPrc, q.BasePrc)
	w.putF64(f+fsOPrc, q.Open)
	w.putF64(f+fsHPrc, q.High)
	w.putF64(f+fsLPrc, q.Low)
	w.putF64(f+fsEPrc, q.Last)
	w.putF64(f+fsYPrc, q.PrevClose)
	w.putF64(f+fsDiff, q.Diff)
	w.putF64(f+fsSPrc, q.Settle)
	w.putF64(f+fsLsPr, q.FinalSettle)
	binary.LittleEndian.PutUint64(w.buf[f+fsAVol:], q.AccVol)
	binary.LittleEndian.PutUint32(w.buf[f+fsRate:], math.Float32bits(float32(q.Rate)))
	w.putFixed(f+fsSPcd, 2, q.SettleCd)
	w.putByte(f+fsSign, q.Sign)
	if q.Halt {
		w.buf[f+fsHalt] = 'Y'
	} else {
		w.buf[f+fsHalt] = 0x20
	}
	return nil
}

// Level — 호가 한 단계 (가격/잔량/건수).
type Level struct {
	Prc      float64
	Vol, Cnt uint64
}

// Book — 파생 호가(5단) FHOGA_T 적재값.
type Book struct {
	Code                           string
	AskTot, BidTot, AskCnt, BidCnt uint64
	ExpPrc                         float64
	ExpVol                         uint64
	Ask, Bid                       []Level
}

// UpdateBook 은 배치된 종목의 FHOGA_T 를 in-place 갱신.
func (w *Writer) UpdateBook(b Book) error {
	i, ok := w.slot[b.Code]
	if !ok {
		return fmt.Errorf("krxshm: 미배치 종목 %q (호가)", b.Code)
	}
	h := entryOff(i) + offFhoga
	binary.LittleEndian.PutUint64(w.buf[h+fhStVol:], b.AskTot)
	binary.LittleEndian.PutUint64(w.buf[h+fhBtVol:], b.BidTot)
	binary.LittleEndian.PutUint64(w.buf[h+fhSaCnt:], b.AskCnt)
	binary.LittleEndian.PutUint64(w.buf[h+fhBaCnt:], b.BidCnt)
	w.putF64(h+fhExPrc, b.ExpPrc)
	binary.LittleEndian.PutUint64(w.buf[h+fhExVol:], b.ExpVol)
	for j := 0; j < NHoga; j++ {
		if j < len(b.Ask) {
			o := h + fhShoga + j*fhUStr
			w.putF64(o, b.Ask[j].Prc)
			binary.LittleEndian.PutUint64(w.buf[o+8:], b.Ask[j].Vol)
			binary.LittleEndian.PutUint64(w.buf[o+16:], b.Ask[j].Cnt)
		}
		if j < len(b.Bid) {
			o := h + fhBhoga + j*fhUStr
			w.putF64(o, b.Bid[j].Prc)
			binary.LittleEndian.PutUint64(w.buf[o+8:], b.Bid[j].Vol)
			binary.LittleEndian.PutUint64(w.buf[o+16:], b.Bid[j].Cnt)
		}
	}
	return nil
}

// Has — 종목이 배치돼 있는지.
func (w *Writer) Has(code string) bool { _, ok := w.slot[code]; return ok }

// putFixed — 고정폭 필드에 문자열(널패딩) 기록.
func (w *Writer) putFixed(off, n int, s string) {
	for i := 0; i < n; i++ {
		if i < len(s) {
			w.buf[off+i] = s[i]
		} else {
			w.buf[off+i] = 0
		}
	}
}
func (w *Writer) putF64(off int, v float64) {
	binary.LittleEndian.PutUint64(w.buf[off:], math.Float64bits(v))
}
func (w *Writer) putByte(off int, s string) {
	if s == "" {
		w.buf[off] = 0x20
	} else {
		w.buf[off] = s[0]
	}
}
