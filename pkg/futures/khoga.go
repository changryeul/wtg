package futures

import (
	"fmt"
)

// SZKHoga 는 KF_HOGA_RTS_T (거래소유형 "KB", 국내선물 호가) 전문 크기.
// char[] 만이라 padding 없음: 헤더 122 + sell[5]*24 + buy[5]*24 = 362 (fpush.h).
const SZKHoga = 362

// nHogaLevel 은 호가 단계 수 (5단).
const nHogaLevel = 5

// hogaEntrySz 는 sell/buy 한 단계 크기 (pco1+prc9+vol9+cnt5).
const hogaEntrySz = 24

// BookLevel 은 호가 한 단계 (가격/잔량/건수).
type BookLevel struct {
	Prc float64 `json:"prc"` // 호가
	Vol int64   `json:"vol"` // 호가잔량
	Cnt int64   `json:"cnt"` // 주문건수
}

// FutBook 은 선물 호가(5단) JSON envelope.
type FutBook struct {
	Kind   string      `json:"kind"` // 항상 "fut.book"
	Code   string      `json:"code"`
	Time   string      `json:"time"`
	AskTot int64       `json:"askTot"` // 매도호가 총잔량 stvl
	BidTot int64       `json:"bidTot"` // 매수호가 총잔량 btvl
	AskCnt int64       `json:"askCnt"` // 매도호가 유효건수 savl
	BidCnt int64       `json:"bidCnt"` // 매수호가 유효건수 bavl
	ExpPrc float64     `json:"expPrc"` // 예상체결가 xprc
	ExpVol int64       `json:"expVol"` // 예상체결수량 xvol
	Ask    []BookLevel `json:"ask"`    // 매도호가 5단 (sell)
	Bid    []BookLevel `json:"bid"`    // 매수호가 5단 (buy)
}

// DecodeKHoga 는 KB 고정폭 전문(≥362B)을 FutBook 으로 파싱한다.
func DecodeKHoga(b []byte) (*FutBook, error) {
	if len(b) < SZKHoga {
		return nil, fmt.Errorf("futures: KB 전문 길이 미달 (%d < %d)", len(b), SZKHoga)
	}
	if t := fstr(b, 0, 2); t != "KB" {
		return nil, fmt.Errorf("futures: KB 전문 아님 (type=%q)", t)
	}
	fb := &FutBook{
		Kind:   "fut.book",
		Code:   fstr(b, 2, 12),
		Time:   fstr(b, 14, 12),
		AskTot: fint(b, 39, 12),   // stvl (stco@26,stdf@27,stvl@39)
		BidTot: fint(b, 64, 12),   // btvl (btco@51,btdf@52,btvl@64)
		AskCnt: fint(b, 76, 12),   // savl
		BidCnt: fint(b, 88, 12),   // bavl
		ExpPrc: ffloat(b, 101, 9), // xprc (xpco@100)
		ExpVol: fint(b, 110, 12),  // xvol
		Ask:    make([]BookLevel, nHogaLevel),
		Bid:    make([]BookLevel, nHogaLevel),
	}
	// sell[5] @122, buy[5] @242 — 각 단계 pco@0/prc@1/vol@10/cnt@19.
	sellOff, buyOff := 122, 122+nHogaLevel*hogaEntrySz
	for i := 0; i < nHogaLevel; i++ {
		so := sellOff + i*hogaEntrySz
		fb.Ask[i] = BookLevel{Prc: ffloat(b, so+1, 9), Vol: fint(b, so+10, 9), Cnt: fint(b, so+19, 5)}
		bo := buyOff + i*hogaEntrySz
		fb.Bid[i] = BookLevel{Prc: ffloat(b, bo+1, 9), Vol: fint(b, bo+10, 9), Cnt: fint(b, bo+19, 5)}
	}
	return fb, nil
}
