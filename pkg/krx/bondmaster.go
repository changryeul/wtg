package krx

import "fmt"

// SZBondMaster 는 A001B(채권 종목정보, IFMSBTD0016) 전문 크기 (A001B.h A001B_T, endc 끝 308).
// A001B.h 는 누적 offset 주석이 없어 필드 크기 순차합으로 산출. char[] only, padding 없음.
const SZBondMaster = 308

// BondMaster 는 채권 종목 기본정보 JSON envelope.
type BondMaster struct {
	Kind           string  `json:"kind"`           // "bond.master"
	Code           string  `json:"code"`           // 종목코드(ISIN)
	Name           string  `json:"name"`           // 종목약명 ksnm
	BondType       string  `json:"bondType"`       // 소매채권분류 typc (GA국채/BA금융채/CA회사채/ET지방채/MA통안채/EC기타)
	ListStatus     string  `json:"listStatus"`     // 채권상장구분 bltc (Y상장/N비상장/D폐지/I미발행/E기타)
	Guarantee      string  `json:"guarantee"`      // 채권보증구분 gtcd
	InterestMethod string  `json:"interestMethod"` // 이자지급방법 cpcd
	ListDate       string  `json:"listDate"`       // 상장일 ltdt
	IssueDate      string  `json:"issueDate"`      // 발행일 isdt
	RedeemDate     string  `json:"redeemDate"`     // 상환일(만기) rddt
	IssueRate      float64 `json:"issueRate"`      // 채권발행율 isrt (양수:할인 음수:할증)
	CouponRate     float64 `json:"couponRate"`     // 표면이자율 cprt
	IssueAmt       float64 `json:"issueAmt"`       // 발행금액 isam
	ListAmt        float64 `json:"listAmt"`        // 상장금액 ltam
	BasePrc        float64 `json:"basePrc"`        // 기준가격 bprc
	PrevCouponDate string  `json:"prevCouponDate"` // 전기이자지급일 pcpd
	NextCouponDate string  `json:"nextCouponDate"` // 차기이자지급일 ncpd
	Halt           bool    `json:"halt"`           // 거래정지 halt
}

// DecodeA001B 는 A001B 원 채권마스터 전문(≥308B) → BondMaster.
func DecodeA001B(b []byte) (*BondMaster, error) {
	if len(b) < SZBondMaster {
		return nil, fmt.Errorf("krx: A001B 길이 미달 (%d < %d)", len(b), SZBondMaster)
	}
	// 앞 5바이트 TR코드 확인 (datc[2]+infc[3] = "A001B").
	if tr := fstr(b, 0, 5); tr != "A001B" {
		return nil, fmt.Errorf("krx: A001B 아님 (tr=%q)", tr)
	}
	return &BondMaster{
		Kind:           "bond.master",
		Code:           fstr(b, 27, 12),         // code
		BondType:       fstr(b, 45, 2),          // typc
		Name:           fstr(b, 47, 40),         // ksnm
		ListStatus:     fstr(b, 130, 1),         // bltc
		Guarantee:      fstr(b, 137, 1),         // gtcd
		InterestMethod: fstr(b, 138, 2),         // cpcd
		ListDate:       fstr(b, 140, 8),         // ltdt
		IssueDate:      fstr(b, 148, 8),         // isdt
		RedeemDate:     fstr(b, 156, 8),         // rddt (만기)
		IssueRate:      ffloat(b, 172, 13),      // isrt
		CouponRate:     ffloat(b, 185, 14),      // cprt 표면이자율
		IssueAmt:       ffloat(b, 208, 22),      // isam
		ListAmt:        ffloat(b, 230, 22),      // ltam
		Halt:           isHalt(fstr(b, 275, 1)), // halt
		PrevCouponDate: fstr(b, 276, 8),         // pcpd
		NextCouponDate: fstr(b, 284, 8),         // ncpd
		BasePrc:        ffloat(b, 294, 11),      // bprc
	}, nil
}
