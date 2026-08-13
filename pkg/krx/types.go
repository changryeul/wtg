// Package krx 는 KRX 선물/옵션/채권 실시간 시세를 web 용 JSON envelope 으로 변환한다.
//
// 트랙2 — WTG 가 KRX 멀티캐스트에서 원 TR(A306F 체결/B606F 호가/A006F 마스터/H306F
// 정산가 · A301K 채권체결/B601K 채권호가/A001B 채권마스터)을 직접 파싱한다
// (C 피드/win 프레임워크 무의존). 원 TR 레이아웃은 sise inc/*.h (고정폭 ASCII, BE 아님,
// 소수점 포함 ASCII 숫자 — C l_s2d=atof 와 동형). 각 codec 은 별도 파일:
// a306f.go / b606f.go / master.go / h306f.go / a301k.go / b601k.go / bondmaster.go.
//
// 본 파일은 codec 공용 envelope 타입(FutTrade/FutBook/BondTrade/BondBook 등)과
// 고정폭 파싱 헬퍼(fstr/ffloat/fint)를 모은다.
package krx

import (
	"strconv"
	"strings"
)

// nHogaLevel 은 호가 단계 수 (5단).
const nHogaLevel = 5

// FutTrade 는 선물/옵션 체결 시세 JSON envelope. 가격은 소수 2자리, diff/rate 는
// 부호 내장. 전일대비(prevClose/diff/rate/sign)는 마스터(A006F) join,
// 정산가(settle/finalSettle/settleCd)는 H306F, 직전대비(cdiff/crate/csign)는 TR 내부.
type FutTrade struct {
	Kind         string  `json:"kind"` // 항상 "fut.trade"
	Code         string  `json:"code"` // 종목코드
	Time         string  `json:"time"` // HHMMSSuuuuuu (12자리)
	BasePrc      float64 `json:"basePrc"`
	Open         float64 `json:"open"`
	High         float64 `json:"high"`
	Low          float64 `json:"low"`
	Last         float64 `json:"last"`         // 현재가(종가) eprc
	PrevClose    float64 `json:"prevClose"`    // 전일종가 yprc
	Diff         float64 `json:"diff"`         // 전일대비 (부호 내장)
	Rate         float64 `json:"rate"`         // 전일대비 등락률 (부호 내장)
	Sign         string  `json:"sign"`         // 전일대비 방향부호 +/-/' '
	Settle       float64 `json:"settle"`       // 정산가 sprc (H306F)
	FinalSettle  float64 `json:"finalSettle"`  // 최종결제가 lspr (H306F)
	SettleCd     string  `json:"settleCd"`     // 정산가구분코드 spcd (H306F)
	Bs           string  `json:"bs"`           // 최종 매도매수구분 ' '/0/1/2
	Tvol         int64   `json:"tvol"`         // 누적 체결수량
	Tamt         float64 `json:"tamt"`         // 누적 거래대금
	Cprc         float64 `json:"cprc"`         // 체결가 (이번 틱)
	Cvol         int64   `json:"cvol"`         // 거래량 (이번 틱)
	NearPrc      float64 `json:"nearPrc"`      // 근월물체결가
	FarPrc       float64 `json:"farPrc"`       // 원월물체결가
	UpLimit      float64 `json:"upLimit"`      // 동적상한가
	DnLimit      float64 `json:"dnLimit"`      // 동적하한가
	PrevTradePrc float64 `json:"prevTradePrc"` // 직전가 pprc
	Cdiff        float64 `json:"cdiff"`        // 직전대비 (부호 내장)
	Crate        float64 `json:"crate"`        // 직전대비 등락률 (부호 내장)
	Csign        string  `json:"csign"`        // 직전대비 방향부호 +/-/' '
}

// BookLevel 은 선물 호가 한 단계 (가격/잔량/건수).
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
	AskCnt int64       `json:"askCnt"` // 매도호가 유효건수
	BidCnt int64       `json:"bidCnt"` // 매수호가 유효건수
	ExpPrc float64     `json:"expPrc"` // 예상체결가
	ExpVol int64       `json:"expVol"` // 예상체결수량
	Ask    []BookLevel `json:"ask"`    // 매도호가 5단
	Bid    []BookLevel `json:"bid"`    // 매수호가 5단
}

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
