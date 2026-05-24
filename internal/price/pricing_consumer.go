package price

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
)

// QuotePublisher 는 마진 적용된 CustomerQuote 를 broker 로 publish 한다.
// PricingConsumer 가 호출하며, 운영 구현은 MymqQuotePublisher (broker pub),
// 테스트는 fakeQuotePublisher 가 캡처.
type QuotePublisher interface {
	PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error
}

// ProfileSource 는 현재 시점에 활성화된 Profile 목록을 제공한다.
// mci-admin 이 etcd 에 등록한 Profile 카탈로그를 watch 로 따라가는 구현 또는
// 정적 dev seed 구현 등.
type ProfileSource interface {
	ActiveProfiles() []session.Profile
}

// StaticProfileSource 는 고정된 Profile 목록을 가진 ProfileSource 구현.
// dev / 테스트 / 1차 prototype 용.
type StaticProfileSource struct {
	Profiles []session.Profile
}

// ActiveProfiles 는 보유 목록을 그대로 반환 (slice 자체를 호출자에게 노출하지 않음).
func (s *StaticProfileSource) ActiveProfiles() []session.Profile {
	out := make([]session.Profile, len(s.Profiles))
	copy(out, s.Profiles)
	return out
}

// PricingConsumer 는 TickConsumer 구현.
//
// 흐름:
//
//	Tick → JSONCookerDecoder → bid/ask
//	     → SymbolMap.Lookup(sym) → session.Pair
//	     → quote.Quote
//	     → for each ActiveProfile :
//	         PricingTable.Apply(raw, profile, SPOT) → CustomerQuote
//	         QuotePublisher.PublishQuote(profile, cq)
//	             broker FCCast Xchg=QUOTE Rkey=Profile.Key()
//
// 동시성:
//   - OnTick 은 broker subscribe goroutine 에서 호출 (단일).
//   - Store.Load 는 atomic.Pointer → lock 없음.
//   - ProfileSource.ActiveProfiles 는 호출자가 thread-safe 보장 (StaticProfileSource 는 immutable slice 복사).
type PricingConsumer struct {
	store     *pricing.Store
	symbols   *quote.SymbolMap
	decoder   CookerBodyDecoder
	publisher QuotePublisher
	profiles  ProfileSource
	tenor     pricing.Tenor // 대부분 SPOT
	logger    *slog.Logger

	// QuoteID 발행 + 등록 (선택적 — 둘 다 nil 이면 quoteid 비활성).
	quoteIDGen      *quoteid.Generator
	quoteIDReg      quoteid.Registry
	quoteValidity   time.Duration
	quoteRegCtx     context.Context
	quoteRegTimeout time.Duration

	ticksIn         atomic.Uint64
	ticksDropped    atomic.Uint64 // sym 미등록 / inactive / decode 실패
	quotesPublished atomic.Uint64
	publishErrors   atomic.Uint64
	quoteRegErrors  atomic.Uint64 // Registry.Put 실패 카운트
}

// PricingConsumerOptions 는 PricingConsumer 생성 옵션.
type PricingConsumerOptions struct {
	Store     *pricing.Store
	Symbols   *quote.SymbolMap
	Decoder   CookerBodyDecoder
	Publisher QuotePublisher
	Profiles  ProfileSource
	// Tenor 는 publish 할 시세의 만기 컨텍스트. 0 값이면 TenorSpot.
	Tenor  pricing.Tenor
	Logger *slog.Logger

	// QuoteIDGen / QuoteIDRegistry — quoteid 활성화. 둘 다 채워야 동작
	// (nil 이면 quoteid 미사용 — 기존 동작). QuoteValidity 는 publish 시점부터
	// 토큰이 유효한 wallclock 길이 (default 500ms).
	QuoteIDGen      *quoteid.Generator
	QuoteIDRegistry quoteid.Registry
	QuoteValidity   time.Duration
	// QuoteRegistryTimeout — Registry.Put 단위 timeout. default 200ms.
	QuoteRegistryTimeout time.Duration
}

