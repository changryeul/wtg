package price

import (
	"sync"
	"testing"
)

func TestConflationFirstUpdate(t *testing.T) {
	c := NewConflation()
	t1 := &Tick{Symbol: "USDKRW", SeqNum: 1}
	prev := c.Update(t1)
	if prev != nil {
		t.Errorf("최초 update prev 는 nil 이어야 함: %v", prev)
	}
	if got := c.Latest("USDKRW"); got != t1 {
		t.Errorf("Latest: %v", got)
	}
}

func TestConflationSwapDropsIntermediate(t *testing.T) {
	c := NewConflation()
	t1 := &Tick{Symbol: "USDKRW", SeqNum: 1}
	t2 := &Tick{Symbol: "USDKRW", SeqNum: 2}
	t3 := &Tick{Symbol: "USDKRW", SeqNum: 3}

	c.Update(t1)
	prev2 := c.Update(t2)
	prev3 := c.Update(t3)

	if prev2 != t1 {
		t.Errorf("swap 1→2: prev2=%v, want %v", prev2, t1)
	}
	if prev3 != t2 {
		t.Errorf("swap 2→3: prev3=%v, want %v", prev3, t2)
	}
	if c.Latest("USDKRW") != t3 {
		t.Errorf("Latest 는 t3 이어야 함")
	}
	stats := c.Stats()
	if stats.Updates != 3 {
		t.Errorf("Updates: %d, want 3", stats.Updates)
	}
	if stats.Swaps != 2 {
		t.Errorf("Swaps: %d, want 2 (intermediate drop 측정)", stats.Swaps)
	}
	if stats.Symbols != 1 {
		t.Errorf("Symbols: %d, want 1", stats.Symbols)
	}
}

func TestConflationMultipleSymbols(t *testing.T) {
	c := NewConflation()
	c.Update(&Tick{Symbol: "USDKRW", SeqNum: 1})
	c.Update(&Tick{Symbol: "EURUSD", SeqNum: 2})
	c.Update(&Tick{Symbol: "JPYUSD", SeqNum: 3})

	if c.SymbolCount() != 3 {
		t.Errorf("SymbolCount: %d, want 3", c.SymbolCount())
	}

	// Range 로 순회.
	seen := map[string]bool{}
	c.Range(func(symbol string, tick *Tick) bool {
		seen[symbol] = true
		return true
	})
	for _, s := range []string{"USDKRW", "EURUSD", "JPYUSD"} {
		if !seen[s] {
			t.Errorf("Range 에서 %q 누락", s)
		}
	}
}

func TestConflationLatestMissing(t *testing.T) {
	c := NewConflation()
	if got := c.Latest("ghost"); got != nil {
		t.Errorf("미등록 심볼은 nil: %v", got)
	}
}

func TestConflationNilUpdate(t *testing.T) {
	c := NewConflation()
	prev := c.Update(nil)
	if prev != nil || c.Stats().Updates != 0 {
		t.Errorf("nil update 는 무시: prev=%v stats=%+v", prev, c.Stats())
	}
}

func TestConflationConcurrentSwap(t *testing.T) {
	// 다수 goroutine 이 같은 심볼에 동시 update — race detector + 카운터 검증.
	c := NewConflation()
	const N = 10
	const M = 1000
	var wg sync.WaitGroup
	wg.Add(N)
	for g := 0; g < N; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < M; i++ {
				c.Update(&Tick{Symbol: "USDKRW", SeqNum: uint32(gid*M + i)})
			}
		}(g)
	}
	wg.Wait()

	stats := c.Stats()
	if stats.Updates != N*M {
		t.Errorf("Updates: %d, want %d", stats.Updates, N*M)
	}
	// Symbols 는 고유 심볼 수 = 1.
	if stats.Symbols != 1 {
		t.Errorf("Symbols: %d, want 1", stats.Symbols)
	}
	// Latest 는 어떤 값이든 USDKRW 있어야 함.
	if c.Latest("USDKRW") == nil {
		t.Error("Latest USDKRW nil")
	}
}

func TestConflationRangeStopsOnFalse(t *testing.T) {
	c := NewConflation()
	c.Update(&Tick{Symbol: "A"})
	c.Update(&Tick{Symbol: "B"})
	c.Update(&Tick{Symbol: "C"})

	count := 0
	c.Range(func(symbol string, tick *Tick) bool {
		count++
		return false // 첫 순회에서 즉시 중단
	})
	if count != 1 {
		t.Errorf("Range stop: count=%d, want 1", count)
	}
}
