package krx

import "fmt"

// 트랙2 — WTG 가 KRX 멀티캐스트를 직접 받아 원 TR 을 파싱한다 (C 피드/win 프레임워크 무의존).
// A306F(IFMSRPD0036, 파생 체결) 원 TR → fut.trade. 앞 5바이트 TR코드 "A306F"(datc"A3"+infc"06F").
//
// 원 A306F 는 KA(C 피드 출력)보다 lean 하다: 전일대비/등락률/정산가/전일종가가 없음 —
// 그건 마스터(A006F 전일종가)와 계산해 만드는 값이라 후속 enrichment 대상.
// 숫자는 소수점 ASCII (C 의 l_s2d=atof 로 검증).

// SZA306F 는 A306F_T 크기 (A306F.h, endc 끝 173).
const SZA306F = 173

// DecodeA306F 는 원 파생 체결 전문(≥173B) → FutTrade (원 TR 가용 필드만).
// diff/sign/rate/prevClose/settle/basePrc 는 마스터 join 전까지 0.
func DecodeA306F(b []byte) (*FutTrade, error) {
	if len(b) < SZA306F {
		return nil, fmt.Errorf("krx: A306F 길이 미달 (%d < %d)", len(b), SZA306F)
	}
	if tr := fstr(b, 0, 5); tr != "A306F" {
		return nil, fmt.Errorf("krx: A306F 아님 (tr=%q)", tr)
	}
	last := ffloat(b, 47, 9) // cprc 체결가
	return &FutTrade{
		Kind:    "fut.trade",
		Code:    fstr(b, 17, 12),    // code (datc2+infc3+seqn8+bdid2+ssid2=17)
		Time:    fstr(b, 35, 12),    // time
		Last:    last,               // cprc → 현재가(=체결가)
		Cprc:    last,               // cprc → 체결가(이번 틱)
		Cvol:    fint(b, 56, 9),     // cvol 거래량
		NearPrc: ffloat(b, 65, 9),   // nprc 근월물
		FarPrc:  ffloat(b, 74, 9),   // fprc 원월물
		Open:    ffloat(b, 83, 9),   // oprc 시가
		High:    ffloat(b, 92, 9),   // hprc 고가
		Low:     ffloat(b, 101, 9),  // lprc 저가
		Tvol:    fint(b, 119, 12),   // tvol 누적거래량
		Tamt:    ffloat(b, 131, 22), // tamt 누적거래대금
		Bs:      fstr(b, 153, 1),    // ftcd 최종매도매수구분
		UpLimit: ffloat(b, 154, 9),  // uldp 동적상한
		DnLimit: ffloat(b, 163, 9),  // lldp 동적하한
	}, nil
}
