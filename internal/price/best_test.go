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

// buildRawWithLast — bid/ask + 체결가(last) 동반 raw Tick.
func buildRawWithLast(sym, source string, bid, ask, last, lastQty float64) *Tick {
	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: sym, Bid: bid, Ask: ask, Src: source, TS: time.Now().UTC(),
		Last: last, LastQty: lastQty,
	})
	return &Tick{Symbol: sym, Source: source, Body: body, Received: time.Now()}
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

// 체결가(last)가 best envelope 로 전달되고, 이후 체결 없는 tick 에도 최근값이
// 유지된다 (mds MDFOLD.fillprc 모델 — BestConsumer 가 per-symbol persist).
func TestBestConsumer_CarriesAndPersistsLast(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)

	// 1) 체결가 동반 tick → best 에 last 실림.
	bc.OnTick(buildRawWithLast("USDKRW", "SMB", 1380.00, 1380.10, 1380.05, 500000))
	// 2) 체결 없는 tick (last=0) 이지만 bid/ask 변동 → best 는 최근 체결가 유지.
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.01, 1380.11))

	ticks := c.snapshot()
	if len(ticks) < 2 {
		t.Fatalf("emit %d개, want >=2", len(ticks))
	}
	e0 := decodeBest(t, ticks[0])
	if e0.Last != 1380.05 || e0.LastQty != 500000 {
		t.Errorf("첫 best last=%v qty=%v, want 1380.05/500000", e0.Last, e0.LastQty)
	}
	e1 := decodeBest(t, ticks[1])
	if e1.Last != 1380.05 {
		t.Errorf("체결 없는 tick 의 best last=%v, want 최근값 1380.05 유지", e1.Last)
	}
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

// Stats.Sources — active feed 이름이 정렬된 list 로 노출되는지.
// 운영자가 "SMB 가 들어왔는지" 를 카운트가 아닌 이름으로 직접 판정.
func TestBestConsumer_StatsSources(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	// 의도적으로 비-알파벳 순서로 ingest — 정렬 보장 확인용.
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.05, 1380.12))
	bc.OnTick(buildRaw("USDKRW", "EBS", 1380.02, 1380.11))

	st := bc.Stats().Symbols["USDKRW"]
	want := []string{"EBS", "KMB", "SMB"}
	if len(st.Sources) != len(want) {
		t.Fatalf("Sources len=%d, want %d (%v)", len(st.Sources), len(want), st.Sources)
	}
	for i, s := range want {
		if st.Sources[i] != s {
			t.Errorf("Sources[%d]=%q, want %q (정렬 안 됨? 전체=%v)", i, st.Sources[i], s, st.Sources)
		}
	}
	if st.ActiveSources != len(want) {
		t.Errorf("ActiveSources=%d, want %d (Sources 와 동일해야)", st.ActiveSources, len(want))
	}
}

// Stats.SourceQuotes — per-source bid/ask/ts 값이 정확히 노출되는지.
// mds W9501S02 (거래소별 호가 조회) 백엔드로 cside/wtgquery 가 사용.
func TestBestConsumer_StatsSourceQuotes(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1378.65, 1378.69))
	bc.OnTick(buildRaw("USDKRW", "KMB", 1378.60, 1378.72))

	st := bc.Stats().Symbols["USDKRW"]
	if len(st.SourceQuotes) != 2 {
		t.Fatalf("SourceQuotes len=%d, want 2 (%v)", len(st.SourceQuotes), st.SourceQuotes)
	}
	smb, ok := st.SourceQuotes["SMB"]
	if !ok {
		t.Fatalf("SourceQuotes[SMB] 누락")
	}
	if smb.Bid != 1378.65 || smb.Ask != 1378.69 {
		t.Errorf("SMB bid/ask = %v/%v, want 1378.65/1378.69", smb.Bid, smb.Ask)
	}
	if smb.TS.IsZero() {
		t.Errorf("SMB TS zero — 수신 시각 안 채워짐")
	}
	kmb, ok := st.SourceQuotes["KMB"]
	if !ok {
		t.Fatalf("SourceQuotes[KMB] 누락")
	}
	if kmb.Bid != 1378.60 || kmb.Ask != 1378.72 {
		t.Errorf("KMB bid/ask = %v/%v, want 1378.60/1378.72", kmb.Bid, kmb.Ask)
	}
}

