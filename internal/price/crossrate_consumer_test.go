package price

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

type captureConsumer struct {
	mu    sync.Mutex
	ticks []*Tick
}

func (c *captureConsumer) OnTick(t *Tick) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticks = append(c.ticks, t)
}
func (c *captureConsumer) snapshot() []*Tick {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Tick, len(c.ticks))
	copy(out, c.ticks)
	return out
}
func (c *captureConsumer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ticks)
}

func mkLegTick(sym string, bid, ask float64) *Tick {
	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: sym, Bid: bid, Ask: ask, TS: time.Now(), Src: SourceBest,
	})
	return &Tick{Symbol: sym, Body: body, Source: SourceBest, Received: time.Now()}
}

// 픽스처: EUR/KRW = EUR/USD × USD/KRW (mul/mul, scale=1)
// 두 leg 모두 도착 후 cross emit 검증.
func TestCrossRate_BasicEmit(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{})
	cr.AddDownstream(cap)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})

	// 한 쪽 leg 만 — emit X.
	cr.OnTick(mkLegTick("EUR/USD", 1.0795, 1.0805))
	if cap.count() != 0 {
		t.Errorf("한쪽 leg 만으로 emit 됨: %d", cap.count())
	}
	if cr.Stats().SkippedMissingLeg != 1 {
		t.Errorf("SkippedMissingLeg = %d, want 1", cr.Stats().SkippedMissingLeg)
	}

	// debounce window 통과시키기 위해 대기.
	time.Sleep(15 * time.Millisecond)

	// 둘째 leg → emit.
	cr.OnTick(mkLegTick("USD/KRW", 1378.65, 1378.69))
	if cap.count() != 1 {
		t.Fatalf("emit count = %d, want 1", cap.count())
	}
	got := cap.snapshot()[0]
	if got.Source != SourceCross {
		t.Errorf("Source = %s, want CROSS", got.Source)
	}
	if got.Symbol != "EURKRW" {
		t.Errorf("Symbol = %s, want EURKRW (fallback reverse)", got.Symbol)
	}
	var env quote.JSONEnvelope
	_ = json.Unmarshal(got.Body, &env)
	wantBid := 1.0795 * 1378.65
	wantAsk := 1.0805 * 1378.69
	if !floatNear(env.Bid, wantBid) || !floatNear(env.Ask, wantAsk) {
		t.Errorf("cross bid/ask: %v/%v, want %v/%v", env.Bid, env.Ask, wantBid, wantAsk)
	}
}

// debounce — 같은 cross 가 짧은 시간 안 두 번 emit 시도 → 두 번째 skip.
func TestCrossRate_Debounce(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{DebounceWindow: 50 * time.Millisecond})
	cr.AddDownstream(cap)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	// 두 leg 준비.
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379)) // 1st emit
	if cap.count() != 1 {
		t.Fatalf("1st: count = %d, want 1", cap.count())
	}
	// debounce 안에 또 트리거.
	cr.OnTick(mkLegTick("USD/KRW", 1378.1, 1379.1))
	if cap.count() != 1 {
		t.Errorf("debounce 안에서 emit 됨: count = %d", cap.count())
	}
	if cr.Stats().SkippedDebounce == 0 {
		t.Error("SkippedDebounce = 0")
	}

	// debounce 지나고 다시.
	time.Sleep(60 * time.Millisecond)
	cr.OnTick(mkLegTick("USD/KRW", 1378.2, 1379.2))
	if cap.count() != 2 {
		t.Errorf("debounce 후: count = %d, want 2", cap.count())
	}
}

// stale leg — maxStaleness 지난 leg 가 있으면 emit X.
func TestCrossRate_StaleLeg(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{
		MaxStaleness:   50 * time.Millisecond,
		DebounceWindow: 1 * time.Millisecond,
	})
	cr.AddDownstream(cap)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	// EUR/USD 가 stale 되도록 대기.
	time.Sleep(80 * time.Millisecond)
	// USD/KRW 가 도착 — EUR/USD 가 stale 이라 skip.
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379))
	if cap.count() != 0 {
		t.Errorf("stale leg 인데 emit: count = %d", cap.count())
	}
	if cr.Stats().SkippedStale == 0 {
		t.Error("SkippedStale = 0")
	}
}

// 100JPY/KRW = USD/KRW / USD/JPY × 100. scale + div 검증.
func TestCrossRate_100JPYKRW(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{DebounceWindow: 1 * time.Millisecond})
	cr.AddDownstream(cap)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"JPY/KRW": {LegA: "USD/KRW", OpA: pricing.CrossOpMul, LegB: "USD/JPY", OpB: pricing.CrossOpDiv, Scale: 100},
	})
	cr.OnTick(mkLegTick("USD/KRW", 1378.40, 1378.90))
	cr.OnTick(mkLegTick("USD/JPY", 151.20, 151.45))

	if cap.count() != 1 {
		t.Fatalf("count = %d, want 1", cap.count())
	}
	var env quote.JSONEnvelope
	_ = json.Unmarshal(cap.snapshot()[0].Body, &env)
	// 시장적 합리성 — 100JPY/KRW ~ 910 근처.
	if env.Bid < 900 || env.Bid > 920 || env.Ask < 900 || env.Ask > 920 {
		t.Errorf("100JPY/KRW 결과 시장 밖: %v/%v", env.Bid, env.Ask)
	}
}

