package price

import (
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Phase 4a — PricingConsumer 의 customer fan-out 경로 검증.

// fakeCustomerPublisher 는 customer-tag 된 publish 호출 캡처.
type fakeCustomerPublisher struct {
	mu    sync.Mutex
	calls []customerPublishCall
	err   error
}

type customerPublishCall struct {
	CustomerID string
	Profile    session.Profile
	CQ         pricing.CustomerQuote
}

func (f *fakeCustomerPublisher) PublishCustomerQuote(customerID string, profile session.Profile, cq pricing.CustomerQuote) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, customerPublishCall{customerID, profile, cq})
	return nil
}

func (f *fakeCustomerPublisher) snapshot() []customerPublishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]customerPublishCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newCustomerFanoutConsumer — Profile-only publisher + customer registry/publisher
// 모두 주입한 PricingConsumer. PricingTable 에 customer rule 까지 등록.
func newCustomerFanoutConsumer(t *testing.T, prof QuotePublisher, custPub CustomerQuotePublisher, reg *CustomerRegistry, profiles []session.Profile) *PricingConsumer {
	t.Helper()

	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
	})

	// HQ 0.02 + Site 0.05 = 0.07 (VIP / WEB / BRANCH).
	// customer VIP-7: add -0.01 → 0.06
	// customer GOLD-3: override 0.005 → 0.005 (HQ/Site 무시)
	tbl := &pricing.PricingTable{
		Version: 8,
		HQMargin: map[pricing.HQKey]pricing.Margin{
			{Pair: "USD/KRW", Tier: session.TierVIP}: {BidAmount: 0.02, AskAmount: 0.02},
		},
		SiteMargin: map[pricing.SiteKey]pricing.Margin{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch}: {BidAmount: 0.05, AskAmount: 0.05},
		},
		CustomerMargin: []pricing.CustomerRule{
			{CustomerID: "VIP-7", Pair: "USD/KRW", BidDelta: -0.01, AskDelta: -0.01, Mode: "add"},
			{CustomerID: "GOLD-3", Pair: "USD/KRW", BidDelta: 0.005, AskDelta: 0.005, Mode: "override"},
		},
	}
	store := pricing.NewStore()
	store.Replace(tbl)

	return NewPricingConsumer(PricingConsumerOptions{
		Store:            store,
		Symbols:          syms,
		Decoder:          JSONCookerDecoder(),
		Publisher:        prof,
		Profiles:         &StaticProfileSource{Profiles: profiles},
		CustomerRegistry: reg,
		CustomerPub:      custPub,
	})
}

// 등록된 customer 가 0 이면 customer publish 0건. Profile publish 는 정상.
func TestPricingConsumer_CustomerFanout_EmptyRegistry(t *testing.T) {
	profPub := &fakeQuotePublisher{}
	custPub := &fakeCustomerPublisher{}
	reg := NewCustomerRegistry()
	pc := newCustomerFanoutConsumer(t, profPub, custPub, reg,
		[]session.Profile{{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}})

	pc.OnTick(mkEnvTick("USDKRW", 1300.00, 1300.10, time.Now()))

	if len(profPub.snapshot()) != 1 {
		t.Errorf("profile publish: got %d, want 1", len(profPub.snapshot()))
	}
	if len(custPub.snapshot()) != 0 {
		t.Errorf("customer publish (empty registry): got %d, want 0", len(custPub.snapshot()))
	}
	if got := pc.Stats().CustomersRegistered; got != 0 {
		t.Errorf("CustomersRegistered = %d, want 0", got)
	}
}

// 등록된 customer 별로 ApplyForCustomer 호출 + publish.
func TestPricingConsumer_CustomerFanout_AppliesPerCustomer(t *testing.T) {
	profPub := &fakeQuotePublisher{}
	custPub := &fakeCustomerPublisher{}
	reg := NewCustomerRegistry()
	prof := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}
	reg.Register("VIP-7", prof)
	reg.Register("GOLD-3", prof)

	pc := newCustomerFanoutConsumer(t, profPub, custPub, reg, []session.Profile{prof})

	pc.OnTick(mkEnvTick("USDKRW", 1300.00, 1300.10, time.Now()))

	// Profile publish 1건 (VIP).
	if len(profPub.snapshot()) != 1 {
		t.Errorf("profile publish: got %d, want 1", len(profPub.snapshot()))
	}

	// Customer publish 2건 (VIP-7, GOLD-3).
	calls := custPub.snapshot()
	if len(calls) != 2 {
		t.Fatalf("customer publish: got %d, want 2", len(calls))
	}

	byID := map[string]customerPublishCall{}
	for _, c := range calls {
		byID[c.CustomerID] = c
	}
	// VIP-7 add: HQ 0.02 + Site 0.05 + (-0.01) = 0.06
	v7 := byID["VIP-7"]
	if !floatNear(1300.00-v7.CQ.Bid, 0.06) {
		t.Errorf("VIP-7 add: bid 차이 = %v, want 0.06", 1300.00-v7.CQ.Bid)
	}
	// GOLD-3 override: 0.005 단독.
	g3 := byID["GOLD-3"]
	if !floatNear(1300.00-g3.CQ.Bid, 0.005) {
		t.Errorf("GOLD-3 override: bid 차이 = %v, want 0.005", 1300.00-g3.CQ.Bid)
	}

	// Stats.
	st := pc.Stats()
	if st.CustomersRegistered != 2 || st.CustomerQuotesPublished != 2 {
		t.Errorf("stats: %+v", st)
	}
}

// Customer publisher 실패는 다른 customer 처리 막지 않음.
func TestPricingConsumer_CustomerFanout_OneFailureDoesntBlock(t *testing.T) {
	profPub := &fakeQuotePublisher{}
	// 첫 호출만 실패하도록 — 단순화 위해 매 호출 실패 → publish 0건/error 2건.
	custPub := &fakeCustomerPublisher{err: errBoom()}
	reg := NewCustomerRegistry()
	prof := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}
	reg.Register("A", prof)
	reg.Register("B", prof)

	pc := newCustomerFanoutConsumer(t, profPub, custPub, reg, []session.Profile{prof})
	pc.OnTick(mkEnvTick("USDKRW", 1300.00, 1300.10, time.Now()))

	st := pc.Stats()
	if st.CustomerQuotesPublished != 0 {
		t.Errorf("on error: published = %d, want 0", st.CustomerQuotesPublished)
	}
	if st.CustomerPublishErrors != 2 {
		t.Errorf("on error: errors = %d, want 2 (둘 다 시도)", st.CustomerPublishErrors)
	}
}

// CustomerRegistry / CustomerPub 둘 다 nil 이면 customer 경로 미생성 — 기존 동작.
func TestPricingConsumer_NoCustomerFanout_BackwardCompat(t *testing.T) {
	profPub := &fakeQuotePublisher{}
	prof := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}
	pc := newTestPricingConsumer(t, profPub, []session.Profile{prof})

	pc.OnTick(mkEnvTick("USDKRW", 1300.00, 1300.10, time.Now()))

	st := pc.Stats()
	if st.CustomerQuotesPublished != 0 || st.CustomerPublishErrors != 0 {
		t.Errorf("backward compat: customer stats non-zero: %+v", st)
	}
	if st.CustomersRegistered != 0 {
		t.Errorf("CustomersRegistered = %d, want 0 (no registry)", st.CustomersRegistered)
	}
}

func errBoom() error {
	return errBoomErr{}
}

type errBoomErr struct{}

func (errBoomErr) Error() string { return "boom" }
