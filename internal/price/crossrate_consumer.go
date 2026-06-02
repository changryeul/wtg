package price

import (
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// SourceCross — CrossRateConsumer 가 emit 하는 합성 Tick 의 Source 마커.
// BestConsumer 의 SourceBest 와 같은 위치 — downstream (Aggregator/PricingConsumer)
// 이 이 값으로 cross vs direct 를 구분 가능.
const SourceCross = "CROSS"

// CrossRateConsumer — direct pair 의 BEST tick 을 받아 의존 cross pair 들의
// 호가를 worse-side 합성 후 downstream 으로 emit.
//
// 동작:
//  1. OnTick: 도착한 tick 의 pair 추출 → leg cache 갱신.
//  2. reverse index 로 이 pair 를 leg 로 쓰는 cross 들 조회.
//  3. 각 cross 마다 maybeEmitCross:
//     - debounce: 마지막 emit 후 windowMs 이내면 skip
//     - 두 leg 모두 fresh (maxStaleness 이내) 여야 emit
//     - ComputeCross 합성 → cross Tick 빌드 → downstream
//
// 부하 정책 (P6 권장):
//   - debounce 10ms — 같은 cross 의 짧은 시간 중복 emit 차단
//   - max staleness 30s — BestConsumer 와 동일 (한 쪽 leg 가 30s+ 미수신이면 emit X)
//   - 즉시 emit (별도 goroutine pool 없음) — leg tick goroutine 에서 동기 처리
//   - reverse index O(1) lookup + ComputeCross 산술 수십 ns → CPU 부담 미미
//   - 진짜 병목은 downstream (PricingConsumer 의 5L fan-out, gRPC stream)
type CrossRateConsumer struct {
	symbols *quote.SymbolMap    // Tick.Symbol → session.Pair (fallback)
	pairs   *pricing.PairMaster // Tick.Symbol → session.Pair (우선 — single SoT)

	// formulas / leg2cross — 운영자 정의 cross 산식. ReplaceFormulas 로 hot reload.
	mu        sync.RWMutex
	formulas  map[session.Pair]pricing.CrossFormula
	leg2cross map[session.Pair][]session.Pair

	// legCache — pair → 최신 호가. atomic.Pointer 대신 sync.Map 사용 (key 가
	// 가변 set 이고 hot read/write 분리).
	legCache sync.Map // session.Pair → *legState

	// debounce — cross pair → 마지막 emit 시각. sync.Map.
	debounce       sync.Map // session.Pair → time.Time
	debounceWindow time.Duration
	maxStaleness   time.Duration

	// lastEmits — cross pair → 마지막 합성 결과 (forward-snapshot 등 외부 조회용).
	// emit 마다 갱신. LatestCross 로 read.
	lastEmits sync.Map // session.Pair → *crossSnap

	downstream []TickConsumer

	logger *slog.Logger

	emitsTotal         atomic.Uint64
	skippedDebounce    atomic.Uint64
	skippedStale       atomic.Uint64
	skippedMissingLeg  atomic.Uint64
	skippedUnknownPair atomic.Uint64
	errors             atomic.Uint64
}

type legState struct {
	bid float64
	ask float64
	ts  time.Time
}

// crossSnap — 마지막 emit 결과 (forward-snapshot 외부 조회용).
type crossSnap struct {
	Bid float64
	Ask float64
	TS  time.Time
}

// CrossRateOptions — 생성 옵션.
type CrossRateOptions struct {
	Symbols        *quote.SymbolMap    // fallback. Pairs 가 있으면 그게 우선.
	Pairs          *pricing.PairMaster // PairMaster 가 우선 — single SoT.
	Logger         *slog.Logger
	DebounceWindow time.Duration // default 10ms
	MaxStaleness   time.Duration // default 30s
}

// NewCrossRateConsumer — 옵션으로 생성. downstream 은 AddDownstream 으로.
func NewCrossRateConsumer(opt CrossRateOptions) *CrossRateConsumer {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dw := opt.DebounceWindow
	if dw <= 0 {
		dw = 10 * time.Millisecond
	}
	ms := opt.MaxStaleness
	if ms <= 0 {
		ms = 30 * time.Second
	}
	return &CrossRateConsumer{
		symbols:        opt.Symbols,
		pairs:          opt.Pairs,
		formulas:       map[session.Pair]pricing.CrossFormula{},
		leg2cross:      map[session.Pair][]session.Pair{},
		debounceWindow: dw,
		maxStaleness:   ms,
		logger:         logger,
	}
}

// AddDownstream — 합성 cross tick 을 받을 consumer 등록.
func (c *CrossRateConsumer) AddDownstream(ds TickConsumer) {
	c.downstream = append(c.downstream, ds)
}

// ReplaceFormulas — 운영 hot reload. 새 set 으로 통째 교체.
// reverse index 도 재빌드. 진행 중인 emit 은 race 없음 (RWMutex).
func (c *CrossRateConsumer) ReplaceFormulas(formulas map[session.Pair]pricing.CrossFormula) {
	leg2 := make(map[session.Pair][]session.Pair, len(formulas)*2)
	for crossPair, f := range formulas {
		leg2[f.LegA] = append(leg2[f.LegA], crossPair)
		leg2[f.LegB] = append(leg2[f.LegB], crossPair)
	}
	c.mu.Lock()
	c.formulas = formulas
	c.leg2cross = leg2
	c.mu.Unlock()
	c.logger.Info("CrossRateConsumer formulas 교체",
		slog.Int("crosses", len(formulas)),
		slog.Int("leg_keys", len(leg2)))
}

// OnTick — TickConsumer 인터페이스 구현. BestConsumer 의 downstream 으로 등록.
func (c *CrossRateConsumer) OnTick(t *Tick) {
	if t == nil || t.Symbol == "" {
		return
	}
	// 자기 자신이 emit 한 cross 를 재진입 차단.
	if t.Source == SourceCross {
		return
	}
	pair, _, found := c.lookupPair(t.Symbol)
	if !found {
		c.skippedUnknownPair.Add(1)
		return
	}
	// leg 캐시 갱신.
	env, err := quote.DecodeJSONEnvelope(t.Body)
	if err != nil {
		return
	}
	state := &legState{bid: env.Bid, ask: env.Ask, ts: time.Now()}
	c.legCache.Store(pair, state)

	// 이 pair 를 leg 로 쓰는 cross 들 조회.
	c.mu.RLock()
	crosses := c.leg2cross[pair]
	c.mu.RUnlock()

	for _, crossPair := range crosses {
		c.maybeEmitCross(crossPair, t)
	}
}

// lookupPair — Tick.Symbol → session.Pair 변환.
//
//	1순위: PairMaster (single SoT, fx-sync 가 미러링)
//	2순위: SymbolMap (legacy)
//	둘 다 없으면 Symbol 자체를 pair 로 (테스트 편의).
func (c *CrossRateConsumer) lookupPair(sym string) (session.Pair, bool, bool) {
	if c.pairs != nil {
		if pair, ok := c.pairs.LookupBySymbol(sym); ok {
			return pair, true, true
		}
	}
	if c.symbols != nil {
		pair, active, found := c.symbols.Lookup(sym)
		if !found || !active {
			return "", false, false
		}
		return pair, active, true
	}
	if c.pairs == nil && c.symbols == nil {
		return session.Pair(sym), true, true
	}
	return "", false, false
}

func (c *CrossRateConsumer) maybeEmitCross(crossPair session.Pair, srcTick *Tick) {
	// debounce check.
	if v, ok := c.debounce.Load(crossPair); ok {
		if time.Since(v.(time.Time)) < c.debounceWindow {
			c.skippedDebounce.Add(1)
			return
		}
	}

	c.mu.RLock()
	formula, ok := c.formulas[crossPair]
	c.mu.RUnlock()
	if !ok {
		return
	}

	aRaw, okA := c.legCache.Load(formula.LegA)
	bRaw, okB := c.legCache.Load(formula.LegB)
	if !okA || !okB {
		c.skippedMissingLeg.Add(1)
		return
	}
	sA := aRaw.(*legState)
	sB := bRaw.(*legState)
	now := time.Now()
	if now.Sub(sA.ts) > c.maxStaleness || now.Sub(sB.ts) > c.maxStaleness {
		c.skippedStale.Add(1)
		return
	}

	res, err := pricing.ComputeCross(formula,
		pricing.CrossInput{Bid: sA.bid, Ask: sA.ask},
		pricing.CrossInput{Bid: sB.bid, Ask: sB.ask})
	if err != nil {
		c.errors.Add(1)
		c.logger.Warn("cross 합성 실패",
			slog.String("cross", string(crossPair)),
			slog.String("leg_a", string(formula.LegA)),
			slog.String("leg_b", string(formula.LegB)),
			slog.Any("error", err))
		return
	}

	// cross Tick 빌드 — Symbol 은 reverse SymbolMap 시도, 없으면 pair 의 slash 제거.
	crossSym := c.reverseSymbol(crossPair)
	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: crossSym, Bid: res.Bid, Ask: res.Ask, TS: now, Src: SourceCross,
	})
	crossTick := &Tick{
		MarketID: srcTick.MarketID,
		Symbol:   crossSym,
		SeqNum:   srcTick.SeqNum,
		Body:     body,
		Received: now,
		Source:   SourceCross,
	}
	for _, ds := range c.downstream {
		ds.OnTick(crossTick)
	}
	c.debounce.Store(crossPair, now)
	c.lastEmits.Store(crossPair, &crossSnap{Bid: res.Bid, Ask: res.Ask, TS: now})
	c.emitsTotal.Add(1)
}

