package price

import (
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

func TestJSONCookerDecoder_ValidEnvelope(t *testing.T) {
	dec := JSONCookerDecoder()
	body := []byte(`{"sym":"USDKRW","bid":1399.5,"ask":1399.6,"ts":"2026-05-23T00:00:00Z"}`)
	bid, ask, ok := dec(body)
	if !ok {
		t.Fatal("정상 envelope 인데 ok=false")
	}
	if bid != 1399.5 || ask != 1399.6 {
		t.Errorf("bid/ask = %v/%v", bid, ask)
	}
}

func TestJSONCookerDecoder_DropsMalformed(t *testing.T) {
	dec := JSONCookerDecoder()
	tests := []struct {
		name string
		body []byte
	}{
		{"빈 body", nil},
		{"빈 JSON", []byte(`{}`)},
		{"ask < bid", []byte(`{"sym":"USDKRW","bid":1400,"ask":1399,"ts":"2026-05-23T00:00:00Z"}`)},
		{"sym 누락", []byte(`{"bid":1,"ask":2,"ts":"2026-05-23T00:00:00Z"}`)},
		{"손상 JSON", []byte(`not json`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := dec(tc.body); ok {
				t.Error("ok=true (drop 되어야 함)")
			}
		})
	}
}

// Aggregator 에 JSONCookerDecoder 를 끼워 end-to-end OHLC 누적이 동작하는지 검증.
func TestAggregator_WithJSONCookerDecoder_EndToEnd(t *testing.T) {
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
	})

	closed := []*quote.Bar{}
	var mu sync.Mutex
	onClose := func(b *quote.Bar) {
		mu.Lock()
		defer mu.Unlock()
		if b.TF == quote.TF1m {
			closed = append(closed, b)
		}
	}

	agg := NewAggregator(syms, JSONCookerDecoder(), onClose)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)

	mkTickJSON := func(sym string, bid, ask float64, ts time.Time) *Tick {
		body, _ := quote.EncodeJSONEnvelope(quote.JSONEnvelope{
			Sym: sym, Bid: bid, Ask: ask, TS: ts,
		})
		return &Tick{Symbol: sym, Body: body, Received: ts}
	}

	// 1m bucket [12:34:00, 12:35:00) 안에 4 tick.
	agg.OnTick(mkTickJSON("USDKRW", 100.0, 100.10, t0))
	agg.OnTick(mkTickJSON("USDKRW", 105.0, 105.10, t0.Add(15*time.Second)))
	agg.OnTick(mkTickJSON("USDKRW", 95.0, 95.10, t0.Add(30*time.Second)))
	agg.OnTick(mkTickJSON("USDKRW", 102.0, 102.10, t0.Add(45*time.Second)))

	// 다음 bucket → 1m 봉 close.
	agg.OnTick(mkTickJSON("USDKRW", 110.0, 110.10, t0.Add(time.Minute+time.Second)))

	mu.Lock()
	defer mu.Unlock()
	if len(closed) != 1 {
		t.Fatalf("1m 봉 close 수 = %d, want 1", len(closed))
	}
	b := closed[0]
	if b.OpenBid != 100.0 || b.HighBid != 105.0 || b.LowBid != 95.0 || b.CloseBid != 102.0 {
		t.Errorf("OHLC bid: O=%v H=%v L=%v C=%v", b.OpenBid, b.HighBid, b.LowBid, b.CloseBid)
	}
	if b.HighAsk != 105.10 || b.LowAsk != 95.10 {
		t.Errorf("OHLC ask: H=%v L=%v", b.HighAsk, b.LowAsk)
	}
	if b.TickCount != 4 {
		t.Errorf("TickCount = %d, want 4", b.TickCount)
	}
}

// JSON envelope 의 손상 / 검증실패 tick 은 봉 누적에 영향 없어야 한다.
func TestAggregator_WithJSONCookerDecoder_DropsBadTicks(t *testing.T) {
	syms := quote.NewSymbolMap()
	syms.Replace([]quote.SymbolEntry{
		{Symbol: "USDKRW", Pair: "USD/KRW", Active: true},
	})

	agg := NewAggregator(syms, JSONCookerDecoder(), nil)

	t0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)

	// 정상 1건.
	good, _ := quote.EncodeJSONEnvelope(quote.JSONEnvelope{Sym: "USDKRW", Bid: 100, Ask: 100.1, TS: t0})
	agg.OnTick(&Tick{Symbol: "USDKRW", Body: good, Received: t0})

	// 손상 3건.
	agg.OnTick(&Tick{Symbol: "USDKRW", Body: []byte(`not json`), Received: t0})
	agg.OnTick(&Tick{Symbol: "USDKRW", Body: []byte(`{"sym":"USDKRW","bid":1,"ask":0,"ts":"2026-05-23T00:00:00Z"}`), Received: t0})
	agg.OnTick(&Tick{Symbol: "USDKRW", Body: nil, Received: t0})

	open := agg.OpenBars()
	for _, b := range open {
		if b.TF == quote.TF1m {
			if b.TickCount != 1 {
				t.Errorf("손상 tick 무시 안 됨: TickCount=%d, want 1", b.TickCount)
			}
		}
	}
}