// Stats.Sources — stale 된 feed 는 이름에서도 제외되어야.
func TestBestConsumer_StatsSourcesExcludesStale(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{MaxStaleness: 30 * time.Millisecond}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	time.Sleep(50 * time.Millisecond)
	bc.OnTick(buildRaw("USDKRW", "KMB", 1380.05, 1380.12))

	st := bc.Stats().Symbols["USDKRW"]
	if len(st.Sources) != 1 || st.Sources[0] != "KMB" {
		t.Errorf("Sources=%v, want [KMB] (SMB stale 제외)", st.Sources)
	}
	if st.ActiveSources != 1 {
		t.Errorf("ActiveSources=%d, want 1", st.ActiveSources)
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

// TestBestConsumer_DedupDisabled_NoDrop — Enabled=false (default) 이면 같은
// 가격도 매번 emit. 회귀 보호.
func TestBestConsumer_DedupDisabled_NoDrop(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{}, c)
	for i := 0; i < 5; i++ {
		bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))
	}
	if got := len(c.snapshot()); got != 5 {
		t.Fatalf("dedup disabled — emit 수=%d, want 5", got)
	}
	st := bc.Stats().Dedup
	if st.Enabled || st.DroppedSamePrice != 0 || st.DroppedBelowTick != 0 {
		t.Errorf("dedup 카운터 불일치: %+v", st)
	}
}

// TestBestConsumer_Dedup_ExactMatchSkip — Enabled + Multiplier=0 이면 완전 동일
// 값만 skip. 첫 emit 은 무조건 통과.
func TestBestConsumer_Dedup_ExactMatchSkip(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{
		Dedup: DedupOptions{Enabled: true, TickSizeMultiplier: 0},
	}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10)) // emit
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10)) // skip (same)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.01, 1380.10)) // emit (bid 다름)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.01, 1380.10)) // skip

	if got := len(c.snapshot()); got != 2 {
		t.Fatalf("emit 수=%d, want 2 (2회 skip)", got)
	}
	st := bc.Stats().Dedup
	if st.DroppedSamePrice != 2 {
		t.Errorf("DroppedSamePrice=%d, want 2", st.DroppedSamePrice)
	}
	if st.Emitted != 2 {
		t.Errorf("Emitted=%d, want 2", st.Emitted)
	}
}

// TestBestConsumer_Dedup_BelowTickSkip — Multiplier=1.0. KRW 페어 tick_size=0.01.
// 0.01 미만 변화 skip.
func TestBestConsumer_Dedup_BelowTickSkip(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{
		Dedup: DedupOptions{Enabled: true, TickSizeMultiplier: 1.0},
	}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10))  // emit
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.001, 1380.10)) // skip — 0.001 < 0.01
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.005, 1380.10)) // skip — 0.005 < 0.01
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.02, 1380.10))  // emit — 0.02 ≥ 0.01

	if got := len(c.snapshot()); got != 2 {
		t.Fatalf("emit 수=%d, want 2", got)
	}
	st := bc.Stats().Dedup
	if st.DroppedBelowTick != 2 {
		t.Errorf("DroppedBelowTick=%d, want 2", st.DroppedBelowTick)
	}
}

// TestBestConsumer_Dedup_EurUsdTickSize — quote=USD → tick_size=0.0001.
// 4-decimal 필터 정확.
func TestBestConsumer_Dedup_EurUsdTickSize(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{
		Dedup: DedupOptions{Enabled: true, TickSizeMultiplier: 1.0},
	}, c)
	bc.OnTick(buildRaw("EURUSD", "SMB", 1.0850, 1.0852))   // emit
	bc.OnTick(buildRaw("EURUSD", "SMB", 1.08505, 1.08525)) // skip — 둘 다 <0.0001
	bc.OnTick(buildRaw("EURUSD", "SMB", 1.0860, 1.0862))   // emit — bid diff=0.001

	if got := len(c.snapshot()); got != 2 {
		st := bc.Stats().Dedup
		t.Fatalf("emit 수=%d, want 2, got=%d, dedup=%+v", got, got, st)
	}
}

// TestBestConsumer_Dedup_OverrideWins — TickSizeOverride 가 default 를 이김.
func TestBestConsumer_Dedup_OverrideWins(t *testing.T) {
	c := &collector{}
	bc := NewBestConsumer(BestOptions{
		Dedup: DedupOptions{
			Enabled:            true,
			TickSizeMultiplier: 1.0,
			TickSizeOverride:   map[string]float64{"USDKRW": 0.5}, // 매우 큰 tick
		},
	}, c)
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.00, 1380.10)) // emit
	bc.OnTick(buildRaw("USDKRW", "SMB", 1380.20, 1380.20)) // skip — 0.2 < 0.5

	if got := len(c.snapshot()); got != 1 {
		t.Fatalf("emit 수=%d, want 1", got)
	}
	if bc.Stats().Dedup.DroppedBelowTick != 1 {
		t.Errorf("DroppedBelowTick=%d, want 1", bc.Stats().Dedup.DroppedBelowTick)
	}
}
