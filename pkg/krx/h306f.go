package krx

import "fmt"

// 트랙2 — H306F(IFMSRID0009, 선물종목 정산가격 데이터) 원 TR → fut.settle.
// 정산가/최종결제가는 A306F 체결에는 없고 H306F 로 별도 분배된다 (C 는 fsise.sPrc 로
// 보관해 매 KA push 에 실어 보냄). WTG 는 code 별로 캐시해 후속 체결 enrich 에 쓴다.
// 앞 5바이트 TR코드 "H306F" (datc[2]"H3"+infc[3]"06F").

// SZH306F 는 H306F_T 크기 (H306F.h, endc 끝 53).
const SZH306F = 53

// FutSettle 는 선물 정산가격 JSON envelope + 체결 enrich 소스.
type FutSettle struct {
	Kind          string  `json:"kind"`          // "fut.settle"
	Code          string  `json:"code"`          // 종목코드
	Settle        float64 `json:"settle"`        // 정산가격 sprc
	SettleCd      string  `json:"settleCd"`      // 정산가격구분코드 spcd (10~41)
	FinalSettle   float64 `json:"finalSettle"`   // 최종결제가격 lspr
	FinalSettleCd string  `json:"finalSettleCd"` // 최종결제가격구분코드 lspc (1~6)
}

// DecodeH306F 는 원 정산가격 전문(≥53B) → FutSettle.
// 오프셋: datc2+infc3=5, code@5[12], infn@17[6], sprc@23[18], spcd@41[2],
// lspr@43[8], lspc@51[1] (H306F.h).
func DecodeH306F(b []byte) (*FutSettle, error) {
	if len(b) < SZH306F {
		return nil, fmt.Errorf("krx: H306F 길이 미달 (%d < %d)", len(b), SZH306F)
	}
	if tr := fstr(b, 0, 5); tr != "H306F" {
		return nil, fmt.Errorf("krx: H306F 아님 (tr=%q)", tr)
	}
	return &FutSettle{
		Kind:          "fut.settle",
		Code:          fstr(b, 5, 12),    // code
		Settle:        ffloat(b, 23, 18), // sprc 정산가격
		SettleCd:      fstr(b, 41, 2),    // spcd 정산가격구분코드
		FinalSettle:   ffloat(b, 43, 8),  // lspr 최종결제가격
		FinalSettleCd: fstr(b, 51, 1),    // lspc 최종결제가격구분코드
	}, nil
}
