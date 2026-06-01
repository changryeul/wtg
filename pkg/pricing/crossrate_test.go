package pricing

import (
	"errors"
	"testing"
)

// ─── 기본 산식 검증 ─────────────────────────────────────────────────────────

// EUR/KRW = EUR/USD × USD/KRW (단순 곱).
func TestComputeCross_MulMul(t *testing.T) {
	f := CrossFormula{LegA: "EUR/USD", OpA: CrossOpMul, LegB: "USD/KRW", OpB: CrossOpMul, Scale: 1}
	eurUsd := CrossInput{Bid: 1.0795, Ask: 1.0805}
	usdKrw := CrossInput{Bid: 1378.65, Ask: 1378.69}

	got, err := ComputeCross(f, eurUsd, usdKrw)
	if err != nil {
		t.Fatal(err)
	}
	// bid = 1.0795 × 1378.65 = 1488.302675
	// ask = 1.0805 × 1378.69 = 1489.674545
	wantBid := 1.0795 * 1378.65
	wantAsk := 1.0805 * 1378.69
	if !near(got.Bid, wantBid) {
		t.Errorf("bid = %v, want %v", got.Bid, wantBid)
	}
	if !near(got.Ask, wantAsk) {
		t.Errorf("ask = %v, want %v", got.Ask, wantAsk)
	}
	// 결과 spread 가 양쪽 leg spread 의 누적 (worse-side) 확인.
	resSpread := got.Ask - got.Bid
	legASpread := eurUsd.Ask - eurUsd.Bid
	legBSpread := usdKrw.Ask - usdKrw.Bid
	if resSpread <= legASpread*usdKrw.Bid || resSpread <= legBSpread*eurUsd.Bid {
		t.Errorf("spread accumulation 실패: resSpread=%v, legA=%v, legB=%v", resSpread, legASpread, legBSpread)
	}
}

// 100JPY/KRW = USD/KRW × (1 / USD/JPY) × 100.
// 한국 시장 컨벤션 — JPY 가 작아서 100엔당 표기.
func TestComputeCross_MulDiv_Scale100(t *testing.T) {
	f := CrossFormula{LegA: "USD/KRW", OpA: CrossOpMul, LegB: "USD/JPY", OpB: CrossOpDiv, Scale: 100}
	usdKrw := CrossInput{Bid: 1378.65, Ask: 1378.69}
	usdJpy := CrossInput{Bid: 151.45, Ask: 151.48}

	got, err := ComputeCross(f, usdKrw, usdJpy)
	if err != nil {
		t.Fatal(err)
	}
	// bid = 100 × 1378.65 × (1/151.48)  ← USD/JPY.ask 가 분모 (worse-side)
	// ask = 100 × 1378.69 × (1/151.45)  ← USD/JPY.bid 가 분모
	wantBid := 100 * 1378.65 / 151.48
	wantAsk := 100 * 1378.69 / 151.45
	if !near(got.Bid, wantBid) {
		t.Errorf("bid = %v, want %v", got.Bid, wantBid)
	}
	if !near(got.Ask, wantAsk) {
		t.Errorf("ask = %v, want %v", got.Ask, wantAsk)
	}
	// 시장적 합리성 — 100JPY/KRW 는 보통 900 ~ 1000 사이 (현재 환율).
	if got.Bid < 800 || got.Ask > 1100 {
		t.Errorf("결과가 시장 범위 밖: bid=%v ask=%v", got.Bid, got.Ask)
	}
}

