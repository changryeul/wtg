package pricing

import (
	"errors"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Package crossrate — 재정통화 (cross-rate) 합성 산식.
//
// 시장 cooker 는 direct pair (USD/KRW, USD/JPY, EUR/USD 등) 만 전송하고,
// mci-price 의 마스터 메모리에 그 호가가 cache 된다. 이 cache 의 두 leg 호가
// 로부터 cross pair 의 호가를 합성하는 게 본 모듈의 책임.
//
// 예시:
//
//	EUR/KRW    = EUR/USD × USD/KRW                  (OpA=mul, OpB=mul, scale=1)
//	100JPY/KRW = USD/KRW × (1 / USD/JPY) × 100      (OpA=mul, OpB=div, scale=100)
//	CNY/KRW    = USD/KRW × (1 / USD/CNY)            (OpA=mul, OpB=div, scale=1)
//
// 합성 정책 — worse-side (보수적):
//
//	bid_result = scale × contribBid(LegA, OpA) × contribBid(LegB, OpB)
//	ask_result = scale × contribAsk(LegA, OpA) × contribAsk(LegB, OpB)
//
//	contribBid(leg, mul) = leg.bid                  // 그대로 (낮은 쪽)
//	contribBid(leg, div) = 1 / leg.ask              // 분모를 ask 로 (역수도 낮은 쪽)
//	contribAsk(leg, mul) = leg.ask                  // 그대로 (높은 쪽)
//	contribAsk(leg, div) = 1 / leg.bid              // 분모를 bid 로 (역수도 높은 쪽)
//
// 이 산식의 의미는 "각 side 가 고객에게 최대한 불리한 leg 호가만 사용" — 어느
// 한 leg 의 호가가 보수적이면 결과도 보수적 (= arbitrage 자유 보장).

// CrossOp — cross formula 에서 leg 의 연산자.
type CrossOp string

const (
	// CrossOpMul — leg 호가를 그대로 곱.
	CrossOpMul CrossOp = "mul"
	// CrossOpDiv — leg 호가의 역수 (1/leg). 분자/분모 side 는 worse-side 규칙.
	CrossOpDiv CrossOp = "div"
)

// CrossFormula — 두 leg 의 cross 합성 산식 정의. PairEntry (또는 SymbolMap)
// 의 cross 항목에 저장되어 운영자가 admin UI 에서 편집.
//
// Scale 의 일반 사용:
//   - 1.0 (default) : 일반 cross. EUR/KRW = EUR/USD × USD/KRW.
//   - 100.0         : 한국 시장 컨벤션의 100JPY/KRW (스케일 100배).
//   - 0.01          : 1/100 스케일 — 거의 안 쓰임.
//
// Scale 이 <= 0 이면 1.0 fallback (zero-value 안전성).
type CrossFormula struct {
	LegA  session.Pair `json:"leg_a"`  // 예: "EUR/USD"
	OpA   CrossOp      `json:"op_a"`   // "mul" | "div"
	LegB  session.Pair `json:"leg_b"`  // 예: "USD/KRW"
	OpB   CrossOp      `json:"op_b"`   // "mul" | "div"
	Scale float64      `json:"scale,omitempty"`
}

// CrossInput — 한 leg 의 현재 호가. 호출자가 mci-price 의 마스터 메모리에서
// 조회해 전달.
type CrossInput struct {
	Bid float64
	Ask float64
}

// CrossResult — 합성된 cross 호가.
type CrossResult struct {
	Bid float64
	Ask float64
}

// 에러.
var (
	ErrCrossInvalidOp    = errors.New("pricing: cross op must be 'mul' or 'div'")
	ErrCrossInvalidInput = errors.New("pricing: cross leg bid/ask must be > 0")
	ErrCrossDivByZero    = errors.New("pricing: cross div op requires nonzero leg side")
)

// ComputeCross — 두 leg 의 호가로 cross 합성. worse-side 산식 + scale.
//
// 검증:
//   - OpA / OpB 가 "mul" 또는 "div" 만 허용. 그 외는 ErrCrossInvalidOp.
//   - 모든 leg 의 bid/ask 가 양수 (> 0) — 0 이나 음수는 ErrCrossInvalidInput.
//     (호가 자체는 항상 양수가 정상 — zero quote 는 stale / 미수신 신호.)
//   - div op 의 분모가 0 이면 ErrCrossDivByZero (위 검증으로 보통 catch 됨).
//
// 호출자 책임:
//   - LegA / LegB 자체는 본 함수가 검증 안 함 — 산식이 유효한지 (예: LegA ==
//     cross_pair 자기 자신 같은 cycle) 는 SymbolMap 빌드 시점에 한 번 검증.
//   - Scale 의 의미 (100JPY/KRW 의 100 등) 는 운영자 정의를 신뢰.
func ComputeCross(f CrossFormula, a, b CrossInput) (CrossResult, error) {
	if f.OpA != CrossOpMul && f.OpA != CrossOpDiv {
		return CrossResult{}, ErrCrossInvalidOp
	}
	if f.OpB != CrossOpMul && f.OpB != CrossOpDiv {
		return CrossResult{}, ErrCrossInvalidOp
	}
	if a.Bid <= 0 || a.Ask <= 0 || b.Bid <= 0 || b.Ask <= 0 {
		return CrossResult{}, ErrCrossInvalidInput
	}
	scale := f.Scale
	if scale <= 0 {
		scale = 1.0
	}

	bidA, err := crossBidContrib(a, f.OpA)
	if err != nil {
		return CrossResult{}, err
	}
	askA, err := crossAskContrib(a, f.OpA)
	if err != nil {
		return CrossResult{}, err
	}
	bidB, err := crossBidContrib(b, f.OpB)
	if err != nil {
		return CrossResult{}, err
	}
	askB, err := crossAskContrib(b, f.OpB)
	if err != nil {
		return CrossResult{}, err
	}

	return CrossResult{
		Bid: scale * bidA * bidB,
		Ask: scale * askA * askB,
	}, nil
}

func crossBidContrib(leg CrossInput, op CrossOp) (float64, error) {
	if op == CrossOpMul {
		return leg.Bid, nil
	}
	if leg.Ask == 0 {
		return 0, ErrCrossDivByZero
	}
	return 1.0 / leg.Ask, nil
}

func crossAskContrib(leg CrossInput, op CrossOp) (float64, error) {
	if op == CrossOpMul {
		return leg.Ask, nil
	}
	if leg.Bid == 0 {
		return 0, ErrCrossDivByZero
	}
	return 1.0 / leg.Bid, nil
}