// LatestCross — 본 cross pair 의 마지막 emit 결과. 외부 (forward-snapshot 등)
// 가 BestConsumer cache 에 없는 cross 호가를 조회하는 경로.
func (c *CrossRateConsumer) LatestCross(pair session.Pair) (bid, ask float64, ts time.Time, ok bool) {
	v, found := c.lastEmits.Load(pair)
	if !found {
		return 0, 0, time.Time{}, false
	}
	s := v.(*crossSnap)
	return s.Bid, s.Ask, s.TS, true
}

// reverseSymbol — cross pair 의 외부 symbol. PairMaster 우선, SymbolMap.Reverse
// 다음, 없으면 pair 의 "/" 제거 (USD/KRW → USDKRW).
func (c *CrossRateConsumer) reverseSymbol(pair session.Pair) string {
	if c.pairs != nil {
		if s, ok := c.pairs.ReverseSymbol(pair); ok {
			return s
		}
	}
	if c.symbols != nil {
		if s, ok := c.symbols.Reverse(pair); ok {
			return s
		}
	}
	s := string(pair)
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '/' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// CrossRateStats — 누적 카운터 snapshot.
type CrossRateStats struct {
	EmitsTotal         uint64 `json:"emits_total"`
	SkippedDebounce    uint64 `json:"skipped_debounce"`
	SkippedStale       uint64 `json:"skipped_stale"`
	SkippedMissingLeg  uint64 `json:"skipped_missing_leg"`
	SkippedUnknownPair uint64 `json:"skipped_unknown_pair"`
	Errors             uint64 `json:"errors"`
	FormulaCount       int    `json:"formula_count"`
	LegKeysCount       int    `json:"leg_keys_count"`
}

func (c *CrossRateConsumer) Stats() CrossRateStats {
	c.mu.RLock()
	fc := len(c.formulas)
	lc := len(c.leg2cross)
	c.mu.RUnlock()
	return CrossRateStats{
		EmitsTotal:         c.emitsTotal.Load(),
		SkippedDebounce:    c.skippedDebounce.Load(),
		SkippedStale:       c.skippedStale.Load(),
		SkippedMissingLeg:  c.skippedMissingLeg.Load(),
		SkippedUnknownPair: c.skippedUnknownPair.Load(),
		Errors:             c.errors.Load(),
		FormulaCount:       fc,
		LegKeysCount:       lc,
	}
}
