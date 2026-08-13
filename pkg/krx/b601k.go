package krx

import "fmt"

// 트랙2 — B601K(IFMSRPD0023, 일반채권/국고채권 우선호가 5단) 원 TR → bond.book.
// 앞 5바이트 TR코드 "B601K" (datc[2]"B6"+infc[3]"01K").
// BB(C 피드 출력)와 달리 매도/매수를 hoga[5] 한 배열에 interleave
// (sprc/bprc/svol/bvol/syld/byld) + 채권은 색상/건수 없이 수익률(yld) 동반.

// SZB601K 는 B601K_T 크기 (B601K.h, endc 끝 462).
const SZB601K = 462

// b601kHogaSz 는 hoga 한 단계 크기 (sprc11+bprc11+svol15+bvol15+syld13+byld13).
const b601kHogaSz = 78

// DecodeB601K 는 원 채권 호가 전문(≥462B) → BondBook.
func DecodeB601K(b []byte) (*BondBook, error) {
	if len(b) < SZB601K {
		return nil, fmt.Errorf("krx: B601K 길이 미달 (%d < %d)", len(b), SZB601K)
	}
	if tr := fstr(b, 0, 5); tr != "B601K" {
		return nil, fmt.Errorf("krx: B601K 아님 (tr=%q)", tr)
	}
	bb := &BondBook{
		Kind:   "bond.book",
		Code:   fstr(b, 17, 12), // code
		Time:   fstr(b, 29, 12), // time
		Ask:    make([]BondLevel, nHogaLevel),
		Bid:    make([]BondLevel, nHogaLevel),
		AskTot: fint(b, 431, 15), // stvl 매도총잔량 (hoga[5]@41, 5*78=390 → 431)
		BidTot: fint(b, 446, 15), // btvl 매수총잔량
	}
	// hoga[5] @41 — 각 단계 sprc@0/bprc@11/svol@22/bvol@37/syld@52/byld@65.
	for i := 0; i < nHogaLevel; i++ {
		o := 41 + i*b601kHogaSz
		bb.Ask[i] = BondLevel{Prc: ffloat(b, o+0, 11), Vol: fint(b, o+22, 15), Yld: ffloat(b, o+52, 13)}
		bb.Bid[i] = BondLevel{Prc: ffloat(b, o+11, 11), Vol: fint(b, o+37, 15), Yld: ffloat(b, o+65, 13)}
	}
	return bb, nil
}
