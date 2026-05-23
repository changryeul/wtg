package pricing

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// 기본 산식: bid 차감 / ask 가산.
func TestApply_BasicMargin(t *testing.T) {
	tbl := &PricingTable{
		Version: 1,
		HQMargin: map[HQKey]Margin{
			{Pair: "USD/KRW", Tier: session.TierStandard}: {BidAmount: 0.10, AskAmount: 0.10},
		},
		SiteMargin: map[SiteKey]Margin{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch}: {BidAmount: 0.05, AskAmount: 0.05},
		},
	}

	raw := Quote{Pair: "USD/KRW", Bid: 1399.50, Ask: 1399.60, TS: time.Unix(0, 0)}
	prof := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard}

	got := tbl.Apply(raw, prof, TenorSpot)

	wantBid := 1399.50 - 0.15
	wantAsk := 1399.60 + 0.15
	if got.Bid != wantBid {
		t.Errorf("bid = %v, want %v", got.Bid, wantBid)
	}
	if got.Ask != wantAsk {
		t.Errorf("ask = %v, want %v", got.Ask, wantAsk)
	}
	if got.TableVersion != 1 {
		t.Errorf("version = %d, want 1", got.TableVersion)
	}
	if got.RawBid != 1399.50 || got.RawAsk != 1399.60 {
		t.Errorf("raw quote not preserved: bid=%v ask=%v", got.RawBid, got.RawAsk)
	}
	if got.Profile != prof {
		t.Errorf("profile mismatch: %+v", got.Profile)
	}
}

// 스왑포인트는 만기에 따라 음수도 허용.
func TestApply_SwapPoint_Signed(t *testing.T) {
	tbl := &PricingTable{
		SwapPoint: map[SwapKey]Margin{
			{Pair: "USD/KRW", Tenor: Tenor1M}: {BidAmount: -0.20, AskAmount: 0.30},
		},
	}
	raw := Quote{Pair: "USD/KRW", Bid: 1000, Ask: 1000}
	got := tbl.Apply(raw, session.Profile{}, Tenor1M)

	// bid: 1000 - (-0.20) = 1000.20
	// ask: 1000 + 0.30 = 1000.30
	if got.Bid != 1000.20 {
		t.Errorf("bid = %v, want 1000.20", got.Bid)
	}
	if got.Ask != 1000.30 {
		t.Errorf("ask = %v, want 1000.30", got.Ask)
	}
}

// HQ 마진 fallback: tier 미등록 → tier="" 와일드카드 → zero.
func TestLookupHQ_Fallback(t *testing.T) {
	tbl := &PricingTable{
		HQMargin: map[HQKey]Margin{
			{Pair: "USD/KRW", Tier: session.TierVIP}: {BidAmount: 0.01, AskAmount: 0.01},
			{Pair: "USD/KRW", Tier: ""}:              {BidAmount: 0.10, AskAmount: 0.10}, // 와일드카드
		},
	}
	// 정확매치
	if m := tbl.lookupHQ("USD/KRW", session.TierVIP); m.BidAmount != 0.01 {
		t.Errorf("VIP exact: bid = %v", m.BidAmount)
	}
	// 와일드카드 fallback
	if m := tbl.lookupHQ("USD/KRW", session.TierStandard); m.BidAmount != 0.10 {
		t.Errorf("STD fallback: bid = %v", m.BidAmount)
	}
	// 미등록 pair → zero
	if m := tbl.lookupHQ("EUR/KRW", session.TierVIP); m.BidAmount != 0 {
		t.Errorf("EUR/KRW miss: bid = %v, want 0", m.BidAmount)
	}
}

// Site 마진 fallback 우선순위: 정확매치 > channel="" > site="" > zero.
func TestLookupSite_FallbackOrder(t *testing.T) {
	tbl := &PricingTable{
		SiteMargin: map[SiteKey]Margin{
			{Pair: "USD/KRW", Channel: "", Site: session.SiteBranch}:                  {BidAmount: 0.20}, // site-only
			{Pair: "USD/KRW", Channel: session.ChannelMobile, Site: ""}:               {BidAmount: 0.30}, // channel-only
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch}:  {BidAmount: 0.10}, // exact
		},
	}
	// exact
	if m := tbl.lookupSite("USD/KRW", session.ChannelWeb, session.SiteBranch); m.BidAmount != 0.10 {
		t.Errorf("exact: bid = %v", m.BidAmount)
	}
	// channel 미매치 → site-only fallback (site 가 channel 보다 우선)
	if m := tbl.lookupSite("USD/KRW", session.ChannelCS, session.SiteBranch); m.BidAmount != 0.20 {
		t.Errorf("site-only fallback: bid = %v", m.BidAmount)
	}
	// site 미매치 → channel-only fallback
	if m := tbl.lookupSite("USD/KRW", session.ChannelMobile, session.SiteHQ); m.BidAmount != 0.30 {
		t.Errorf("channel-only fallback: bid = %v", m.BidAmount)
	}
	// 모두 미매치 → zero
	if m := tbl.lookupSite("EUR/KRW", session.ChannelFIX, session.SiteHQ); m.BidAmount != 0 {
		t.Errorf("miss: bid = %v, want 0", m.BidAmount)
	}
}

// Store 는 lock-free read + atomic Replace 를 보장.
func TestStore_AtomicReplace(t *testing.T) {
	s := NewStore()
	if s.Load() == nil {
		t.Fatal("initial Load returned nil")
	}
	v1 := &PricingTable{Version: 1}
	v2 := &PricingTable{Version: 2}
	s.Replace(v1)
	if s.Load().Version != 1 {
		t.Errorf("v1 not stored")
	}
	s.Replace(v2)
	if s.Load().Version != 2 {
		t.Errorf("v2 not stored")
	}
}

// 동시 read/write 에 race 가 나지 않는지 확인 (go test -race 와 함께).
func TestStore_ConcurrentReadWrite(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	var stop atomic.Bool

	// reader 들
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = s.Load().Version
			}
		}()
	}
	// writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := int64(0); v < 1000; v++ {
			s.Replace(&PricingTable{Version: v})
		}
		stop.Store(true)
	}()

	wg.Wait()
}
