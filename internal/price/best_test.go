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

func TestBestConsumer_CrossedFallsBackToNewest(t *testing.T) {
	// 두 feed 가 서로 분리된 가격대 → max(bid)/min(ask) 가 cross.
	// fallback 정책: 최신 ts 의 feed bid/ask 를 그대로 사용 (해당 feed
	// 는 자체 spread 가 유효하므로 cross 없음).
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	// SMB: bid 1380.60, ask 1380.62
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.60, 1380.62))
	// KMB: bid 1380.40, ask 1380.42 — 이게 더 늦은 ts (가장 최신)
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.40, 1380.42))

	// 정상 best 라면 best_bid=1380.60 (SMB), best_ask=1380.42 (KMB) → crossed!
	// fallback 으로 최신 feed (KMB) 값 그대로 emit 되어야.
	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	if last.Bid != 1380.40 || last.Ask != 1380.42 {
		t.Errorf("cross fallback 실패: got bid=%v ask=%v, want 1380.40/1380.42 (KMB 최신)", last.Bid, last.Ask)
	}
	// Stats 가 crossed 마커 노출
	st := bc.Stats()
	sym := st.Symbols["USDKRW"]
	if !sym.CrossedFallbck {
		t.Errorf("Stats.CrossedFallbck=false, want true")
	}
	if sym.ActiveSources != 2 {
		t.Errorf("Stats.ActiveSources=%d, want 2", sym.ActiveSources)
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

// 3 feeds 의 정상 best 산정 — max(bid) / min(ask) 가 서로 다른 feed.
func TestBestConsumer_ThreeFeedsNormal(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	// A: bid 1380.00 / ask 1380.20
	bc.OnTick(buildRaw("USDKRW", "A", 1380.00, 1380.20))
	// B: bid 1380.10 (max) / ask 1380.15
	bc.OnTick(buildRaw("USDKRW", "B", 1380.10, 1380.15))
	// C: bid 1380.05 / ask 1380.12 (min)
	bc.OnTick(buildRaw("USDKRW", "C", 1380.05, 1380.12))

	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	if last.Bid != 1380.10 {
		t.Errorf("3 feeds best bid=%v, want 1380.10 (B)", last.Bid)
	}
	if last.Ask != 1380.12 {
		t.Errorf("3 feeds best ask=%v, want 1380.12 (C)", last.Ask)
	}
	st := bc.Stats()
	if st.Symbols["USDKRW"].ActiveSources != 3 {
		t.Errorf("active_sources=%d, want 3", st.Symbols["USDKRW"].ActiveSources)
	}
	if st.Symbols["USDKRW"].CrossedFallbck {
		t.Errorf("crossed_fallback=true unexpectedly")
	}
}

// 3 feeds cross — 가장 최신 ts 의 feed 로 fallback 검증.
func TestBestConsumer_ThreeFeedsCrossedNewestWins(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	// A: 옛 가격대 (bid 높음)
	bc.OnTick(buildRaw("USDKRW", "A", 1380.60, 1380.65))
	time.Sleep(2 * time.Millisecond)
	// B: 다른 옛 가격대
	bc.OnTick(buildRaw("USDKRW", "B", 1380.55, 1380.58))
	time.Sleep(2 * time.Millisecond)
	// C: 더 낮은 가격대 (max(bid 60) > min(ask 12) → cross)
	bc.OnTick(buildRaw("USDKRW", "C", 1380.10, 1380.12))

	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	// fallback: 최신 feed C 의 (bid, ask)
	if last.Bid != 1380.10 || last.Ask != 1380.12 {
		t.Errorf("cross fallback 실패: bid=%v ask=%v, want 1380.10/1380.12 (C 최신)", last.Bid, last.Ask)
	}
	if !bc.Stats().Symbols["USDKRW"].CrossedFallbck {
		t.Errorf("Stats.CrossedFallbck=false, want true")
	}
}

// cross 발생 후 정정 tick 도착 → 정상 best 복귀.
// 운영 흐름의 가장 빈번한 패턴.
func TestBestConsumer_CrossResolvedByCorrectiveTick(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	// 1) cross 생성
	bc.OnTick(buildRaw("USDKRW", "A", 1380.60, 1380.62))
	bc.OnTick(buildRaw("USDKRW", "B", 1380.40, 1380.42))
	if !bc.Stats().Symbols["USDKRW"].CrossedFallbck {
		t.Fatalf("setup: cross 미발생")
	}
	// 2) A 가 정정된 quote 보내서 spread 정상화 (A 의 bid 가 B 의 ask 보다 낮음)
	bc.OnTick(buildRaw("USDKRW", "A", 1380.41, 1380.43))

	// 정상 best: max(bid)=1380.41 (A) / min(ask)=1380.42 (B)
	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	if last.Bid != 1380.41 {
		t.Errorf("cross 해소 후 bid=%v, want 1380.41 (A 정정)", last.Bid)
	}
	if last.Ask != 1380.42 {
		t.Errorf("cross 해소 후 ask=%v, want 1380.42 (B)", last.Ask)
	}
	if bc.Stats().Symbols["USDKRW"].CrossedFallbck {
		t.Errorf("cross 미해소 — Stats.CrossedFallbck=true")
	}
}

// stale 으로 한 feed 가 제외되면 cross 검출 조건 (srcCount>1) 미충족 →
// 단일 fresh feed 의 spread 그대로 emit (cross fallback 미발동).
func TestBestConsumer_StaleEliminatesCross(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{MaxStaleness: 30 * time.Millisecond}, c)
	// A 가 옛 quote — cross 유발할 가격대
	bc.OnTick(buildRaw("USDKRW", "A", 1380.60, 1380.62))
	time.Sleep(50 * time.Millisecond)
	// A stale. B 가 단독 fresh feed.
	bc.OnTick(buildRaw("USDKRW", "B", 1380.40, 1380.42))

	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	// B 단독: spread 일관 — cross 미발생, B 의 값 그대로.
	if last.Bid != 1380.40 || last.Ask != 1380.42 {
		t.Errorf("stale 후 single fresh feed: bid=%v ask=%v, want 1380.40/1380.42", last.Bid, last.Ask)
	}
	st := bc.Stats().Symbols["USDKRW"]
	if st.CrossedFallbck {
		t.Errorf("single fresh feed 인데 CrossedFallbck=true")
	}
	if st.ActiveSources != 1 {
		t.Errorf("active_sources=%d, want 1 (A stale)", st.ActiveSources)
	}
}