// ReplaceFormulas — formula 교체 시 동작 변경 + reverse index 재빌드.
func TestCrossRate_ReplaceFormulas(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{DebounceWindow: 1 * time.Millisecond})
	cr.AddDownstream(cap)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379))
	if cap.count() != 1 {
		t.Fatal("1st: 미emit")
	}

	// 새 formula — 다른 cross.
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"GBP/KRW": {LegA: "GBP/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	st := cr.Stats()
	if st.FormulaCount != 1 || st.LegKeysCount != 2 {
		t.Errorf("교체 후 stats: %+v", st)
	}
	// 옛 formula (EUR/KRW) 는 더 이상 매칭 X.
	time.Sleep(2 * time.Millisecond)
	cr.OnTick(mkLegTick("EUR/USD", 1.09, 1.091))
	if cap.count() != 1 {
		t.Errorf("교체 후 옛 cross 가 여전히 emit: %d", cap.count())
	}
}

// 자기 자신이 emit 한 cross 가 재진입하지 않는지 (loop 방어).
func TestCrossRate_NoReentry(t *testing.T) {
	cap := &captureConsumer{}
	cr := NewCrossRateConsumer(CrossRateOptions{DebounceWindow: 1 * time.Millisecond})
	cr.AddDownstream(cap)
	// 자기 자신을 downstream 으로도 추가 (loop trigger 시도).
	cr.AddDownstream(cr)
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379))
	// emit 1번만 — 재진입 시 Source=CROSS 라 skip.
	if cap.count() != 1 {
		t.Errorf("재진입 발생: count = %d", cap.count())
	}
}

// LatestCross 의 isStale 플래그 — emit 직후 false, maxStaleness 지나면 true.
// 운영 시 forward-snapshot 등 외부 호출자가 stale 한 cross 호가를 거부 가능.
func TestCrossRate_LatestCrossStaleFlag(t *testing.T) {
	cr := NewCrossRateConsumer(CrossRateOptions{
		MaxStaleness:   30 * time.Millisecond,
		DebounceWindow: 1 * time.Millisecond,
	})
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
	})
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379))

	// 직후 — fresh
	_, _, _, isStale, ok := cr.LatestCross("EUR/KRW")
	if !ok {
		t.Fatalf("LatestCross ok=false right after emit")
	}
	if isStale {
		t.Errorf("emit 직후 isStale=true (예상 false)")
	}

	// maxStaleness 지나면 isStale=true 가 되어야 (재 emit 없어도 ts 가 옛 것)
	time.Sleep(60 * time.Millisecond)
	_, _, _, isStale2, ok2 := cr.LatestCross("EUR/KRW")
	if !ok2 {
		t.Fatalf("LatestCross ok=false after delay (lastEmits 캐시는 유지되어야)")
	}
	if !isStale2 {
		t.Errorf("60ms 후 isStale=false (예상 true — maxStaleness=30ms)")
	}
}

// 등록 안 된 pair 는 ok=false.
func TestCrossRate_LatestCrossMissing(t *testing.T) {
	cr := NewCrossRateConsumer(CrossRateOptions{})
	_, _, _, isStale, ok := cr.LatestCross("USD/UNKNOWN")
	if ok {
		t.Errorf("등록 안 된 pair 에 ok=true")
	}
	if isStale {
		t.Errorf("ok=false 인데 isStale=true")
	}
}

// CrossRateStats 의 LastEmitsTotal / LastEmitsStale — 운영 가시성.
// 등록된 cross pair 가 lastEmits 에 얼마나 있고 그중 stale 비율은 얼마.
func TestCrossRate_StatsLastEmitsSnapshot(t *testing.T) {
	cr := NewCrossRateConsumer(CrossRateOptions{
		MaxStaleness:   30 * time.Millisecond,
		DebounceWindow: 1 * time.Millisecond,
	})
	cr.AddDownstream(&captureConsumer{})
	cr.ReplaceFormulas(map[session.Pair]pricing.CrossFormula{
		"EUR/KRW": {LegA: "EUR/USD", OpA: pricing.CrossOpMul, LegB: "USD/KRW", OpB: pricing.CrossOpMul, Scale: 1},
		"JPY/KRW": {LegA: "USD/KRW", OpA: pricing.CrossOpMul, LegB: "USD/JPY", OpB: pricing.CrossOpDiv, Scale: 100},
	})

	// EUR/KRW emit
	cr.OnTick(mkLegTick("EUR/USD", 1.08, 1.081))
	cr.OnTick(mkLegTick("USD/KRW", 1378, 1379))
	// JPY/KRW emit
	cr.OnTick(mkLegTick("USD/JPY", 151.2, 151.45))

	st := cr.Stats()
	if st.LastEmitsTotal != 2 {
		t.Errorf("LastEmitsTotal=%d, want 2 (EUR/KRW + JPY/KRW)", st.LastEmitsTotal)
	}
	if st.LastEmitsStale != 0 {
		t.Errorf("emit 직후 LastEmitsStale=%d, want 0", st.LastEmitsStale)
	}

	// 두 cross 모두 stale 되도록 대기
	time.Sleep(60 * time.Millisecond)
	st2 := cr.Stats()
	if st2.LastEmitsStale != 2 {
		t.Errorf("60ms 후 LastEmitsStale=%d, want 2 (둘 다 maxStaleness=30ms 초과)", st2.LastEmitsStale)
	}
}

// floatNear 는 다른 파일에 정의되어 있음 (pricing_consumer_test.go).
