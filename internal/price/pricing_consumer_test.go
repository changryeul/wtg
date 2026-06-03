package price

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
)

// floatNear 는 부동소수점 근사 비교 (1e-9 tolerance).
func floatNear(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// fakeQuotePublisher 는 publish 호출을 캡처.
type fakeQuotePublisher struct {
	mu    sync.Mutex
	calls []publishCall
	err   error
}

type publishCall struct {
	Profile session.Profile
	CQ      pricing.CustomerQuote
}

func (f *fakeQuotePublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, publishCall{profile, cq})
	return nil
}

func (f *fakeQuotePublisher) snapshot() []publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func newTestPricingConsumer(t *testing.T, pub QuotePublisher, profiles []session.Profile) *PricingConsumer {
	t.Helper()

	// SymbolMap — USDKRW active, JPYKRW inactive.
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
		{Symbol: "JPYKRW", Pair: "JPY/KRW", Active: false},
	})

	// PricingTable — 정확매치 entry 일부.
	tbl := &pricing.PricingTable{
		Version: 7,
		HQMargin: map[pricing.HQKey]pricing.Margin{
			{Pair: "USD/KRW", Tier: session.TierStandard}: {BidAmount: 0.10, AskAmount: 0.10},
			{Pair: "USD/KRW", Tier: session.TierVIP}:      {BidAmount: 0.02, AskAmount: 0.02},
		},
		SiteMargin: map[pricing.SiteKey]pricing.Margin{
			{Pair: "USD/KRW", Channel: session.ChannelWeb, Site: session.SiteBranch}: {BidAmount: 0.05, AskAmount: 0.05},
		},
	}
	store := pricing.NewStore()
	store.Replace(tbl)

	return NewPricingConsumer(PricingConsumerOptions{
		Store:     store,
		Symbols:   syms,
		Decoder:   JSONCookerDecoder(),
		Publisher: pub,
		Profiles:  &StaticProfileSource{Profiles: profiles},
	})
}

func mkEnvTick(sym string, bid, ask float64, ts time.Time) *Tick {
	body, _ := quote.EncodeJSONEnvelope(quote.JSONEnvelope{
		Sym: sym, Bid: bid, Ask: ask, TS: ts,
	})
	return &Tick{Symbol: sym, Body: body, Received: ts}
}

func TestPricingConsumer_PublishPerProfile(t *testing.T) {
	pub := &fakeQuotePublisher{}
	profiles := []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard},
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
		{Channel: session.ChannelMobile, Site: session.SiteHQ, Tier: session.TierStandard},
	}
	pc := newTestPricingConsumer(t, pub, profiles)

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	pc.OnTick(mkEnvTick("USDKRW", 1399.50, 1399.60, t0))

	calls := pub.snapshot()
	if len(calls) != len(profiles) {
		t.Fatalf("publish 수 = %d, want %d", len(calls), len(profiles))
	}

	// 첫 profile (WEB/BRANCH/STD): HQ 0.10 + site 0.05 = 0.15 차감/가산.
	std := findCall(calls, "WEB.BRANCH.STD")
	if std == nil {
		t.Fatal("WEB.BRANCH.STD 호출 누락")
	}
	if !floatNear(std.CQ.Bid, 1399.50-0.15) || !floatNear(std.CQ.Ask, 1399.60+0.15) {
		t.Errorf("STD: bid=%v ask=%v want=%v/%v", std.CQ.Bid, std.CQ.Ask, 1399.35, 1399.75)
	}

	// VIP: HQ 0.02 + site 0.05 = 0.07.
	vip := findCall(calls, "WEB.BRANCH.VIP")
	if vip == nil {
		t.Fatal("WEB.BRANCH.VIP 호출 누락")
	}
	if !floatNear(vip.CQ.Bid, 1399.50-0.07) || !floatNear(vip.CQ.Ask, 1399.60+0.07) {
		t.Errorf("VIP: bid=%v ask=%v", vip.CQ.Bid, vip.CQ.Ask)
	}

	// MOB/HQ/STD: HQ 0.10 (정확매치) + site margin 매치 없음 = 0.10 만.
	mob := findCall(calls, "MOB.HQ.STD")
	if mob == nil {
		t.Fatal("MOB.HQ.STD 호출 누락")
	}
	if !floatNear(mob.CQ.Bid, 1399.50-0.10) || !floatNear(mob.CQ.Ask, 1399.60+0.10) {
		t.Errorf("MOB STD: bid=%v ask=%v", mob.CQ.Bid, mob.CQ.Ask)
	}

	// TableVersion 모든 publish 에 동일.
	for _, c := range calls {
		if c.CQ.TableVersion != 7 {
			t.Errorf("TableVersion = %d, want 7 (%s)", c.CQ.TableVersion, c.Profile.Key())
		}
	}

	stats := pc.Stats()
	if stats.QuotesPublished != uint64(len(profiles)) {
		t.Errorf("QuotesPublished = %d", stats.QuotesPublished)
	}
	if stats.TicksIn != 1 {
		t.Errorf("TicksIn = %d", stats.TicksIn)
	}
}