// MaxStaleness 음수 — 모든 quote 영구 active (stale 검사 비활성).
func TestBestConsumer_NegativeStalenessKeepsAll(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{MaxStaleness: -1}, c)
	bc.OnTick(buildRaw("USDKRW", "A", 1380.00, 1380.10))
	// 100ms — 일반적이면 stale 이지만 음수라 영구 유효
	time.Sleep(100 * time.Millisecond)
	bc.OnTick(buildRaw("USDKRW", "B", 1380.05, 1380.08))

	got := c.snapshot()
	last := decodeBest(t, got[len(got)-1])
	// 두 feed 모두 active → max(bid)=1380.05 / min(ask)=1380.08
	if last.Bid != 1380.05 || last.Ask != 1380.08 {
		t.Errorf("negative staleness: bid=%v ask=%v, want 1380.05/1380.08 (둘 다 active)", last.Bid, last.Ask)
	}
	if bc.Stats().Symbols["USDKRW"].ActiveSources != 2 {
		t.Errorf("active_sources=%d, want 2 (영구 유효)", bc.Stats().Symbols["USDKRW"].ActiveSources)
	}
}

// 자기 자신이 emit 한 SourceBest tick 을 다시 받으면 ignore — ring 방어.
func TestBestConsumer_IgnoresOwnBestSource(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(buildRaw("USDKRW", SourceBest, 1380.00, 1380.10))
	if len(c.snapshot()) != 0 {
		t.Errorf("SourceBest 입력이 emit 됨 — ring 방어 실패")
	}
}

// nil / 빈 Symbol drop — defensive path.
func TestBestConsumer_NilAndEmptySymbolDropped(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(nil)
	bc.OnTick(&Tick{Symbol: "", Source: "SMB", Body: []byte(`{}`)})
	if len(c.snapshot()) != 0 {
		t.Errorf("nil / 빈 symbol 이 emit 됨 — drop 해야 함")
	}
}

