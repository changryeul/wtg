package price

import (
	"encoding/json"
	"log/slog"
	"sync"
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
}

// BestConsumer 는 raw 다중시장 Tick 을 받아 best 호가를 산정해 downstream 으로
// 전달한다. TickConsumer 인터페이스를 구현 — Server.AddConsumer 로 등록.
type BestConsumer struct {
	maxStaleness time.Duration
	logger       *slog.Logger

	mu    sync.Mutex
	cache map[string]map[string]sourceQuote // symbol → source → quote

	downstream []TickConsumer
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
	return &BestConsumer{
		maxStaleness: stale,
		logger:       logger,
		cache:        make(map[string]map[string]sourceQuote),
		downstream:   downstream,
	}
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

	// raw body 에서 bid/ask 추출.
	env, err := quote.DecodeJSONEnvelope(t.Body)
	if err != nil {
		// 디코딩 실패는 quiet drop — Aggregator/Conflation 도 동일 정책.
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
	b.mu.Unlock()

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
}

type BestSymbolStat struct {
	ActiveSources  int     `json:"active_sources"`
	BestBid        float64 `json:"best_bid"`
	BestAsk        float64 `json:"best_ask"`
	CrossedFallbck bool    `json:"crossed_fallback,omitempty"`
}

// Stats 는 현재 cache 상태의 스냅샷을 반환 (HTTP /v1/best-stats 노출용).
func (b *BestConsumer) Stats() BestStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := BestStats{Symbols: make(map[string]BestSymbolStat, len(b.cache))}
	for sym, bySource := range b.cache {
		bid, ask, n := b.recomputeLocked(bySource)
		crossed := n < 0
		if crossed {
			n = -n
		}
		out.Symbols[sym] = BestSymbolStat{
			ActiveSources:  n,
			BestBid:        bid,
			BestAsk:        ask,
			CrossedFallbck: crossed,
		}
	}
	return out
}
