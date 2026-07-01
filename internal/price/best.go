package price

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// best.go — 다중시장 best 호가 산정 컨슈머.
//
// 모델 (nmds/mds 의 mdssise_make_best 패턴):
//
//	per (Symbol, Source) 최신 quote 캐시
//	  → best_bid = max(bid)  across active sources
//	    best_ask = min(ask)  across active sources
//	  → 매 raw tick 마다 재계산 후 합성 Tick(Source=BEST) emit
//
// downstream (Aggregator/PricingConsumer/gRPC) 는 BEST 만 소비. raw 다중시장
// 시세는 BestConsumer 에서 흡수되어 외부에는 통합된 best 만 노출.
//
// 단일 feed 환경 (cooker 가 이미 best 합산해서 publish) 에서도 안전 — best of 1
// = 그 자체. 회귀 없음.

// BestOptions — BestConsumer 생성 옵션.
type BestOptions struct {
	// MaxStaleness — feed quote 가 이 시간 이상 갱신 없으면 best 계산에서 제외.
	// 0 이면 30s 기본. 음수면 stale 제외 비활성 (모든 quote 영구 유효).
	MaxStaleness time.Duration

	// Logger — nil 이면 slog.Default().
	Logger *slog.Logger

	// Dedup — same-price / below-tick 필터. default off. 자세히는 DedupOptions.
	Dedup DedupOptions
}

// DedupOptions — best emit 전 값 기반 필터. FX 관례상 quiet market 에서 같은
// bid/ask 가 반복 발행되면 downstream (Aggregator/PricingConsumer/gRPC) 이
// 의미 없는 처리 반복. 활성 시 이전 emit 값과 비교해 skip.
//
// 두 단계:
//  1. exact-match — 이전 emit 과 bid + ask 가 정확히 동일하면 skip.
//  2. below-tick — TickSizeMultiplier > 0 이면 bid/ask 각 변화가 tick_size ×
//     multiplier 미만이면 skip. 예: multiplier=1.0 이면 1 tick 미만 skip.
//
// default off — 잘못 튜닝 시 downstream 이 stale quote 위에서 결정. 운영 관측
// 후 on 판단.
type DedupOptions struct {
	// Enabled — false 면 모든 dedup skip (기존 동작 유지).
	Enabled bool

	// TickSizeMultiplier — 0=exact-match 만. 1.0=1 tick 미만 skip. 음수 무효.
	// 실제 tick size 는 심볼 → quote currency 로 추정 (JPY/KRW/CNY/TWD/IDR/VND=0.01,
	// 그 외=0.0001) — Phase C 에서 SymbolEntry.TickSize override 노출 예정.
	TickSizeMultiplier float64

	// TickSizeOverride — 심볼별 tick_size 강제 (default 추정 대신). 빈 map 이면
	// symbolQuoteTickSize() 로 fallback. 편의: Phase C 의 etcd 카탈로그 대신
	// static seed 로 override 필요할 때만 사용.
	TickSizeOverride map[string]float64
}

// lastEmit — dedup 필터에 쓰는 이전 emit 스냅샷.
type lastEmit struct {
	bid, ask float64
	// hasValue — zero-value (첫 emit) 인지 구분.
	hasValue bool
}

// BestConsumer 는 raw 다중시장 Tick 을 받아 best 호가를 산정해 downstream 으로
// 전달한다. TickConsumer 인터페이스를 구현 — Server.AddConsumer 로 등록.
type BestConsumer struct {
	maxStaleness time.Duration
	logger       *slog.Logger

	mu    sync.Mutex
	cache map[string]map[string]sourceQuote // symbol → source → quote

	downstream []TickConsumer

	// invariant 위반으로 reject 한 raw quote 수 — bid<=0 / ask<=0 / bid>ask.
	// feed cooker / forwarder 의 데이터 sanity 진단. /v1/best-stats 노출 (후속).
	rejectedQuotes atomic.Uint64

	// dedup — enabled 면 emit 전 same-price / below-tick 필터.
	dedup                 DedupOptions
	lastEmitted           map[string]lastEmit // symbol → last emit (cache mu 로 보호)
	dedupDroppedSame      atomic.Uint64
	dedupDroppedBelowTick atomic.Uint64
	emittedTotal          atomic.Uint64
}

type sourceQuote struct {
	bid, ask float64
	ts       time.Time
}

