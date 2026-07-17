package price

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/pricing"
)

func TestSwapStore_SetGet(t *testing.T) {
	s := NewSwapStore()
	if _, ok := s.Received("USD/KRW", "M01"); ok {
		t.Error("빈 store 인데 Received ok=true")
	}
	s.SetReceived("USD/KRW", "M01", 2.50, 2.70)
	m, ok := s.Received("USD/KRW", "M01")
	if !ok || m.BidAmount != 2.50 || m.AskAmount != 2.70 {
		t.Errorf("Received=%+v ok=%v, want {2.50,2.70}", m, ok)
	}
	s.SetReceived("USD/KRW", "M01", 2.55, 2.75) // 최신 갱신
	m, _ = s.Received("USD/KRW", "M01")
	if m.BidAmount != 2.55 {
		t.Errorf("갱신 실패: %+v", m)
	}
}

// effective = received + delta (둘 다 add 규약).
func TestSwapStore_Effective(t *testing.T) {
	s := NewSwapStore()
	s.SetReceived("USD/KRW", "M01", 2.50, 2.70)
	s.SetDelta("USD/KRW", "M01", 0.05, -0.03) // 운영자 조정
	eff, ok := s.Effective("USD/KRW", "M01")
	if !ok {
		t.Fatal("Effective ok=false")
	}
	if !near(eff.BidAmount, 2.55) || !near(eff.AskAmount, 2.67) {
		t.Errorf("effective=%+v, want {2.55,2.67}", eff)
	}
}

// received 없이 delta 만 있어도 effective = delta (하위호환).
func TestSwapStore_DeltaOnly(t *testing.T) {
	s := NewSwapStore()
	s.SetDelta("USD/KRW", "M01", 2.0, 2.2)
	eff, ok := s.Effective("USD/KRW", "M01")
	if !ok || !near(eff.BidAmount, 2.0) || !near(eff.AskAmount, 2.2) {
		t.Errorf("delta-only effective=%+v ok=%v", eff, ok)
	}
}

// Tenors = received ∪ delta forward tenor (spot 제외).
func TestSwapStore_Tenors(t *testing.T) {
	s := NewSwapStore()
	s.SetReceived("USD/KRW", "M01", 2.5, 2.7)
	s.SetDelta("USD/KRW", "M03", 7, 7.4)
	s.SetReceived("USD/KRW", pricing.TenorSpot, 0, 0) // spot — 제외돼야
	got := s.Tenors("USD/KRW")
	if len(got) != 2 {
		t.Fatalf("Tenors=%v, want 2 (M01,M03; spot 제외)", got)
	}
}

// ViewSnapshot — received+delta+effective 병합.
func TestSwapStore_ViewSnapshot(t *testing.T) {
	s := NewSwapStore()
	s.SetReceived("USD/KRW", "M01", 2.5, 2.7)
	s.SetDelta("USD/KRW", "M01", 0.05, -0.03)
	views := s.ViewSnapshot()
	if len(views) != 1 {
		t.Fatalf("views %d, want 1", len(views))
	}
	v := views[0]
	if !near(v.RecvBid, 2.5) || !near(v.DeltaBid, 0.05) || !near(v.EffBid, 2.55) {
		t.Errorf("view=%+v", v)
	}
}
