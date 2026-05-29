package price

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// ─── fake publisher with sink ─────────────────────────────────────────

type fakeSinkPublisher struct {
	mu          sync.Mutex
	subscribed  map[string]bool // profileKey → bool
	pubCalls    atomic.Uint64
	pubProfiles []string
}

func newFakeSinkPublisher(subscribed ...string) *fakeSinkPublisher {
	m := make(map[string]bool, len(subscribed))
	for _, p := range subscribed {
		m[p] = true
	}
	return &fakeSinkPublisher{subscribed: m}
}

func (p *fakeSinkPublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	p.pubCalls.Add(1)
	p.mu.Lock()
	p.pubProfiles = append(p.pubProfiles, profile.Key())
	p.mu.Unlock()
	return nil
}

func (p *fakeSinkPublisher) HasQuoteSubscribers(profileKey string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.subscribed[profileKey]
}

func (p *fakeSinkPublisher) SetSubscribed(profileKey string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscribed[profileKey] = on
}

// publisher 가 sink 인터페이스 만족 안 하는 경우 (옛 동작 backward compat 검증).
type fakeNoSinkPublisher struct {
	pubCalls atomic.Uint64
}

func (p *fakeNoSinkPublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	p.pubCalls.Add(1)
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────

func mkPCForPruning(t *testing.T, pub QuotePublisher, profs []session.Profile) *PricingConsumer {
	t.Helper()
	store := pricing.NewStore()
	store.Replace(&pricing.PricingTable{}) // 빈 table — Apply 가 raw 그대로 반환
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
	})
	return NewPricingConsumer(PricingConsumerOptions{
		Store:     store,
		Symbols:   syms,
		Decoder:   JSONCookerDecoder(),
		Publisher: pub,
		Profiles:  &StaticProfileSource{Profiles: profs},
		Tenor:     pricing.TenorSpot,
	})
}

func mkTickForPruning(t *testing.T) *Tick {
	t.Helper()
	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: "USDKRW", Bid: 1380.45, Ask: 1380.55, TS: time.Now(),
	})
	return &Tick{Symbol: "USDKRW", Body: body, Received: time.Now()}
}

// ─── Tests ──────────────────────────────────────────────────────────

func TestPricingConsumer_Phase3_SkipsUnsubscribedProfile(t *testing.T) {
	profs := []session.Profile{
		{Channel: "WEB", Site: "BRANCH", Tier: "VIP"},
		{Channel: "WEB", Site: "BRANCH", Tier: "GOLD"},
		{Channel: "WEB", Site: "BRANCH", Tier: "STD"},
	}
	// VIP 만 구독자 있음 — GOLD, STD 는 skip 되어야.
	pub := newFakeSinkPublisher("WEB.BRANCH.VIP")
	pc := mkPCForPruning(t, pub, profs)

	pc.OnTick(mkTickForPruning(t))

	if pub.pubCalls.Load() != 1 {
		t.Errorf("publish=%d, want 1 (VIP 만)", pub.pubCalls.Load())
	}
	if len(pub.pubProfiles) != 1 || pub.pubProfiles[0] != "WEB.BRANCH.VIP" {
		t.Errorf("published profiles=%v, want [WEB.BRANCH.VIP]", pub.pubProfiles)
	}
	if got := pc.Stats().ProfilesSkipped; got != 2 {
		t.Errorf("ProfilesSkipped=%d, want 2 (GOLD, STD)", got)
	}
}

func TestPricingConsumer_Phase3_AllSubscribed_NoSkip(t *testing.T) {
	profs := []session.Profile{
		{Channel: "WEB", Site: "BRANCH", Tier: "VIP"},
		{Channel: "WEB", Site: "BRANCH", Tier: "GOLD"},
	}
	pub := newFakeSinkPublisher("WEB.BRANCH.VIP", "WEB.BRANCH.GOLD")
	pc := mkPCForPruning(t, pub, profs)
	pc.OnTick(mkTickForPruning(t))

	if pub.pubCalls.Load() != 2 {
		t.Errorf("publish=%d, want 2", pub.pubCalls.Load())
	}
	if got := pc.Stats().ProfilesSkipped; got != 0 {
		t.Errorf("ProfilesSkipped=%d, want 0", got)
	}
}

func TestPricingConsumer_Phase3_NoSubscribers_AllSkipped(t *testing.T) {
	profs := []session.Profile{
		{Channel: "WEB", Site: "BRANCH", Tier: "VIP"},
		{Channel: "WEB", Site: "BRANCH", Tier: "GOLD"},
	}
	pub := newFakeSinkPublisher() // empty — 모두 skip
	pc := mkPCForPruning(t, pub, profs)
	pc.OnTick(mkTickForPruning(t))

	if pub.pubCalls.Load() != 0 {
		t.Errorf("publish=%d, want 0 (구독자 없음)", pub.pubCalls.Load())
	}
	if got := pc.Stats().ProfilesSkipped; got != 2 {
		t.Errorf("ProfilesSkipped=%d, want 2", got)
	}
}