// NewBestConsumer 는 BestConsumer 를 생성한다. downstream 은 best Tick 을
// 받을 consumer 들 (Aggregator, PricingConsumer, gRPCServer 등).
func NewBestConsumer(opts BestOptions, downstream ...TickConsumer) *BestConsumer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stale := opts.MaxStaleness
	if stale == 0 {
		stale = 30 * time.Second
	}
	d := opts.Dedup
	if d.TickSizeMultiplier < 0 {
		d.TickSizeMultiplier = 0
	}
	return &BestConsumer{
		maxStaleness: stale,
		logger:       logger,
		cache:        make(map[string]map[string]sourceQuote),
		downstream:   downstream,
		dedup:        d,
		lastEmitted:  make(map[string]lastEmit),
	}
}

// shouldDedupLocked — dedup 필터. caller 가 mu 잡음. skip 하려면 true 반환.
//
// hasValue=false (첫 emit) 이면 절대 skip 안 함. exact-match 우선, 그다음
// below-tick. tick_size 는 심볼별 override → symbolQuoteTickSize fallback.
//
// 반환값의 두 bool 은 각각 same-price / below-tick 계열 여부 (metric label).
func (b *BestConsumer) shouldDedupLocked(symbol string, bid, ask float64) (skip, sameSp bool) {
	if !b.dedup.Enabled {
		return false, false
	}
	prev, ok := b.lastEmitted[symbol]
	if !ok || !prev.hasValue {
		return false, false
	}
	// exact-match — same-price.
	if prev.bid == bid && prev.ask == ask {
		return true, true
	}
	// below-tick — multiplier > 0 이면.
	if b.dedup.TickSizeMultiplier > 0 {
		ts := b.tickSizeFor(symbol)
		if ts > 0 {
			threshold := ts * b.dedup.TickSizeMultiplier
			if absDiff(prev.bid, bid) < threshold && absDiff(prev.ask, ask) < threshold {
				return true, false
			}
		}
	}
	return false, false
}

// tickSizeFor — override 우선, 없으면 심볼의 quote currency 로 추정.
func (b *BestConsumer) tickSizeFor(symbol string) float64 {
	if v, ok := b.dedup.TickSizeOverride[symbol]; ok && v > 0 {
		return v
	}
	return symbolQuoteTickSize(symbol)
}

