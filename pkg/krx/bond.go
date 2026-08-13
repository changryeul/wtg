package krx

import "fmt"

// 채권(원화채권) 실시간 시세 — bpush.h KB_CHEG_RTS_T("BA")/KB_HOGA_RTS_T("BB").
// 선물(KA/KB)과 달리 수익률(yield) 필드 + 가격 11자리 + 호가 date 필드.

// SZBACheg / SZBBHoga — 전문 크기 (char[] only, padding 없음).
const (
	SZBACheg = 230 // KB_CHEG_RTS_T
	SZBBHoga = 540 // KB_HOGA_RTS_T
)

// BondTrade 는 채권 체결 JSON envelope.
type BondTrade struct {
	Kind   string  `json:"kind"` // "bond.trade"
	Code   string  `json:"code"`
	Time   string  `json:"time"`
	Last   float64 `json:"last"`  // 체결가 cprc
	Yield  float64 `json:"yield"` // 체결수익률 cyld
	Diff   float64 `json:"diff"`  // 직전대비
	Rate   float64 `json:"rate"`  // 직전대비 등락률
	Sign   string  `json:"sign"`  // 직전대비 부호
	YDiff  float64 `json:"yDiff"` // 전일대비
	YRate  float64 `json:"yRate"` // 전일대비 등락률
	YSign  string  `json:"ySign"` // 전일대비 부호
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	OYield float64 `json:"oYield"` // 시가수익률
	HYield float64 `json:"hYield"`
	LYield float64 `json:"lYield"`
	Cvol   int64   `json:"cvol"` // 체결량
	Camt   float64 `json:"camt"` // 거래금액
	Tvol   int64   `json:"tvol"` // 누적 체결수량
	Tamt   float64 `json:"tamt"` // 누적 거래대금
}

// DecodeBACheg 는 BA(채권 체결, ≥230B) → BondTrade.
func DecodeBACheg(b []byte) (*BondTrade, error) {
	if len(b) < SZBACheg {
		return nil, fmt.Errorf("krx: BA 길이 미달 (%d < %d)", len(b), SZBACheg)
	}
	if t := fstr(b, 0, 2); t != "BA" {
		return nil, fmt.Errorf("krx: BA 아님 (type=%q)", t)
	}
	return &BondTrade{
		Kind:   "bond.trade",
		Code:   fstr(b, 2, 12),
		Time:   fstr(b, 14, 12),
		YSign:  fstr(b, 26, 1),
		YDiff:  ffloat(b, 27, 11),
		YRate:  ffloat(b, 38, 6),
		Sign:   fstr(b, 44, 1),
		Last:   ffloat(b, 45, 11),
		Yield:  ffloat(b, 56, 13),
		Diff:   ffloat(b, 69, 11),
		Rate:   ffloat(b, 80, 6),
		Cvol:   fint(b, 86, 10),
		Camt:   ffloat(b, 96, 22),
		Open:   ffloat(b, 119, 11),
		High:   ffloat(b, 131, 11),
		Low:    ffloat(b, 143, 11),
		OYield: ffloat(b, 154, 13),
		HYield: ffloat(b, 167, 13),
		LYield: ffloat(b, 180, 13),
		Tvol:   fint(b, 193, 15),
		Tamt:   ffloat(b, 208, 22),
	}, nil
}

// BondLevel 은 채권 호가 한 단계 (가격/잔량/수익률).
type BondLevel struct {
	Prc float64 `json:"prc"`
	Vol int64   `json:"vol"`
	Yld float64 `json:"yld"`
}

// BondBook 은 채권 호가(5단) JSON envelope.
type BondBook struct {
	Kind   string      `json:"kind"` // "bond.book"
	Code   string      `json:"code"`
	Date   string      `json:"date"`
	Time   string      `json:"time"`
	AskTot int64       `json:"askTot"` // 매도호가 총잔량 stvl
	BidTot int64       `json:"bidTot"` // 매수호가 총잔량 btvl
	Ask    []BondLevel `json:"ask"`
	Bid    []BondLevel `json:"bid"`
}

// bondEntrySz — 채권 호가 한 단계 (pco1+prc11+vol15+yld13).
const bondEntrySz = 40

// DecodeBBHoga 는 BB(채권 호가, ≥540B) → BondBook.
func DecodeBBHoga(b []byte) (*BondBook, error) {
	if len(b) < SZBBHoga {
		return nil, fmt.Errorf("krx: BB 길이 미달 (%d < %d)", len(b), SZBBHoga)
	}
	if t := fstr(b, 0, 2); t != "BB" {
		return nil, fmt.Errorf("krx: BB 아님 (type=%q)", t)
	}
	bb := &BondBook{
		Kind:   "bond.book",
		Code:   fstr(b, 2, 12),
		Date:   fstr(b, 14, 8),
		Time:   fstr(b, 22, 12),
		AskTot: fint(b, 50, 15), // stvl (stco@34,stdf@35,stvl@50)
		BidTot: fint(b, 81, 15), // btvl (btco@65,btdf@66,btvl@81)
		Ask:    make([]BondLevel, nHogaLevel),
		Bid:    make([]BondLevel, nHogaLevel),
	}
	// sell[5] @140, buy[5] @340 — pco@0/prc@1[11]/vol@12[15]/yld@27[13].
	sellOff, buyOff := 140, 140+nHogaLevel*bondEntrySz
	for i := 0; i < nHogaLevel; i++ {
		so := sellOff + i*bondEntrySz
		bb.Ask[i] = BondLevel{Prc: ffloat(b, so+1, 11), Vol: fint(b, so+12, 15), Yld: ffloat(b, so+27, 13)}
		bo := buyOff + i*bondEntrySz
		bb.Bid[i] = BondLevel{Prc: ffloat(b, bo+1, 11), Vol: fint(b, bo+12, 15), Yld: ffloat(b, bo+27, 13)}
	}
	return bb, nil
}
