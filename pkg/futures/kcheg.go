// Package futures 는 KRX 선물/파생 실시간 시세 전문(RTS)을 web 용 JSON envelope 으로
// 변환한다. 원 전문 레이아웃은 유안타 선물 피드의 fpush.h/bpush.h (고정폭 ASCII).
//
// 기존 피드(C)는 KRX TR 을 파싱해 KA(체결)/KB(호가) 전문을 이미 생성한다. WTG 는 그
// 깨끗한 KA/KB 를 받아 JSON 으로 디코드해 web ws 로 내보낸다 (docs/futures-sise-design.md).
package futures

import (
	"fmt"
	"strconv"
	"strings"
)

// SZKCheg 는 KF_CHEG_RTS_T (거래소유형 "KA", 국내선물 체결) 전문 크기.
// char[] 만으로 구성돼 padding 이 없어 필드 크기 합과 일치 (fpush.h).
const SZKCheg = 234

// FutTrade 는 선물 체결 시세 JSON envelope. 숫자는 원 전문의 %-9.02f/%ld 를 그대로
// 파싱한 값 (가격 소수 2자리, diff/rate 는 부호 내장).
type FutTrade struct {
	Kind      string  `json:"kind"` // 항상 "fut.trade"
	Code      string  `json:"code"` // 종목코드
	Time      string  `json:"time"` // HHMMSSuuuuuu (12자리)
	BasePrc   float64 `json:"basePrc"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Last      float64 `json:"last"`      // 현재가(종가) eprc
	PrevClose float64 `json:"prevClose"` // 전일종가 yprc
	Diff      float64 `json:"diff"`      // 전일대비 (부호 내장)
	Settle    float64 `json:"settle"`    // 정산가 sprc
	Rate      float64 `json:"rate"`      // 전일대비 등락률 (부호 내장)
	Sign      string  `json:"sign"`      // 방향부호 +/-/' '
	Bs        string  `json:"bs"`        // 최종 매도매수구분 ' '/0/1/2
	Tvol      int64   `json:"tvol"`      // 누적 체결수량
	Tamt      float64 `json:"tamt"`      // 누적 거래대금
	Cprc      float64 `json:"cprc"`      // 체결가 (이번 틱)
	Cvol      int64   `json:"cvol"`      // 거래량 (이번 틱)
	NearPrc   float64 `json:"nearPrc"`   // 근월물체결가
	FarPrc    float64 `json:"farPrc"`    // 원월물체결가
	UpLimit   float64 `json:"upLimit"`   // 동적상한가
	DnLimit   float64 `json:"dnLimit"`   // 동적하한가
}

// DecodeKChe 는 KA 고정폭 전문(≥234B)을 FutTrade 로 파싱한다.
// 앞 2바이트가 "KA" 가 아니면 오류. 숫자 필드는 좌측정렬 공백패딩이라 Trim 후 파싱.
func DecodeKChe(b []byte) (*FutTrade, error) {
	if len(b) < SZKCheg {
		return nil, fmt.Errorf("futures: KA 전문 길이 미달 (%d < %d)", len(b), SZKCheg)
	}
	if t := fstr(b, 0, 2); t != "KA" {
		return nil, fmt.Errorf("futures: KA 전문 아님 (type=%q)", t)
	}
	// 오프셋은 fpush.h KF_CHEG_RTS_T 필드 순서 (색상 필드 ocol/hcol/lcol/ecol 포함).
	return &FutTrade{
		Kind:      "fut.trade",
		Code:      fstr(b, 2, 12),
		BasePrc:   ffloat(b, 14, 9),
		Open:      ffloat(b, 24, 9),
		High:      ffloat(b, 34, 9),
		Low:       ffloat(b, 44, 9),
		Last:      ffloat(b, 54, 9),
		PrevClose: ffloat(b, 63, 9),
		Diff:      ffloat(b, 72, 9),
		Settle:    ffloat(b, 81, 9),
		Rate:      ffloat(b, 99, 6),
		Sign:      fstr(b, 107, 1),
		Time:      fstr(b, 108, 12),
		Bs:        fstr(b, 120, 1),
		Tvol:      fint(b, 121, 12),
		Tamt:      ffloat(b, 133, 22),
		Cprc:      ffloat(b, 155, 9),
		Cvol:      fint(b, 164, 9),
		NearPrc:   ffloat(b, 173, 9),
		FarPrc:    ffloat(b, 182, 9),
		UpLimit:   ffloat(b, 191, 9),
		DnLimit:   ffloat(b, 200, 9),
	}, nil
}

// fstr 은 [off,off+n) 을 공백트림 문자열로.
func fstr(b []byte, off, n int) string {
	return strings.TrimSpace(string(b[off : off+n]))
}

// ffloat 은 고정폭 숫자필드 → float64 (공백/비수치 = 0).
func ffloat(b []byte, off, n int) float64 {
	s := fstr(b, off, n)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// fint 은 고정폭 정수필드 → int64 (공백/비수치 = 0).
func fint(b []byte, off, n int) int64 {
	s := fstr(b, off, n)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