func TestPricingConsumer_DropUnknownSymbol(t *testing.T) {
	pub := &fakeQuotePublisher{}
	pc := newTestPricingConsumer(t, pub, []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
	})

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	pc.OnTick(mkEnvTick("XAUUSD", 2000, 2001, t0)) // 미등록
	pc.OnTick(mkEnvTick("JPYKRW", 9, 10, t0))      // inactive

	if len(pub.snapshot()) != 0 {
		t.Error("미등록/inactive 가 publish 됨")
	}
	if pc.Stats().TicksDropped != 2 {
		t.Errorf("TicksDropped = %d, want 2", pc.Stats().TicksDropped)
	}
}

func TestPricingConsumer_DropBadEnvelope(t *testing.T) {
	pub := &fakeQuotePublisher{}
	pc := newTestPricingConsumer(t, pub, []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
	})

	// 정상 envelope 가 아님 (ask < bid).
	body, _ := quote.EncodeJSONEnvelope(quote.JSONEnvelope{
		Sym: "USDKRW", Bid: 1400, Ask: 1399, TS: time.Now(),
	})
	pc.OnTick(&Tick{Symbol: "USDKRW", Body: body, Received: time.Now()})

	if len(pub.snapshot()) != 0 {
		t.Error("손상 envelope 가 publish 됨")
	}
}

func TestPricingConsumer_PublishErrorContinues(t *testing.T) {
	pub := &fakeQuotePublisher{err: errors.New("broker down")}
	profiles := []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard},
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
	}
	pc := newTestPricingConsumer(t, pub, profiles)

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	pc.OnTick(mkEnvTick("USDKRW", 1399.50, 1399.60, t0))

	stats := pc.Stats()
	if stats.PublishErrors != uint64(len(profiles)) {
		t.Errorf("PublishErrors = %d, want %d", stats.PublishErrors, len(profiles))
	}
	if stats.QuotesPublished != 0 {
		t.Errorf("QuotesPublished = %d (모두 실패해야 함)", stats.QuotesPublished)
	}
}

func TestPricingConsumer_NoActiveProfiles(t *testing.T) {
	pub := &fakeQuotePublisher{}
	pc := newTestPricingConsumer(t, pub, nil)

	t0 := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	pc.OnTick(mkEnvTick("USDKRW", 1399.50, 1399.60, t0))

	if len(pub.snapshot()) != 0 {
		t.Error("active profile 0 인데 publish 발생")
	}
	if pc.Stats().TicksIn != 1 {
		t.Error("tick 카운트 누락")
	}
}

// MymqQuotePublisher: FrameInput 의 Func/Xchg/Rkey/Body 가 컨벤션과 일치하는지.
func TestMymqQuotePublisher_FrameShape(t *testing.T) {
	var captured *mymq.FrameInput
	sender := senderFunc(func(in *mymq.FrameInput) error {
		captured = in
		return nil
	})
	pub := NewMymqQuotePublisher(sender)

	cq := pricing.CustomerQuote{
		Pair:         "USD/KRW",
		Profile:      session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
		Tenor:        pricing.TenorSpot,
		Bid:          1399.43,
		Ask:          1399.67,
		TS:           time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		RawBid:       1399.50,
		RawAsk:       1399.60,
		TableVersion: 42,
	}
	if err := pub.PublishQuote(cq.Profile, cq); err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("Send 호출 안 됨")
	}
	if captured.Func != mymq.FCCast {
		t.Errorf("Func = %v, want FCCast", captured.Func)
	}
	if captured.Xchg != mymq.ExchangeQuote {
		t.Errorf("Xchg = %q, want %q", captured.Xchg, mymq.ExchangeQuote)
	}
	if captured.Rkey != "WEB.BRANCH.VIP" {
		t.Errorf("Rkey = %q, want WEB.BRANCH.VIP", captured.Rkey)
	}
	if len(captured.Rkey) > mymq.LRkey {
		t.Errorf("Rkey %d bytes > LRkey(%d)", len(captured.Rkey), mymq.LRkey)
	}

	// Body 검증.
	var dto customerQuoteDTO
	if err := json.Unmarshal(captured.Body, &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Pair != "USD/KRW" || dto.Channel != "WEB" || dto.Site != "BRANCH" || dto.Tier != "VIP" {
		t.Errorf("DTO mismatch: %+v", dto)
	}
	if dto.Bid != 1399.43 || dto.Ask != 1399.67 {
		t.Errorf("bid/ask: %v / %v", dto.Bid, dto.Ask)
	}
	if dto.Version != 42 {
		t.Errorf("Version = %d", dto.Version)
	}
}