// symbolQuoteTickSize — 심볼 마지막 3자를 quote ccy 로 간주하고 tick_size 추정.
// FX dealer convention: JPY/KRW/CNY/TWD/IDR/VND 는 0.01, 그 외 major/EM 은 0.0001.
// 심볼 형식이 6자 미만이면 0 반환 (즉 below-tick dedup skip).
func symbolQuoteTickSize(symbol string) float64 {
	if len(symbol) < 6 {
		return 0
	}
	quote := symbol[len(symbol)-3:]
	// 대문자 정규화 (etc/symbols.json 은 uppercase 이나 안전 보정).
	q := [3]byte{}
	for i := 0; i < 3; i++ {
		c := quote[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		q[i] = c
	}
	switch string(q[:]) {
	case "JPY", "KRW", "CNY", "TWD", "IDR", "VND":
		return 0.01
	default:
		return 0.0001
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// AddDownstream 은 emit 대상 consumer 를 추가한다. Server.Start 가 호출하기
// 전에만 안전 (downstream 변경에 lock 안 잡음 — 정상 운영 시 hot 호출 없음).
func (b *BestConsumer) AddDownstream(c TickConsumer) {
	b.downstream = append(b.downstream, c)
}

// OnTick 은 raw 다중시장 tick 을 받아 best 를 재계산하고 합성 best Tick 을
// downstream 으로 fan-out 한다.
func (b *BestConsumer) OnTick(t *Tick) {
	if t == nil || t.Symbol == "" {
		return
	}
	// Source 미지정 시 best 산정 불가 — drop.
	// (forwarder 가 JSONEnvelope.Src 를 채우고 Server.handleUnsolicited 가
	//  Tick.Source 에 복사하는 contract.)
	if t.Source == "" {
		return
	}
	// 자기 자신이 emit 한 합성 best 를 다시 받지 않게 ignore — 정상 wiring 에선
	// 일어나지 않지만 ring/loop 방어.
	if t.Source == SourceBest {
		return
	}

	// raw body 에서 bid/ask 추출. DecodeJSONEnvelope 자체가 invariant 검증
	// (bid<=0 / ask<=0 / bid>ask / missing sym) — ErrEnvelopeInvalidBidAsk 등
	// 으로 거부. 즉 broken raw 가 cache 를 오염시키지는 않음.
	//
	// 다만 운영 가시성을 위해 decoder reject 를 카운터로 노출 — feed cooker /
	// forwarder 의 데이터 sanity 진단. /v1/best-stats 의 rejected_quotes 모니터링.
	env, err := quote.DecodeJSONEnvelope(t.Body)
	if err != nil {
		b.rejectedQuotes.Add(1)
		return
	}

	b.mu.Lock()
	bySource, ok := b.cache[t.Symbol]
	if !ok {
		bySource = make(map[string]sourceQuote)
		b.cache[t.Symbol] = bySource
	}
	bySource[t.Source] = sourceQuote{bid: env.Bid, ask: env.Ask, ts: time.Now()}
	bestBid, bestAsk, srcCount := b.recomputeLocked(bySource)
	// dedup — emit 전 이전 값과 비교. skip 시 lastEmitted 갱신 안 함 (다음
	// 실제 emit 시점이 정확한 delta 판정 기준).
	skip, sameSp := b.shouldDedupLocked(t.Symbol, bestBid, bestAsk)
	if !skip {
		b.lastEmitted[t.Symbol] = lastEmit{bid: bestBid, ask: bestAsk, hasValue: true}
	}
	b.mu.Unlock()

	if skip {
		if sameSp {
			b.dedupDroppedSame.Add(1)
		} else {
			b.dedupDroppedBelowTick.Add(1)
		}
		return
	}

	// 음수 = cross fallback 발동 (정보 보존용). hot path 는 절대값만 본다.
	if srcCount == 0 {
		// 방금 update 했는데 0 이 나오는 건 모든 entry 가 stale → 비정상.
		return
	}

	// 합성 best envelope. TS 는 emit 시각 — Aggregator OHLC 시계열에 사용.
	bestBody, err := json.Marshal(quote.JSONEnvelope{
		Sym: t.Symbol,
		Bid: bestBid,
		Ask: bestAsk,
		TS:  time.Now().UTC(),
		Src: SourceBest,
		Seq: uint64(t.SeqNum),
	})
	if err != nil {
		b.logger.Warn("best envelope 마샬 실패", slog.String("sym", t.Symbol), slog.Any("err", err))
		return
	}

	best := &Tick{
		MarketID: t.MarketID,
		Symbol:   t.Symbol,
		SeqNum:   t.SeqNum,
		Mask:     t.Mask,
		Type:     t.Type,
		Flag:     t.Flag,
		Body:     bestBody,
		Received: time.Now(),
		Source:   SourceBest,
	}
	b.emittedTotal.Add(1)
	for _, c := range b.downstream {
		c.OnTick(best)
	}
}

// recomputeLocked — bySource 의 active entry 들로 best 산정. caller 가 lock 잡음.
//
// 정상: max(bid) / min(ask) across active sources.
// Crossed (best_bid > best_ask): max bid 와 min ask 가 서로 다른 feed 라서
// 한쪽 feed 가 stale/skewed 인 신호. mds 의 mdssise_make_new_best 와 같은
// 정책으로, 최신 ts 의 feed 의 bid/ask 를 그대로 사용 (해당 feed 자체는
// 일관된 spread 라 cross 가 없음). srcCount=1 로 표시해서 호출자가 "fallback
// 발동" 을 구분할 수 있게 음수 값으로 negate 한다.
func (b *BestConsumer) recomputeLocked(bySource map[string]sourceQuote) (bid, ask float64, srcCount int) {
	now := time.Now()
	first := true
	var newest sourceQuote
	hasNewest := false
	for _, sq := range bySource {
		if b.maxStaleness >= 0 && now.Sub(sq.ts) > b.maxStaleness {
			continue
		}
		if first {
			bid, ask = sq.bid, sq.ask
			first = false
		} else {
			if sq.bid > bid {
				bid = sq.bid
			}
			if sq.ask < ask {
				ask = sq.ask
			}
		}
		srcCount++
		if !hasNewest || sq.ts.After(newest.ts) {
			newest = sq
			hasNewest = true
		}
	}
	// Cross 검출 → 최신 feed 의 일관된 bid/ask 로 fallback.
	if hasNewest && srcCount > 1 && bid > ask {
		bid, ask = newest.bid, newest.ask
		srcCount = -srcCount // 음수 = "crossed → fallback 발동" 마커
	}
	return bid, ask, srcCount
}

// Stats — 디버깅용 스냅샷 (per-symbol active source count + last best).
type BestStats struct {
	Symbols map[string]BestSymbolStat `json:"symbols"`
	// 운영 가시성 — invariant 위반으로 reject 한 raw quote 누적.
	// 0 이 아니면 cooker / forwarder 의 데이터 sanity 점검 필요.
	RejectedQuotes uint64 `json:"rejected_quotes"`

	// Dedup — same-price / below-tick 필터 카운터. Enabled=false 면 모두 0.
	Dedup BestDedupStats `json:"dedup"`
}

// BestDedupStats — dedup 필터 누적. Emit 은 실제로 fan-out 된 tick,
// Dropped* 는 skip 된 tick.
type BestDedupStats struct {
	Enabled            bool    `json:"enabled"`
	TickSizeMultiplier float64 `json:"tick_size_multiplier"`
	Emitted            uint64  `json:"emitted"`
	DroppedSamePrice   uint64  `json:"dropped_same_price"`
	DroppedBelowTick   uint64  `json:"dropped_below_tick"`
}

type BestSymbolStat struct {
	ActiveSources int `json:"active_sources"`
	// Sources — active (non-stale) source 이름. 운영자가 어떤 feed 가
	// 실제 best 산정에 기여 중인지 확인 (예: ["KMB","SMB"]). 정렬 보장 —
	// 동일 입력에 대해 동일 출력을 내기 위함. ActiveSources 와 동일 staleness
	// 필터로 수집되므로 len(Sources) == ActiveSources (crossed fallback 시도
	// |ActiveSources|).
	Sources []string `json:"sources"`
	// SourceQuotes — per-source 의 raw bid/ask/ts. mds 의 W9501S02 형
	// "거래소별 호가 조회" 백엔드 (wire adapter `cside/wtgquery` 가 사용).
	// Sources 와 동일 staleness 필터. Sources 가 quick scan, 본 필드가 detail.
	SourceQuotes   map[string]BestSourceQuote `json:"source_quotes"`
	BestBid        float64                    `json:"best_bid"`
	BestAsk        float64                    `json:"best_ask"`
	CrossedFallbck bool                       `json:"crossed_fallback,omitempty"`
}

// BestSourceQuote — per-source 호가 + 수신 시각.
type BestSourceQuote struct {
	Bid float64   `json:"bid"`
	Ask float64   `json:"ask"`
	TS  time.Time `json:"ts"`
}

// Stats 는 현재 cache 상태의 스냅샷을 반환 (HTTP /v1/best-stats 노출용).
func (b *BestConsumer) Stats() BestStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := BestStats{
		Symbols:        make(map[string]BestSymbolStat, len(b.cache)),
		RejectedQuotes: b.rejectedQuotes.Load(),
		Dedup: BestDedupStats{
			Enabled:            b.dedup.Enabled,
			TickSizeMultiplier: b.dedup.TickSizeMultiplier,
			Emitted:            b.emittedTotal.Load(),
			DroppedSamePrice:   b.dedupDroppedSame.Load(),
			DroppedBelowTick:   b.dedupDroppedBelowTick.Load(),
		},
	}
	now := time.Now()
	for sym, bySource := range b.cache {
		bid, ask, n := b.recomputeLocked(bySource)
		crossed := n < 0
		if crossed {
			n = -n
		}
		// active source 이름 + 값 수집 — recomputeLocked 와 동일한 staleness
		// 필터. 운영자가 "SMB 가 들어왔는지" 를 카운트가 아닌 이름으로
		// 직접 판정. Stats 는 hot path 아니라 alloc 비용 무시 가능.
		sources := make([]string, 0, len(bySource))
		srcQuotes := make(map[string]BestSourceQuote, len(bySource))
		for src, sq := range bySource {
			if b.maxStaleness >= 0 && now.Sub(sq.ts) > b.maxStaleness {
				continue
			}
			sources = append(sources, src)
			srcQuotes[src] = BestSourceQuote{Bid: sq.bid, Ask: sq.ask, TS: sq.ts}
		}
		sort.Strings(sources)
		out.Symbols[sym] = BestSymbolStat{
			ActiveSources:  n,
			Sources:        sources,
			SourceQuotes:   srcQuotes,
			BestBid:        bid,
			BestAsk:        ask,
			CrossedFallbck: crossed,
		}
	}
	return out
}
