package price

import (
	"sync"
	"sync/atomic"
)

// Conflation 은 심볼별 latest Tick 만 유지하는 자료구조.
//
// 동작:
//   - producer (mymq Subscribe goroutine) 가 Update(tick) 호출 → 해당 심볼의
//     atomic.Pointer 를 swap. 이전 값은 자동으로 GC 대상.
//   - consumer (fanout / monitoring) 가 Latest(symbol) 호출 → 현재 latest 값.
//   - producer 폭주 시 consumer 가 못 따라가도 intermediate tick 자동 drop —
//     호가 시세는 latest 만 의미 있으므로 안전.
//
// 동시성:
//   - sync.Map 으로 심볼 추가는 lock-free, atomic.Pointer 로 값은 lock-free
//   - 단, Range 시점에 추가/삭제는 일관성 보장 안 됨 (snapshot 아님)
//
// 메트릭:
//   - totalUpdate: producer 의 총 update 횟수
//   - totalSwap:   같은 심볼이 다른 값으로 교체된 횟수 (drop 측정)
//   - totalSeen:   고유 심볼 수 (atomic, 빠른 조회)
type Conflation struct {
	bySymbol sync.Map // map[string]*atomic.Pointer[Tick]

	totalUpdate atomic.Uint64
	totalSwap   atomic.Uint64
	totalSeen   atomic.Uint64
}

// NewConflation 은 빈 Conflation 을 생성한다.
func NewConflation() *Conflation {
	return &Conflation{}
}

// Update 는 새 Tick 을 등록한다. 동일 심볼의 이전 값은 swap 으로 대체된다.
// 반환값: previous Tick (없으면 nil), 즉 conflation drop 측정용.
func (c *Conflation) Update(t *Tick) *Tick {
	if t == nil {
		return nil
	}
	c.totalUpdate.Add(1)

	// 빠른 경로: 이미 등록된 심볼이면 atomic swap.
	if v, ok := c.bySymbol.Load(t.Symbol); ok {
		p := v.(*atomic.Pointer[Tick])
		prev := p.Swap(t)
		if prev != nil {
			c.totalSwap.Add(1)
		}
		return prev
	}

	// 느린 경로: 신규 심볼.
	p := &atomic.Pointer[Tick]{}
	p.Store(t)
	if actual, loaded := c.bySymbol.LoadOrStore(t.Symbol, p); loaded {
		// 다른 goroutine 이 동시에 등록 → race 회피.
		ap := actual.(*atomic.Pointer[Tick])
		prev := ap.Swap(t)
		if prev != nil {
			c.totalSwap.Add(1)
		}
		return prev
	}
	c.totalSeen.Add(1)
	return nil
}

// Latest 는 해당 심볼의 현재 latest Tick 을 반환 (없으면 nil).
func (c *Conflation) Latest(symbol string) *Tick {
	v, ok := c.bySymbol.Load(symbol)
	if !ok {
		return nil
	}
	return v.(*atomic.Pointer[Tick]).Load()
}

// Range 는 모든 (심볼, latest Tick) 쌍을 순회한다 (스냅샷 보장 X).
// fn 이 false 반환 시 순회 중단.
func (c *Conflation) Range(fn func(symbol string, tick *Tick) bool) {
	c.bySymbol.Range(func(k, v any) bool {
		t := v.(*atomic.Pointer[Tick]).Load()
		if t == nil {
			return true
		}
		return fn(k.(string), t)
	})
}

// SymbolCount 는 등록된 고유 심볼 수.
func (c *Conflation) SymbolCount() uint64 {
	return c.totalSeen.Load()
}

// Stats 는 모니터링용 누적 카운터.
type ConflationStats struct {
	Symbols uint64 // 등록된 고유 심볼 수
	Updates uint64 // 총 Update 호출 수
	Swaps   uint64 // 동일 심볼 swap 횟수 (= conflation drop intermediate)
}

// Stats 는 누적 카운터 snapshot.
func (c *Conflation) Stats() ConflationStats {
	return ConflationStats{
		Symbols: c.totalSeen.Load(),
		Updates: c.totalUpdate.Load(),
		Swaps:   c.totalSwap.Load(),
	}
}