// publisher 가 QuoteSubscriberSink 만족 안 하면 모든 Profile 처리 (backward compat).
func TestPricingConsumer_Phase3_NoSink_BackwardCompat(t *testing.T) {
	profs := []session.Profile{
		{Channel: "WEB", Site: "BRANCH", Tier: "VIP"},
		{Channel: "WEB", Site: "BRANCH", Tier: "GOLD"},
		{Channel: "WEB", Site: "BRANCH", Tier: "STD"},
	}
	pub := &fakeNoSinkPublisher{}
	pc := mkPCForPruning(t, pub, profs)
	pc.OnTick(mkTickForPruning(t))

	if pub.pubCalls.Load() != 3 {
		t.Errorf("sink 없는 publisher: publish=%d, want 3 (모든 Profile)", pub.pubCalls.Load())
	}
	if got := pc.Stats().ProfilesSkipped; got != 0 {
		t.Errorf("sink 없는데 ProfilesSkipped=%d", got)
	}
}

// 구독자가 hot-on/off 시 즉시 반영.
func TestPricingConsumer_Phase3_DynamicSubscriptionChange(t *testing.T) {
	profs := []session.Profile{
		{Channel: "WEB", Site: "BRANCH", Tier: "VIP"},
	}
	pub := newFakeSinkPublisher() // 처음엔 구독자 없음
	pc := mkPCForPruning(t, pub, profs)

	pc.OnTick(mkTickForPruning(t))
	if pub.pubCalls.Load() != 0 {
		t.Errorf("초기 publish=%d, want 0", pub.pubCalls.Load())
	}

	// 구독자 등록 — 다음 tick 에서 publish.
	pub.SetSubscribed("WEB.BRANCH.VIP", true)
	pc.OnTick(mkTickForPruning(t))
	if pub.pubCalls.Load() != 1 {
		t.Errorf("구독자 추가 후 publish=%d, want 1", pub.pubCalls.Load())
	}

	// 구독자 해지 — 다시 skip.
	pub.SetSubscribed("WEB.BRANCH.VIP", false)
	pc.OnTick(mkTickForPruning(t))
	if pub.pubCalls.Load() != 1 {
		t.Errorf("구독자 해지 후 publish 누적=%d, want 1 (해지 후 skip)", pub.pubCalls.Load())
	}
}

// ─── GRPCServer.HasQuoteSubscribers ───────────────────────────────

func TestGRPCServer_HasQuoteSubscribers_NoSubscribers(t *testing.T) {
	g := NewGRPCServer(nil, 16)
	if g.HasQuoteSubscribers("WEB.BRANCH.VIP") {
		t.Errorf("subscriber 0 인데 true 반환")
	}
}

// 구독자가 특정 profile filter 를 명시 — 그 profile 만 true.
func TestGRPCServer_HasQuoteSubscribers_FilteredMatch(t *testing.T) {
	g := NewGRPCServer(nil, 16)
	id := g.nextSubID.Add(1)
	g.qmu.Lock()
	g.quoteSubscribers[id] = &quoteSubscriber{
		id:       id,
		profiles: map[string]struct{}{"WEB.BRANCH.VIP": {}},
	}
	g.qmu.Unlock()

	if !g.HasQuoteSubscribers("WEB.BRANCH.VIP") {
		t.Errorf("필터 매칭인데 false")
	}
	if g.HasQuoteSubscribers("WEB.BRANCH.STD") {
		t.Errorf("필터 미매칭인데 true")
	}
}

// 구독자가 profile filter 비어있음 (= 모든 profile 받음).
func TestGRPCServer_HasQuoteSubscribers_WildcardSubscriber(t *testing.T) {
	g := NewGRPCServer(nil, 16)
	id := g.nextSubID.Add(1)
	g.qmu.Lock()
	g.quoteSubscribers[id] = &quoteSubscriber{
		id:       id,
		profiles: map[string]struct{}{}, // 빈 set = 무필터
	}
	g.qmu.Unlock()

	if !g.HasQuoteSubscribers("WEB.BRANCH.VIP") {
		t.Errorf("wildcard sub 인데 VIP 가 false")
	}
	if !g.HasQuoteSubscribers("ANY.PROFILE.KEY") {
		t.Errorf("wildcard sub 인데 임의 profile 이 false")
	}
}