// Invariant — broken raw (bid<=0 / ask<=0 / bid>ask / missing sym) 는 decoder
// 가 ErrEnvelopeInvalidBidAsk 등으로 거부. BestConsumer 는 그 reject 를
// rejectedQuotes 카운터로 노출 — feed cooker / forwarder 데이터 sanity 진단.
//
// cache 는 정상 raw 만 받음 → broken 입력이 best 를 깎지 않음.
func TestBestConsumer_DecoderRejectIncrementsCounter(t *testing.T) {
	// buildRaw 는 무조건 marshal 하므로 invariant 위반 (bid>ask 등) raw 도
	// 만들 수 있지만, decoder 가 거부.
	cases := []struct {
		name string
		bid  float64
		ask  float64
	}{
		{"bid_zero", 0, 1380.10},
		{"ask_zero", 1380.05, 0},
		{"both_zero", 0, 0},
		{"bid_negative", -1, 1380.10},
		{"ask_negative", 1380.05, -1},
		{"bid_greater_than_ask", 1380.20, 1380.10}, // crossed single feed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &collector{}
			bc := NewBestConsumer(BestOptions{}, c)
			bc.OnTick(buildRaw("USDKRW", "SMB", tc.bid, tc.ask))
			if len(c.snapshot()) != 0 {
				t.Errorf("broken raw 가 downstream 으로 흘렀음: bid=%v ask=%v", tc.bid, tc.ask)
			}
			if bc.Stats().RejectedQuotes != 1 {
				t.Errorf("RejectedQuotes=%d, want 1", bc.Stats().RejectedQuotes)
			}
		})
	}
}

// 정상 raw 는 counter 증가 안 함.
func TestBestConsumer_ValidRawDoesNotIncrementReject(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	if len(c.snapshot()) != 1 {
		t.Errorf("정상 raw 가 reject 됨: snapshot=%d", len(c.snapshot()))
	}
	if bc.Stats().RejectedQuotes != 0 {
		t.Errorf("RejectedQuotes=%d, want 0", bc.Stats().RejectedQuotes)
	}
}

// 정상 feed 가 cache 에 있을 때 broken raw 가 들어와도 cache 오염 X.
func TestBestConsumer_BrokenRawDoesNotOverwriteCache(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	// 정상 입력
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	// 같은 source 가 broken (ask=0) — decoder reject → cache 덮어쓰지 않음.
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.05, 0))

	if len(c.snapshot()) != 1 {
		t.Errorf("snapshot=%d, want 1 (broken 후속은 reject)", len(c.snapshot()))
	}
	st := bc.Stats()
	if st.Symbols["USDKRW"].BestAsk != 1380.10 {
		t.Errorf("BestAsk=%v, want 1380.10 (broken 후속 무시)", st.Symbols["USDKRW"].BestAsk)
	}
	if st.RejectedQuotes != 1 {
		t.Errorf("RejectedQuotes=%d, want 1", st.RejectedQuotes)
	}
}

// not-json body 도 decoder reject — RejectedQuotes 카운트.
func TestBestConsumer_InvalidJSONRejectIncrementsCounter(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(&Tick{Symbol: "USDKRW", Source: "SMB", Body: []byte("not json")})
	if bc.Stats().RejectedQuotes != 1 {
		t.Errorf("RejectedQuotes=%d, want 1 (non-JSON body)", bc.Stats().RejectedQuotes)
	}
}

// Concurrent OnTick — race detector 로 데이터 race 검증.
//
//	go test -race -run TestBestConsumer_ConcurrentSafe ./internal/price/
func TestBestConsumer_ConcurrentSafe(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	var wg sync.WaitGroup
	const goroutines = 8
	const perG = 200
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			src := "F" + string(rune('A'+id))
			for i := 0; i < perG; i++ {
				bc.OnTick(buildRaw("USDKRW", src, 1380.0+float64(i%5)*0.01, 1380.1+float64(i%5)*0.01))
			}
		}(g)
	}
	wg.Wait()
	got := len(c.snapshot())
	if got != goroutines*perG {
		t.Errorf("emit 수=%d, want %d (모든 raw 입력 1:1 emit)", got, goroutines*perG)
	}
	// Stats 도 일관성 — symbol 1 개, sources 8 개.
	st := bc.Stats().Symbols["USDKRW"]
	if st.ActiveSources != goroutines {
		t.Errorf("active_sources=%d, want %d", st.ActiveSources, goroutines)
	}
}
