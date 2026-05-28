package price

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// collector — downstream TickConsumer mock. 받은 Tick 을 슬라이스에 누적.
type collector struct {
	mu    sync.Mutex
	ticks []*Tick
}

func (c *collector) OnTick(t *Tick) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticks = append(c.ticks, t)
}

func (c *collector) snapshot() []*Tick {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Tick, len(c.ticks))
	copy(out, c.ticks)
	return out
}

// buildRaw — bid/ask + source 로 raw Tick 합성. BestConsumer 가 받는 입력.
func buildRaw(sym, source string, bid, ask float64) *Tick {
	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: sym, Bid: bid, Ask: ask, Src: source, TS: time.Now().UTC(),
	})
	return &Tick{
		Symbol:   sym,
		Source:   source,
		Body:     body,
		Received: time.Now(),
	}
}

// decodeBest — emit 된 best Tick 의 body 를 v1 envelope 로 파싱.
func decodeBest(t *testing.T, tick *Tick) quote.JSONEnvelope {
	t.Helper()
	env, err := quote.DecodeJSONEnvelope(tick.Body)
	if err != nil {
		t.Fatalf("best Tick body decode 실패: %v", err)
	}
	return env
}

func TestBestConsumer_TwoFeedsHigherBidLowerAsk(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	// SMB: bid 1380.00 / ask 1380.10
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	// KMB: bid 1380.05 (더 높음) / ask 1380.08 (더 낮음)
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.05, 1380.08))

	got := c.snapshot()
	if len(got) != 2 {
		t.Fatalf("downstream 호출 수=%d, want 2 (raw 입력 매번 emit)", len(got))
	}
	// 마지막 emit 의 best
	last := decodeBest(t, got[len(got)-1])
	if last.Bid != 1380.05 {
		t.Errorf("best bid=%v, want 1380.05 (max(1380.00, 1380.05))", last.Bid)
	}
	if last.Ask != 1380.08 {
		t.Errorf("best ask=%v, want 1380.08 (min(1380.10, 1380.08))", last.Ask)
	}
	if last.Sym != "USDKRW" {
		t.Errorf("sym=%q, want USDKRW", last.Sym)
	}
	if last.Src != SourceBest {
		t.Errorf("src=%q, want %q", last.Src, SourceBest)
	}
	if got[len(got)-1].Source != SourceBest {
		t.Errorf("Tick.Source=%q, want %q", got[len(got)-1].Source, SourceBest)
	}
}

func TestBestConsumer_SingleFeedPassthrough(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))

	got := c.snapshot()
	if len(got) != 1 {
		t.Fatalf("downstream 호출 수=%d, want 1", len(got))
	}
	env := decodeBest(t, got[0])
	if env.Bid != 1380.00 || env.Ask != 1380.10 {
		t.Errorf("single feed best 불일치: bid=%v ask=%v", env.Bid, env.Ask)
	}
}

func TestBestConsumer_StaleSourceExcluded(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{MaxStaleness: 50 * time.Millisecond}, c)

	// SMB 가 더 좋은 bid 를 등록
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.10, 1380.20))
	// 70ms 후 → SMB stale
	time.Sleep(70 * time.Millisecond)
	// KMB 가 더 낮은 bid 보고
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.00, 1380.30))

	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	if last.Bid != 1380.00 {
		t.Errorf("stale 제외 후 best bid=%v, want 1380.00 (SMB stale, KMB 만 active)", last.Bid)
	}
	if last.Ask != 1380.30 {
		t.Errorf("stale 제외 후 best ask=%v, want 1380.30", last.Ask)
	}
}

func TestBestConsumer_MissingSourceDropped(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	tick := buildRaw("USDKRW", "", 1380.00, 1380.10) // Source 빈값
	bc.OnTick(tick)

	if len(c.snapshot()) != 0 {
		t.Errorf("Source 빈 raw 가 best 까지 흐름 — drop 해야 함")
	}
}

func TestBestConsumer_InvalidBodyDropped(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	bc.OnTick(&Tick{Symbol: "USDKRW", Source: "SMB", Body: []byte("not json")})
	if len(c.snapshot()) != 0 {
		t.Errorf("invalid body 가 emit 됨 — drop 해야 함")
	}
}

func TestBestConsumer_DifferentSymbolsIndependent(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	bc.OnTick(buildRaw("EURUSD", "SMB", 1.0850, 1.0852))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.05, 1380.08))

	got := c.snapshot()
	if len(got) != 3 {
		t.Fatalf("호출 수=%d, want 3", len(got))
	}
	// 마지막 emit 은 USDKRW best (KMB 가 더 높은 bid / 더 낮은 ask)
	last := decodeBest(t, got[2])
	if last.Sym != "USDKRW" || last.Bid != 1380.05 || last.Ask != 1380.08 {
		t.Errorf("USDKRW best 불일치: %+v", last)
	}
	// 중간 emit (EURUSD 첫 등장) — single feed, EUR/USD 만
	mid := decodeBest(t, got[1])
	if mid.Sym != "EURUSD" || mid.Bid != 1.0850 || mid.Ask != 1.0852 {
		t.Errorf("EURUSD best 불일치: %+v", mid)
	}
}

func TestBestConsumer_FanOutToMultipleDownstream(t *testing.T) {
	c1, c2 := &collector{}, &collector{}
	bc := NewBestConsumer(BestOptions{}, c1, c2)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	if len(c1.snapshot()) != 1 || len(c2.snapshot()) != 1 {
		t.Errorf("downstream fan-out 실패: c1=%d c2=%d", len(c1.snapshot()), len(c2.snapshot()))
	}
}
