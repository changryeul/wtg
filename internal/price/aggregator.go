package price

import (
	"context"
	"sync"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// CookerBodyDecoder 는 broker pushdata.msgb (raw bytes) 안에 들어있는
// 실 시세 페이로드를 bid/ask 로 디코딩한다.
//
// **현재 cooker payload wire 포맷은 운영팀과 합의 진행 중.** docs/cooker-quote-schema.md
// (작성 예정) 의 v1 JSON envelope 가 합의되면 그 디코더를 주입한다.
// 합의 전까지는 nil 을 주입하거나 test stub 으로 대체.
//
// ok=false 면 해당 tick 은 drop (집계 대상에서 제외).
type CookerBodyDecoder func(body []byte) (bid, ask float64, ok bool)

// BarCloseHandler 는 봉이 닫힐 때 호출되는 콜백.
//
// 호출 시점:
//   - OnTick 안에서 새 bucket 의 tick 이 도착했을 때 (이전 봉 close)
//   - Sweeper goroutine 이 만료된 봉을 발견했을 때
//
// 호출 goroutine 은 호출 시점에 따라 다르므로 핸들러는 thread-safe 해야 한다.
// 핸들러 안에서 시간이 오래 걸리는 작업(DB write 등)은 별도 채널/큐로 위임할 것.
type BarCloseHandler func(*quote.Bar)

// Aggregator 는 raw Tick 스트림을 받아 (pair × timeframe) 별 OHLC 봉을 누적한다.
//
// 흐름:
//
//	OnTick(*Tick)
//	  └→ Tick.Symbol → SymbolMap.Lookup → session.Pair (active 검사)
//	  └→ Tick.Body → CookerBodyDecoder → bid/ask
//	  └→ Quote 구성 → AllTimeframes 각각에 대해 applyTo
//	       └→ 현재 봉이 같은 bucket : Update
//	       └→ 다른 bucket : 이전 봉 Close + 콜백 → 새 봉 NewBar
//
// 동시성:
//   - OnTick 은 broker subscribe goroutine 에서 호출되므로 단일 producer.
//   - Sweeper goroutine 이 별도로 만료 봉 정리.
//   - bars map 접근은 단일 mutex 로 보호 (100tps × 6 TF = 600 op/sec — 무경쟁).
type Aggregator struct {
	symbols *quote.SymbolMap
	decoder CookerBodyDecoder
	onClose BarCloseHandler

	mu   sync.Mutex
	bars map[barKey]*quote.Bar
}

type barKey struct {
	Pair session.Pair
	TF   quote.Timeframe
}

// NewAggregator 는 Aggregator 를 구성한다.
// symbols 는 필수, decoder/onClose 는 nil 허용 (no-op).
func NewAggregator(symbols *quote.SymbolMap, decoder CookerBodyDecoder, onClose BarCloseHandler) *Aggregator {
	if onClose == nil {
		onClose = func(*quote.Bar) {}
	}
	return &Aggregator{
		symbols: symbols,
		decoder: decoder,
		onClose: onClose,
		bars:    make(map[barKey]*quote.Bar),
	}
}

// OnTick 은 TickConsumer 인터페이스 구현. broker 에서 도착한 tick 1건 처리.
// decoder/symbols 미설정 또는 inactive 심볼은 drop.
func (a *Aggregator) OnTick(t *Tick) {
	if t == nil || a.decoder == nil {
		return
	}
	pair, active, found := a.symbols.Lookup(t.Symbol)
	if !found || !active {
		return
	}
	bid, ask, ok := a.decoder(t.Body)
	if !ok {
		return
	}
	q := quote.Quote{
		Pair: pair,
		Bid:  bid,
		Ask:  ask,
		TS:   t.Received,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, tf := range quote.AllTimeframes {
		a.applyTo(tf, q)
	}
}

// applyTo 는 caller 가 a.mu 를 보유한 상태에서 호출되어야 한다.
func (a *Aggregator) applyTo(tf quote.Timeframe, q quote.Quote) {
	key := barKey{Pair: q.Pair, TF: tf}
	cur, ok := a.bars[key]
	if !ok {
		a.bars[key] = quote.NewBar(tf, q)
		return
	}
	if cur.Contains(q.TS) {
		cur.Update(q)
		return
	}
	// 새 bucket — 이전 봉 close.
	cur.Close()
	a.onClose(cur)
	a.bars[key] = quote.NewBar(tf, q)
}

// Sweep 은 now 이전에 종료됐어야 할 봉을 close + 콜백 후 map 에서 제거한다.
// 다음 tick 이 들어오면 새 봉이 자연스럽게 생성된다 (gap = 빈 봉 안 만듦).
//
// tick 흐름이 끊긴 후에도 DB INSERT 가 적기에 발생하도록 보장.
func (a *Aggregator) Sweep(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	utc := now.UTC()
	for k, b := range a.bars {
		end := b.OpenedAt.Add(b.TF.Duration())
		if !utc.Before(end) {
			b.Close()
			a.onClose(b)
			delete(a.bars, k)
		}
	}
}

// RunSweeper 는 ctx 가 끝날 때까지 interval 마다 Sweep 을 호출한다.
// 권장 interval: 1초 (TF1s 의 1봉 주기와 일치).
func (a *Aggregator) RunSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.Sweep(now)
		}
	}
}

// OpenBars 는 현재 진행 중(아직 close 안 된)인 봉들의 snapshot.
// 모니터링 / 디버깅용. hot path 에서 호출하지 말 것.
func (a *Aggregator) OpenBars() []quote.Bar {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]quote.Bar, 0, len(a.bars))
	for _, b := range a.bars {
		out = append(out, *b)
	}
	return out
}
