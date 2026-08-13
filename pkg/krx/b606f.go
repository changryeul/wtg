package krx

import "fmt"

// B606F(IFMSRPD0034, 파생 우선호가 5단) 원 TR → fut.book. 트랙2 원-TR 직파싱.
// KB(C 피드 출력)와 달리 매도/매수를 hoga[5] 한 배열에 interleave
// (sprc/bprc/svol/bvol/scnt/bcnt). 앞 5바이트 TR코드 "B606F".

// SZB606F 는 B606F_T 크기 (B606F.h, endc 끝 324).
const SZB606F = 324

// b606fHogaSz 는 hoga 한 단계 크기 (sprc9+bprc9+svol9+bvol9+scnt5+bcnt5).
const b606fHogaSz = 46

// DecodeB606F 는 원 파생 호가 전문(≥324B) → FutBook.
func DecodeB606F(b []byte) (*FutBook, error) {
	if len(b) < SZB606F {
		return nil, fmt.Errorf("krx: B606F 길이 미달 (%d < %d)", len(b), SZB606F)
	}
	if tr := fstr(b, 0, 5); tr != "B606F" {
		return nil, fmt.Errorf("krx: B606F 아님 (tr=%q)", tr)
	}
	fb := &FutBook{
		Kind:   "fut.book",
		Code:   fstr(b, 17, 12),   // code
		Time:   fstr(b, 35, 12),   // time
		AskTot: fint(b, 277, 9),   // stvl 매도총잔량 (hoga[5]@47, 5*46=230 → 277)
		BidTot: fint(b, 286, 9),   // btvl 매수총잔량
		AskCnt: fint(b, 295, 5),   // apvc 매도유효건수
		BidCnt: fint(b, 300, 5),   // bpvc 매수유효건수
		ExpPrc: ffloat(b, 305, 9), // etpr 예상체결가
		ExpVol: fint(b, 314, 9),   // etvl 예상체결수량
		Ask:    make([]BookLevel, nHogaLevel),
		Bid:    make([]BookLevel, nHogaLevel),
	}
	// hoga[5] @47 — 각 단계 sprc@0/bprc@9/svol@18/bvol@27/scnt@36/bcnt@41.
	for i := 0; i < nHogaLevel; i++ {
		o := 47 + i*b606fHogaSz
		fb.Ask[i] = BookLevel{Prc: ffloat(b, o+0, 9), Vol: fint(b, o+18, 9), Cnt: fint(b, o+36, 5)}
		fb.Bid[i] = BookLevel{Prc: ffloat(b, o+9, 9), Vol: fint(b, o+27, 9), Cnt: fint(b, o+41, 5)}
	}
	return fb, nil
}