// 스프레드 확대 정량 검증 — div op 가 worse-side 로 동작하는지.
// 좁은 leg + 좁은 leg → cross spread 가 양쪽 leg 의 비례 누적이어야.
func TestComputeCross_DivWidensSpread(t *testing.T) {
	// 두 leg 모두 spread = 0.10.
	f := CrossFormula{LegA: "USD/KRW", OpA: CrossOpMul, LegB: "USD/JPY", OpB: CrossOpDiv, Scale: 100}
	usdKrw := CrossInput{Bid: 1378.60, Ask: 1378.70}
	usdJpyTight := CrossInput{Bid: 151.40, Ask: 151.50}  // spread 0.10
	usdJpyWide := CrossInput{Bid: 151.30, Ask: 151.60}   // spread 0.30

	r1, _ := ComputeCross(f, usdKrw, usdJpyTight)
	r2, _ := ComputeCross(f, usdKrw, usdJpyWide)
	s1 := r1.Ask - r1.Bid
	s2 := r2.Ask - r2.Bid
	if s2 <= s1 {
		t.Errorf("leg B spread 가 넓을 때 결과도 더 넓어야: tight=%v wide=%v", s1, s2)
	}
}

// ─── Scale 동작 검증 ───────────────────────────────────────────────────────

func TestComputeCross_ScaleAppliedBothSides(t *testing.T) {
	f := CrossFormula{LegA: "EUR/USD", OpA: CrossOpMul, LegB: "USD/KRW", OpB: CrossOpMul, Scale: 1}
	a := CrossInput{Bid: 1.0, Ask: 1.0}
	b := CrossInput{Bid: 1.0, Ask: 1.0}

	r1, _ := ComputeCross(f, a, b)
	f.Scale = 100
	r100, _ := ComputeCross(f, a, b)
	if !near(r1.Bid, 1.0) || !near(r1.Ask, 1.0) {
		t.Errorf("scale=1 결과 = %+v", r1)
	}
	if !near(r100.Bid, 100.0) || !near(r100.Ask, 100.0) {
		t.Errorf("scale=100 결과 = %+v", r100)
	}
}