// helper — *mymq.FrameInput 콜백 만 가진 mock sender.
type senderFunc func(*mymq.FrameInput) error

func (f senderFunc) Send(in *mymq.FrameInput) error { return f(in) }

func findCall(calls []publishCall, profileKey string) *publishCall {
	for i := range calls {
		if calls[i].Profile.Key() == profileKey {
			return &calls[i]
		}
	}
	return nil
}

func TestPricingConsumer_AttachQuoteID(t *testing.T) {
	pub := &fakeQuotePublisher{}
	profiles := []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierStandard},
		{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierVIP},
	}

	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}})
	tbl := &pricing.PricingTable{
		Version: 7,
		HQMargin: map[pricing.HQKey]pricing.Margin{
			{Pair: "USD/KRW", Tier: session.TierStandard}: {BidAmount: 0.10, AskAmount: 0.10},
			{Pair: "USD/KRW", Tier: session.TierVIP}:      {BidAmount: 0.02, AskAmount: 0.02},
		},
	}
	store := pricing.NewStore()
	store.Replace(tbl)

	reg := quoteid.NewMemoryRegistry(0)
	gen := quoteid.NewGenerator("A")

	pc := NewPricingConsumer(PricingConsumerOptions{
		Store:           store,
		Symbols:         syms,
		Decoder:         JSONCookerDecoder(),
		Publisher:       pub,
		Profiles:        &StaticProfileSource{Profiles: profiles},
		QuoteIDGen:      gen,
		QuoteIDRegistry: reg,
		QuoteValidity:   30 * time.Second, // 테스트 실행 시간 안에 만료되지 않도록 여유.
	})

	// tick TS 는 현재 시각 — registry 의 lazy expiry 가 real time.Now 와 비교하므로
	// 과거 TS 를 쓰면 ValidUntil 이 이미 지나 lookup 실패.
	ts := time.Now()
	pc.OnTick(mkEnvTick("USDKRW", 1400.0, 1400.05, ts))

	calls := pub.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls=%d, want 2", len(calls))
	}

	// 각 호출이 unique QuoteID 를 가졌는지.
	seen := map[string]struct{}{}
	for _, c := range calls {
		if c.CQ.QuoteID == "" {
			t.Errorf("Profile %s: QuoteID 미부착", c.Profile.Key())
			continue
		}
		if _, dup := seen[c.CQ.QuoteID]; dup {
			t.Errorf("QuoteID 중복: %s", c.CQ.QuoteID)
		}
		seen[c.CQ.QuoteID] = struct{}{}

		// ValidUntil = TS + Validity.
		want := c.CQ.TS.Add(30 * time.Second)
		if !c.CQ.ValidUntil.Equal(want) {
			t.Errorf("ValidUntil mismatch: got %v, want %v", c.CQ.ValidUntil, want)
		}

		// Registry 에 등록되어 있고 lookup 가능.
		rec, err := reg.Get(context.Background(), quoteid.QuoteID(c.CQ.QuoteID))
		if err != nil {
			t.Errorf("Registry.Get(%s): %v", c.CQ.QuoteID, err)
			continue
		}
		if rec.Profile.Key() != c.Profile.Key() {
			t.Errorf("Registry Profile mismatch: got %s, want %s", rec.Profile.Key(), c.Profile.Key())
		}
		if rec.Bid != c.CQ.Bid || rec.Ask != c.CQ.Ask {
			t.Errorf("Registry bid/ask mismatch: %v/%v vs %v/%v", rec.Bid, rec.Ask, c.CQ.Bid, c.CQ.Ask)
		}
		if rec.Issuer != "A" {
			t.Errorf("Issuer = %q, want A", rec.Issuer)
		}
	}
	if pc.Stats().QuoteRegErrors != 0 {
		t.Errorf("Registry 에러 발생: %d", pc.Stats().QuoteRegErrors)
	}
}

func TestPricingConsumer_NoQuoteIDWhenDisabled(t *testing.T) {
	pub := &fakeQuotePublisher{}
	profiles := []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierStandard},
	}
	pc := newTestPricingConsumer(t, pub, profiles) // QuoteIDGen 미주입.

	pc.OnTick(mkEnvTick("USDKRW", 1400.0, 1400.05, time.Unix(1700000000, 0)))

	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	if calls[0].CQ.QuoteID != "" {
		t.Errorf("Gen 비활성인데 QuoteID 발급됨: %q", calls[0].CQ.QuoteID)
	}
	if !calls[0].CQ.ValidUntil.IsZero() {
		t.Errorf("ValidUntil 미설정 기대, got %v", calls[0].CQ.ValidUntil)
	}
}

