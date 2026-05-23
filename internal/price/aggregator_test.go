package price

import (
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// 테스트용 decoder: body 는 사용하지 않고 fixed bid/ask 반환.
func staticDecoder(bid, ask float64) CookerBodyDecoder {
	return func(_ []byte) (float64, float64, bool) {
		return bid, ask, true
	}
}

func newSymMap() *quote.SymbolMap {
	m := quote.NewSymbolMap()
	m.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
		{Symbol: "JPYKRW", Pair: "JPY/KRW", Active: false}, // 정지
	})
	return m
}

func mkTick(symbol string, ts time.Time) *Tick {
	return &Tick{Symbol: symbol, Received: ts}
}

func TestAggregator_OpenAndUpdate(t *testing.T) {
	closed := []*quote.Bar{}
	var mu sync.Mutex
	onClose := func(b *quote.Bar) {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, b)
	}

	agg := NewAggregator(newSymMap(), staticDecoder(100, 100.1), onClose)

	// 같은 1초 안의 tick 3개 → 어느 timeframe 도 close 안 됨.
	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	agg.OnTick(mkTick("USDKRW", t0))
	agg.OnTick(mkTick("USDKRW", t0.Add(200*time.Millisecond)))
	agg.OnTick(mkTick("USDKRW", t0.Add(800*time.Millisecond)))

	mu.Lock()
	gotClosed := len(closed)
	mu.Unlock()
	if gotClosed != 0 {
		t.Errorf("같은 초 내 tick 만 있을 때 close 발생: closed=%d", gotClosed)
	}

	// 진행 중 봉 6개 (AllTimeframes).
	open := agg.OpenBars()
	if len(open) != len(quote.AllTimeframes) {
		t.Errorf("OpenBars len = %d, want %d", len(open), len(quote.AllTimeframes))
	}
}

func TestAggregator_NewBucketClosesOldBar(t *testing.T) {
	closed := []*quote.Bar{}
	var mu sync.Mutex
	onClose := func(b *quote.Bar) {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, b)
	}

	agg := NewAggregator(newSymMap(), staticDecoder(100, 100.1), onClose)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	agg.OnTick(mkTick("USDKRW", t0))

	// 1m bucket 을 넘는 tick → 1m 봉 close.
	t1 := time.Date(2026, 5, 23, 12, 35, 1, 0, time.UTC)
	agg.OnTick(mkTick("USDKRW", t1))

	mu.Lock()
	gotClosed := closed
	mu.Unlock()

	// 1m, 1s 봉이 close 되었는지 검사. (1s 봉은 t0 + 1s 부터 새로 시작)
	closedTFs := map[quote.Timeframe]bool{}
	for _, b := range gotClosed {
		closedTFs[b.TF] = true
		if !b.ClosedAt.After(b.OpenedAt) {
			t.Errorf("ClosedAt(%v) > OpenedAt(%v) 여야 함", b.ClosedAt, b.OpenedAt)
		}
	}
	if !closedTFs[quote.TF1m] {
		t.Error("TF1m 봉이 close 되지 않음")
	}
	if !closedTFs[quote.TF1s] {
		t.Error("TF1s 봉이 close 되지 않음")
	}
	// 5m bucket [12:30,12:35) 였는데 t1=12:35:01 은 다음 bucket [12:35,12:40). close 되어야 함.
	if !closedTFs[quote.TF5m] {
		t.Error("TF5m 봉이 close 되지 않음")
	}
}

func TestAggregator_Sweep_ClosesStaleBars(t *testing.T) {
	closed := []*quote.Bar{}
	var mu sync.Mutex
	onClose := func(b *quote.Bar) {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, b)
	}

	agg := NewAggregator(newSymMap(), staticDecoder(100, 100.1), onClose)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	agg.OnTick(mkTick("USDKRW", t0))

	// 한참 후 (15분 뒤) sweep — 1s/1m/5m/15m 봉 close, 1h/1d 는 아직.
	agg.Sweep(t0.Add(15*time.Minute + time.Second))

	mu.Lock()
	gotClosed := closed
	mu.Unlock()

	closedTFs := map[quote.Timeframe]bool{}
	for _, b := range gotClosed {
		closedTFs[b.TF] = true
	}
	for _, tf := range []quote.Timeframe{quote.TF1s, quote.TF1m, quote.TF5m, quote.TF15m} {
		if !closedTFs[tf] {
			t.Errorf("%s 봉이 sweep 후 close 되지 않음", tf)
		}
	}
	// 1h 봉은 [12:00, 13:00) 이고 sweep 시점이 12:49 → 아직 진행 중.
	if closedTFs[quote.TF1h] {
		t.Error("TF1h 봉이 너무 일찍 close 됨")
	}
}

func TestAggregator_InactiveSymbolDropped(t *testing.T) {
	called := 0
	onClose := func(*quote.Bar) { called++ }
	agg := NewAggregator(newSymMap(), staticDecoder(100, 100.1), onClose)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	agg.OnTick(mkTick("JPYKRW", t0))           // inactive
	agg.OnTick(mkTick("XAUUSD", t0))           // 미등록
	agg.OnTick(mkTick("USDKRW", t0.Add(5*time.Minute))) // active

	open := agg.OpenBars()
	for _, b := range open {
		if b.Pair != "USD/KRW" {
			t.Errorf("inactive/미등록 심볼이 봉 생성: %s", b.Pair)
		}
	}
}

func TestAggregator_DecoderFalseDrops(t *testing.T) {
	agg := NewAggregator(
		newSymMap(),
		func(_ []byte) (float64, float64, bool) { return 0, 0, false },
		nil,
	)
	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	agg.OnTick(mkTick("USDKRW", t0))
	if got := len(agg.OpenBars()); got != 0 {
		t.Errorf("decoder false 인데 봉이 생성됨: %d", got)
	}
}

func TestAggregator_OHLCAccuracy(t *testing.T) {
	closed := []*quote.Bar{}
	var mu sync.Mutex
	onClose := func(b *quote.Bar) {
		mu.Lock()
		defer mu.Unlock()
		if b.TF == quote.TF1m {
			closed = append(closed, b)
		}
	}

	bid, ask := 100.0, 100.1
	var bidPtr, askPtr = &bid, &ask
	decoder := func(_ []byte) (float64, float64, bool) {
		return *bidPtr, *askPtr, true
	}

	agg := NewAggregator(newSymMap(), decoder, onClose)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	for i, v := range []float64{100, 105, 95, 102} {
		bid = v
		ask = v + 0.1
		agg.OnTick(mkTick("USDKRW", t0.Add(time.Duration(i*10)*time.Second)))
	}
	// 다음 bucket tick → 1m 봉 close.
	bid = 110
	ask = 110.1
	agg.OnTick(mkTick("USDKRW", t0.Add(time.Minute+time.Second)))

	mu.Lock()
	gotClosed := closed
	mu.Unlock()
	if len(gotClosed) != 1 {
		t.Fatalf("TF1m 봉 close 횟수 = %d, want 1", len(gotClosed))
	}
	b := gotClosed[0]
	if b.OpenBid != 100 || b.HighBid != 105 || b.LowBid != 95 || b.CloseBid != 102 {
		t.Errorf("OHLC bid mismatch: O=%v H=%v L=%v C=%v", b.OpenBid, b.HighBid, b.LowBid, b.CloseBid)
	}
	if b.TickCount != 4 {
		t.Errorf("TickCount = %d, want 4", b.TickCount)
	}
}
