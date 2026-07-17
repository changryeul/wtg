package price

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/session"
)

func TestReceivedSwapStore_SetGet(t *testing.T) {
	s := NewReceivedSwapStore()
	if _, ok := s.Get("USD/KRW", "M01"); ok {
		t.Error("빈 store 인데 Get ok=true")
	}
	s.Set("USD/KRW", "M01", 2.50, 2.70)
	m, ok := s.Get("USD/KRW", "M01")
	if !ok || m.BidAmount != 2.50 || m.AskAmount != 2.70 {
		t.Errorf("Get=%+v ok=%v, want {2.50,2.70}", m, ok)
	}
	// 덮어쓰기 (최신 수신).
	s.Set("USD/KRW", "M01", 2.55, 2.75)
	m, _ = s.Get("USD/KRW", "M01")
	if m.BidAmount != 2.55 {
		t.Errorf("갱신 실패: %+v", m)
	}
}

func TestReceivedSwapStore_Snapshot(t *testing.T) {
	s := NewReceivedSwapStore()
	s.Set("USD/KRW", "M01", 2.5, 2.7)
	s.Set("USD/KRW", "M03", 7.1, 7.4)
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot %d개, want 2", len(snap))
	}
	if snap[pricing.SwapKey{Pair: "USD/KRW", Tenor: "M03"}].AskAmount != 7.4 {
		t.Errorf("snapshot M03=%+v", snap[pricing.SwapKey{Pair: "USD/KRW", Tenor: "M03"}])
	}
	// snapshot 은 복사본 — 원본 불변.
	snap[pricing.SwapKey{Pair: "USD/KRW", Tenor: "M01"}] = pricing.Margin{BidAmount: 999}
	m, _ := s.Get("USD/KRW", "M01")
	if m.BidAmount == 999 {
		t.Error("snapshot 이 원본을 공유함 (복사 아님)")
	}
}

// effective = 수신값(로이터) + 운영자 delta (delta/skew 모델).
func TestEffectiveSwap(t *testing.T) {
	received := pricing.Margin{BidAmount: 2.50, AskAmount: 2.70}
	delta := pricing.Margin{BidAmount: 0.05, AskAmount: -0.03} // 운영자 조정
	eff := EffectiveSwap(received, delta)
	if !near(eff.BidAmount, 2.55) {
		t.Errorf("effective bid=%v, want 2.55", eff.BidAmount)
	}
	if !near(eff.AskAmount, 2.67) {
		t.Errorf("effective ask=%v, want 2.67", eff.AskAmount)
	}
}

// 수신값 없으면 effective = delta 만 (기존 동작 하위호환 — Reuters 미연결 시).
func TestEffectiveSwap_NoReceived(t *testing.T) {
	s := NewReceivedSwapStore()
	received, _ := s.Get("USD/KRW", "M01") // zero Margin
	delta := pricing.Margin{BidAmount: 2.0, AskAmount: 2.2}
	eff := EffectiveSwap(received, delta)
	if eff.BidAmount != 2.0 || eff.AskAmount != 2.2 {
		t.Errorf("no-received effective=%+v, want delta 그대로", eff)
	}
	_ = session.Pair("USD/KRW")
}