// ─── TickBufferSize: 비동기 buffer + backpressure ──────────────────────────────

// 비동기 buffer 활성 — channel 로 enqueue 후 worker 가 처리.
// publish 호출이 비동기 worker 에서 발생하는지 + Stats 의 buffer 카운터.
func TestPricingConsumer_BufferedAsyncPublish(t *testing.T) {
	pub := &fakeQuotePublisher{}
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}})
	tbl := &pricing.PricingTable{Version: 1, HQMargin: map[pricing.HQKey]pricing.Margin{
		{Pair: "USD/KRW", Tier: session.TierStandard}: {BidAmount: 0.10, AskAmount: 0.10},
	}}
	store := pricing.NewStore()
	store.Replace(tbl)

	pc := NewPricingConsumer(PricingConsumerOptions{
		Store:     store,
		Symbols:   syms,
		Decoder:   JSONCookerDecoder(),
		Publisher: pub,
		Profiles: &StaticProfileSource{Profiles: []session.Profile{
			{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard},
		}},
		TickBufferSize: 64,
	})

	pc.OnTick(mkEnvTick("USDKRW", 1378.40, 1378.45, time.Now()))

	// worker 가 처리할 시간.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(pub.snapshot()) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(pub.snapshot()) != 1 {
		t.Errorf("buffered async publish 미발생: calls=%d", len(pub.snapshot()))
	}

	st := pc.Stats()
	if st.BufferCapacity != 64 {
		t.Errorf("BufferCapacity=%d, want 64", st.BufferCapacity)
	}
	if st.BufferDropped != 0 {
		t.Errorf("BufferDropped=%d, want 0", st.BufferDropped)
	}
}

// channel full 시 newest drop + bufferDropped 카운터 증가.
// upstream (호출 goroutine) 은 block 되지 않음.
func TestPricingConsumer_BufferFullDropsNewest(t *testing.T) {
	// publish 가 느린 publisher — worker 가 첫 tick 에서 멈춰 있는 동안 channel 가득.
	pub := &slowPublisher{delay: 200 * time.Millisecond}
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{{Symbol: "USDKRW", Pair: "USD/KRW", Active: true}})
	store := pricing.NewStore()
	store.Replace(&pricing.PricingTable{Version: 1})

	pc := NewPricingConsumer(PricingConsumerOptions{
		Store:     store,
		Symbols:   syms,
		Decoder:   JSONCookerDecoder(),
		Publisher: pub,
		Profiles: &StaticProfileSource{Profiles: []session.Profile{
			{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard},
		}},
		TickBufferSize: 2, // 매우 작은 buffer 로 빠른 가득.
	})

	// 첫 tick 은 worker 가 즉시 픽업 → publish 안에서 200ms block.
	// 다음 2 tick 은 channel buffer 점유.
	// 그 다음 3 tick 은 channel full → drop.
	for i := 0; i < 6; i++ {
		pc.OnTick(mkEnvTick("USDKRW", 1378.0+float64(i)*0.01, 1378.1, time.Now()))
	}

	st := pc.Stats()
	if st.TicksIn != 6 {
		t.Errorf("TicksIn=%d, want 6", st.TicksIn)
	}
	if st.BufferDropped < 3 {
		t.Errorf("BufferDropped=%d, want >=3 (6 호출 - 1 worker 픽업 - 2 buffer)", st.BufferDropped)
	}
}

// TickBufferSize=0 (default) — synchronous 동작 유지 (기존 path).
func TestPricingConsumer_SynchronousDefault(t *testing.T) {
	pub := &fakeQuotePublisher{}
	pc := newTestPricingConsumer(t, pub, []session.Profile{
		{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierStandard},
	})
	pc.OnTick(mkEnvTick("USDKRW", 1378.40, 1378.45, time.Now()))
	// synchronous — OnTick 반환 직후 publish 호출 완료.
	if len(pub.snapshot()) != 1 {
		t.Errorf("synchronous: calls=%d, want 1 (즉시 처리)", len(pub.snapshot()))
	}
	st := pc.Stats()
	if st.BufferCapacity != 0 {
		t.Errorf("BufferCapacity=%d, want 0 (synchronous)", st.BufferCapacity)
	}
}

// slowPublisher — publish 호출 시 지정 시간 block. backpressure 시뮬레이션.
type slowPublisher struct {
	delay time.Duration
}

func (s *slowPublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	time.Sleep(s.delay)
	return nil
}
