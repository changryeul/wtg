package krx

import "fmt"

// 트랙2 — A301K(IFMSRPD0027, 채권 체결) 원 TR → bond.trade.
// 앞 5바이트 TR코드 "A301K" (datc[2]"A3"+infc[3]"01K").
//
// 원 A301K 는 BA(C 피드 출력)보다 lean: 직전대비/전일대비/부호가 없음 — 직전가(pPrc)
// 캐시로 직전대비, 마스터(A001B 기준가)로 전일대비를 만든다 (ws.go enrichBondTrade).
// 채권은 전일종가 TR 이 없어 A001B 기준가(bprc)를 전일대비 기준으로 쓴다.
// 숫자는 소수점 ASCII (C l_s2d=atof).

// SZA301K 는 A301K_T 크기 (A301K.h, endc 끝 223).
const SZA301K = 223

// DecodeA301K 는 원 채권 체결 전문(≥223B) → BondTrade (원 TR 가용 필드만).
// diff/rate/sign(직전대비)·yDiff/yRate/ySign(전일대비)은 join 전까지 0.
func DecodeA301K(b []byte) (*BondTrade, error) {
	if len(b) < SZA301K {
		return nil, fmt.Errorf("krx: A301K 길이 미달 (%d < %d)", len(b), SZA301K)
	}
	if tr := fstr(b, 0, 5); tr != "A301K" {
		return nil, fmt.Errorf("krx: A301K 아님 (tr=%q)", tr)
	}
	// 오프셋: datc2+infc3+seqn8+bdid2+ssid2=17, code@17[12], time@29[12],
	// cprc@41[11], cvol@52[10], cday@62[8], camt@70[22], tyld@92[13],
	// oprc@105[11], hprc@116[11], lprc@127[11], oyld@138[13], hyld@151[13],
	// lyld@164[13], tvol@177[15], tamt@192[22], sday@214[8].
	return &BondTrade{
		Kind:   "bond.trade",
		Code:   fstr(b, 17, 12),
		Time:   fstr(b, 29, 12),
		Last:   ffloat(b, 41, 11),  // cprc 체결가
		Cvol:   fint(b, 52, 10),    // cvol 거래량
		Camt:   ffloat(b, 70, 22),  // camt 거래대금
		Yield:  ffloat(b, 92, 13),  // tyld 체결수익률
		Open:   ffloat(b, 105, 11), // oprc 시가
		High:   ffloat(b, 116, 11), // hprc 고가
		Low:    ffloat(b, 127, 11), // lprc 저가
		OYield: ffloat(b, 138, 13), // oyld 시가수익률
		HYield: ffloat(b, 151, 13), // hyld 고가수익률
		LYield: ffloat(b, 164, 13), // lyld 저가수익률
		Tvol:   fint(b, 177, 15),   // tvol 누적체결수량
		Tamt:   ffloat(b, 192, 22), // tamt 누적거래대금
	}, nil
}