func TestComputeCross_ZeroScaleFallsBackTo1(t *testing.T) {
	f := CrossFormula{LegA: "EUR/USD", OpA: CrossOpMul, LegB: "USD/KRW", OpB: CrossOpMul, Scale: 0}
	a := CrossInput{Bid: 1.0795, Ask: 1.0805}
	b := CrossInput{Bid: 1378.65, Ask: 1378.69}
	got, err := ComputeCross(f, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !near(got.Bid, 1.0795*1378.65) {
		t.Errorf("scale=0 → fallback 1.0 안 됨: bid=%v", got.Bid)
	}
}

func TestComputeCross_NegativeScaleFallsBackTo1(t *testing.T) {
	f := CrossFormula{LegA: "EUR/USD", OpA: CrossOpMul, LegB: "USD/KRW", OpB: CrossOpMul, Scale: -5}
	a := CrossInput{Bid: 1, Ask: 1}
	b := CrossInput{Bid: 1, Ask: 1}
	got, _ := ComputeCross(f, a, b)
	if got.Bid != 1.0 {
		t.Errorf("음수 scale → fallback 1.0: bid=%v", got.Bid)
	}
}

// ─── Op 검증 (4 조합) ───────────────────────────────────────────────────────

// OpA=div, OpB=mul — 역수 leg 가 앞에.
// 예: USD/EUR = 1/EUR/USD (정의가 좀 어색하지만 산식 검증용).
func TestComputeCross_DivMul(t *testing.T) {
	f := CrossFormula{LegA: "EUR/USD", OpA: CrossOpDiv, LegB: "EUR/KRW", OpB: CrossOpMul, Scale: 1}
	eurUsd := CrossInput{Bid: 1.0795, Ask: 1.0805}
	eurKrw := CrossInput{Bid: 1488.30, Ask: 1489.50}
	got, err := ComputeCross(f, eurUsd, eurKrw)
	if err != nil {
		t.Fatal(err)
	}
	// bid = 1 × (1/eur_usd.ask) × eur_krw.bid = eur_krw.bid / eur_usd.ask
	// ask = 1 × (1/eur_usd.bid) × eur_krw.ask = eur_krw.ask / eur_usd.bid
	wantBid := eurKrw.Bid / eurUsd.Ask
	wantAsk := eurKrw.Ask / eurUsd.Bid
	if !near(got.Bid, wantBid) {
		t.Errorf("bid = %v, want %v", got.Bid, wantBid)
	}
	if !near(got.Ask, wantAsk) {
		t.Errorf("ask = %v, want %v", got.Ask, wantAsk)
	}
}

// OpA=div, OpB=div (드물지만 산식 검증).
func TestComputeCross_DivDiv(t *testing.T) {
	f := CrossFormula{LegA: "A/X", OpA: CrossOpDiv, LegB: "B/X", OpB: CrossOpDiv, Scale: 1}
	a := CrossInput{Bid: 2.0, Ask: 2.1}
	b := CrossInput{Bid: 3.0, Ask: 3.2}
	got, err := ComputeCross(f, a, b)
	if err != nil {
		t.Fatal(err)
	}
	// bid = (1/a.ask) × (1/b.ask) = 1/(2.1 × 3.2)
	// ask = (1/a.bid) × (1/b.bid) = 1/(2.0 × 3.0)
	wantBid := 1.0 / (2.1 * 3.2)
	wantAsk := 1.0 / (2.0 * 3.0)
	if !near(got.Bid, wantBid) || !near(got.Ask, wantAsk) {
		t.Errorf("div×div: got %+v, want bid=%v ask=%v", got, wantBid, wantAsk)
	}
}

// ─── 에러 케이스 ───────────────────────────────────────────────────────────

func TestComputeCross_InvalidOp(t *testing.T) {
	f := CrossFormula{LegA: "A", OpA: "bogus", LegB: "B", OpB: CrossOpMul, Scale: 1}
	_, err := ComputeCross(f, CrossInput{1, 1}, CrossInput{1, 1})
	if !errors.Is(err, ErrCrossInvalidOp) {
		t.Errorf("err = %v, want ErrCrossInvalidOp", err)
	}
}

func TestComputeCross_ZeroBid(t *testing.T) {
	f := CrossFormula{LegA: "A", OpA: CrossOpMul, LegB: "B", OpB: CrossOpMul, Scale: 1}
	_, err := ComputeCross(f, CrossInput{Bid: 0, Ask: 1.0}, CrossInput{1, 1})
	if !errors.Is(err, ErrCrossInvalidInput) {
		t.Errorf("err = %v, want ErrCrossInvalidInput", err)
	}
}

func TestComputeCross_NegativeBid(t *testing.T) {
	f := CrossFormula{LegA: "A", OpA: CrossOpMul, LegB: "B", OpB: CrossOpMul, Scale: 1}
	_, err := ComputeCross(f, CrossInput{Bid: -1, Ask: 1}, CrossInput{1, 1})
	if !errors.Is(err, ErrCrossInvalidInput) {
		t.Errorf("err = %v, want ErrCrossInvalidInput", err)
	}
}

// ─── 실 시나리오: USD/KRW direct + USD/JPY direct → 100JPY/KRW ─────────────

func TestComputeCross_RealisticUSDJPYKRW(t *testing.T) {
	// 현재 시장 근사 값 (2026년 가정).
	usdKrw := CrossInput{Bid: 1378.40, Ask: 1378.90}
	usdJpy := CrossInput{Bid: 151.20, Ask: 151.45}

	f := CrossFormula{LegA: "USD/KRW", OpA: CrossOpMul, LegB: "USD/JPY", OpB: CrossOpDiv, Scale: 100}
	got, err := ComputeCross(f, usdKrw, usdJpy)
	if err != nil {
		t.Fatal(err)
	}
	// 시장적 합리성 — 100JPY/KRW 는 ~910 근처 (1378/151 × 100).
	if got.Bid < 900 || got.Bid > 920 {
		t.Errorf("100JPY/KRW bid 시장 밖: %v", got.Bid)
	}
	if got.Ask < 900 || got.Ask > 920 {
		t.Errorf("100JPY/KRW ask 시장 밖: %v", got.Ask)
	}
	if got.Ask <= got.Bid {
		t.Errorf("ask 가 bid 이하: %v / %v (cross spread 음수)", got.Bid, got.Ask)
	}
}