// NewPricingConsumer 는 PricingConsumer 를 구성한다.
// store / symbols / decoder / publisher / profiles 는 모두 필수 (nil 이면 panic).
func NewPricingConsumer(opt PricingConsumerOptions) *PricingConsumer {
	if opt.Store == nil || opt.Symbols == nil || opt.Decoder == nil ||
		opt.Publisher == nil || opt.Profiles == nil {
		panic("price: PricingConsumer 필수 의존성 누락")
	}
	tenor := opt.Tenor
	if tenor == "" {
		tenor = pricing.TenorSpot
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	validity := opt.QuoteValidity
	if validity <= 0 {
		validity = 500 * time.Millisecond
	}
	regTimeout := opt.QuoteRegistryTimeout
	if regTimeout <= 0 {
		regTimeout = 200 * time.Millisecond
	}
	return &PricingConsumer{
		store:           opt.Store,
		symbols:         opt.Symbols,
		decoder:         opt.Decoder,
		publisher:       opt.Publisher,
		profiles:        opt.Profiles,
		tenor:           tenor,
		logger:          logger,
		quoteIDGen:      opt.QuoteIDGen,
		quoteIDReg:      opt.QuoteIDRegistry,
		quoteValidity:   validity,
		quoteRegCtx:     context.Background(),
		quoteRegTimeout: regTimeout,
	}
}

// OnTick 은 TickConsumer 인터페이스.
func (c *PricingConsumer) OnTick(t *Tick) {
	if t == nil {
		return
	}
	c.ticksIn.Add(1)

	pair, active, found := c.symbols.Lookup(t.Symbol)
	if !found || !active {
		c.ticksDropped.Add(1)
		return
	}
	bid, ask, ok := c.decoder(t.Body)
	if !ok {
		c.ticksDropped.Add(1)
		return
	}
	raw := quote.Quote{Pair: pair, Bid: bid, Ask: ask, TS: t.Received}

	tbl := c.store.Load()
	for _, prof := range c.profiles.ActiveProfiles() {
		cq := tbl.Apply(raw, prof, c.tenor)
		c.attachQuoteID(&cq, prof)
		if err := c.publisher.PublishQuote(prof, cq); err != nil {
			c.publishErrors.Add(1)
			c.logger.Warn("PricingConsumer: publish 실패",
				slog.String("profile", prof.Key()),
				slog.String("pair", string(pair)),
				slog.Any("error", err),
			)
			continue
		}
		c.quotesPublished.Add(1)
	}
}

// attachQuoteID — Generator + Registry 가 활성이면 QuoteID 발급 + ValidUntil
// 부착 + Registry.Put. Registry.Put 실패는 publish 자체를 막지 않음 (감사 추적
// best-effort) — 운영에서 quote_register_errors 메트릭으로 모니터링.
func (c *PricingConsumer) attachQuoteID(cq *pricing.CustomerQuote, prof session.Profile) {
	if c.quoteIDGen == nil || c.quoteIDReg == nil {
		return
	}
	id := c.quoteIDGen.Next()
	validUntil := cq.TS.Add(c.quoteValidity)
	cq.QuoteID = string(id)
	cq.ValidUntil = validUntil

	rec := quoteid.Record{
		QuoteID:    id,
		Pair:       cq.Pair,
		Profile:    prof,
		Tenor:      string(cq.Tenor),
		Bid:        cq.Bid,
		Ask:        cq.Ask,
		IssuedAt:   cq.TS.UnixNano(),
		ValidUntil: validUntil.UnixNano(),
		Sequence:   c.quoteIDGen.NextSequence(),
		Issuer:     c.quoteIDGen.Instance(),
	}
	ctx, cancel := context.WithTimeout(c.quoteRegCtx, c.quoteRegTimeout)
	defer cancel()
	if err := c.quoteIDReg.Put(ctx, rec); err != nil {
		c.quoteRegErrors.Add(1)
		c.logger.Warn("PricingConsumer: QuoteID Registry.Put 실패",
			slog.String("quote_id", string(id)),
			slog.Any("error", err))
	}
}

// PricingConsumerStats 는 누적 카운터 snapshot.
type PricingConsumerStats struct {
	TicksIn         uint64 `json:"ticks_in"`
	TicksDropped    uint64 `json:"ticks_dropped"`
	QuotesPublished uint64 `json:"quotes_published"`
	PublishErrors   uint64 `json:"publish_errors"`
	QuoteRegErrors  uint64 `json:"quote_register_errors"`
}

// Stats 는 누적 카운터 snapshot 을 반환.
func (c *PricingConsumer) Stats() PricingConsumerStats {
	return PricingConsumerStats{
		TicksIn:         c.ticksIn.Load(),
		TicksDropped:    c.ticksDropped.Load(),
		QuotesPublished: c.quotesPublished.Load(),
		PublishErrors:   c.publishErrors.Load(),
		QuoteRegErrors:  c.quoteRegErrors.Load(),
	}
}

// ─── MymqQuotePublisher ────────────────────────────────────────────────────

// MymqQuotePublisher 는 mymq.Client 를 통해 ExchangeQuote 로 publish 한다.
// routing-key 는 Profile.Key() (예: "WEB.BRANCH.VIP").
//
// 메시지 본문은 JSON (customerQuoteDTO). mci-edge-price 와의 wire 호환을 위해
// 이 DTO 형식을 합의 문서로 별도 유지하는 것이 좋다 (TODO: docs/quote-publish-schema.md).
type MymqQuotePublisher struct {
	client interface {
		Send(*mymq.FrameInput) error
	}
}

// NewMymqQuotePublisher 는 mymq.Client (또는 Send 메서드만 가진 mock) 을 받는다.
func NewMymqQuotePublisher(c interface {
	Send(*mymq.FrameInput) error
}) *MymqQuotePublisher {
	return &MymqQuotePublisher{client: c}
}

// PublishQuote 는 CustomerQuote 를 ExchangeQuote 로 broker publish.
func (p *MymqQuotePublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	dto := customerQuoteDTO{
		Pair:    string(cq.Pair),
		Channel: string(profile.Channel),
		Site:    string(profile.Site),
		Tier:    string(profile.Tier),
		Tenor:   string(cq.Tenor),
		Bid:     cq.Bid,
		Ask:     cq.Ask,
		TS:      cq.TS.UTC().Format(time.RFC3339Nano),
		RawBid:  cq.RawBid,
		RawAsk:  cq.RawAsk,
		Version: cq.TableVersion,
		QuoteID: cq.QuoteID,
	}
	if !cq.ValidUntil.IsZero() {
		dto.ValidUntil = cq.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	body, err := json.Marshal(dto)
	if err != nil {
		return fmt.Errorf("pricing_consumer: marshal: %w", err)
	}
	return p.client.Send(&mymq.FrameInput{
		Func: mymq.FCCast,
		Xchg: mymq.ExchangeQuote,
		Rkey: mymq.RKeyQuote(profile.Key()),
		Body: body,
	})
}

// ─── MultiQuotePublisher ───────────────────────────────────────────────────

// MultiQuotePublisher 는 여러 QuotePublisher 에 동시 송신 (broker + gRPC 등).
// 일부 publisher 실패는 무시하고 나머지 진행. 마지막 에러를 반환.
type MultiQuotePublisher struct {
	publishers []QuotePublisher
}

// NewMultiQuotePublisher 는 fan-out publisher 를 구성.
// nil publisher 는 자동 제거.
func NewMultiQuotePublisher(publishers ...QuotePublisher) *MultiQuotePublisher {
	out := make([]QuotePublisher, 0, len(publishers))
	for _, p := range publishers {
		if p != nil {
			out = append(out, p)
		}
	}
	return &MultiQuotePublisher{publishers: out}
}

// PublishQuote 는 각 publisher 에 순차 송신.
// 일부 실패는 마지막 에러를 모아서 반환하되 다른 publisher 송신은 계속한다.
func (m *MultiQuotePublisher) PublishQuote(profile session.Profile, cq pricing.CustomerQuote) error {
	var last error
	for _, p := range m.publishers {
		if err := p.PublishQuote(profile, cq); err != nil {
			last = err
		}
	}
	return last
}

// customerQuoteDTO 는 broker publish wire JSON.
type customerQuoteDTO struct {
	Pair       string  `json:"pair"`
	Channel    string  `json:"chan"`
	Site       string  `json:"site"`
	Tier       string  `json:"tier"`
	Tenor      string  `json:"tenor"`
	Bid        float64 `json:"bid"`
	Ask        float64 `json:"ask"`
	TS         string  `json:"ts"`
	RawBid     float64 `json:"raw_bid,omitempty"`
	RawAsk     float64 `json:"raw_ask,omitempty"`
	Version    int64   `json:"v"`
	QuoteID    string  `json:"quote_id,omitempty"`
	ValidUntil string  `json:"valid_until,omitempty"` // RFC3339Nano
}
