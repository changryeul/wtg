package quote

import (
	"testing"
	"time"
)

func TestTimeframe_Duration(t *testing.T) {
	tests := []struct {
		tf   Timeframe
		want time.Duration
	}{
		{TF1s, time.Second},
		{TF1m, time.Minute},
		{TF5m, 5 * time.Minute},
		{TF15m, 15 * time.Minute},
		{TF1h, time.Hour},
		{TF1d, 24 * time.Hour},
		{Timeframe("bogus"), 0},
	}
	for _, tc := range tests {
		t.Run(string(tc.tf), func(t *testing.T) {
			if got := tc.tf.Duration(); got != tc.want {
				t.Errorf("Duration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTimeframe_Persistent(t *testing.T) {
	if TF1s.Persistent() {
		t.Error("TF1s 가 Persistent=true (1s 는 메모리 전용)")
	}
	if !TF1m.Persistent() {
		t.Error("TF1m 이 Persistent=false")
	}
	if !TF1d.Persistent() {
		t.Error("TF1d 가 Persistent=false")
	}
	if Timeframe("bogus").Persistent() {
		t.Error("미지원 TF 가 Persistent=true")
	}
}

func TestTimeframe_BucketStart_UTC(t *testing.T) {
	// 2026-05-23T12:34:56.789Z
	ts := time.Date(2026, 5, 23, 12, 34, 56, 789_000_000, time.UTC)

	tests := []struct {
		tf   Timeframe
		want time.Time
	}{
		{TF1s, time.Date(2026, 5, 23, 12, 34, 56, 0, time.UTC)},
		{TF1m, time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)},
		{TF5m, time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC)},
		{TF15m, time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC)},
		{TF1h, time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)},
		{TF1d, time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(string(tc.tf), func(t *testing.T) {
			if got := tc.tf.BucketStart(ts); !got.Equal(tc.want) {
				t.Errorf("BucketStart = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTimeframe_BucketStart_FromOtherTZ(t *testing.T) {
	// KST (UTC+9) 으로 들어와도 UTC bucket 으로 정규화되어야 함.
	kst := time.FixedZone("KST", 9*60*60)
	ts := time.Date(2026, 5, 23, 21, 34, 56, 0, kst) // = 2026-05-23T12:34:56Z

	got := TF1h.BucketStart(ts)
	want := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("BucketStart = %v, want %v", got, want)
	}
}

func TestNewBar_OpensFromFirstTick(t *testing.T) {
	ts := time.Date(2026, 5, 23, 12, 34, 56, 0, time.UTC)
	q := Quote{Pair: "USD/KRW", Bid: 1399.50, Ask: 1399.60, TS: ts}

	bar := NewBar(TF1m, q)

	if bar.Pair != "USD/KRW" || bar.TF != TF1m {
		t.Errorf("Pair/TF mismatch: %+v", bar)
	}
	wantOpen := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	if !bar.OpenedAt.Equal(wantOpen) {
		t.Errorf("OpenedAt = %v, want %v", bar.OpenedAt, wantOpen)
	}
	if bar.OpenBid != 1399.50 || bar.HighBid != 1399.50 || bar.LowBid != 1399.50 || bar.CloseBid != 1399.50 {
		t.Errorf("초기 OHLC.Bid 모두 first.Bid 여야 함: %+v", bar)
	}
	if bar.TickCount != 1 {
		t.Errorf("TickCount = %d, want 1", bar.TickCount)
	}
}

func TestBar_UpdateAccumulates(t *testing.T) {
	ts0 := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	bar := NewBar(TF1m, Quote{Pair: "USD/KRW", Bid: 100, Ask: 100.1, TS: ts0})

	// 새 high
	bar.Update(Quote{Pair: "USD/KRW", Bid: 105, Ask: 105.1, TS: ts0.Add(10 * time.Second)})
	// 새 low
	bar.Update(Quote{Pair: "USD/KRW", Bid: 95, Ask: 95.1, TS: ts0.Add(20 * time.Second)})
	// close 만 갱신
	bar.Update(Quote{Pair: "USD/KRW", Bid: 102, Ask: 102.1, TS: ts0.Add(30 * time.Second)})

	if bar.HighBid != 105 {
		t.Errorf("HighBid = %v, want 105", bar.HighBid)
	}
	if bar.LowBid != 95 {
		t.Errorf("LowBid = %v, want 95", bar.LowBid)
	}
	if bar.OpenBid != 100 {
		t.Errorf("OpenBid = %v, want 100 (immutable)", bar.OpenBid)
	}
	if bar.CloseBid != 102 {
		t.Errorf("CloseBid = %v, want 102", bar.CloseBid)
	}
	if bar.HighAsk != 105.1 || bar.LowAsk != 95.1 || bar.CloseAsk != 102.1 {
		t.Errorf("Ask OHLC mismatch: high=%v low=%v close=%v", bar.HighAsk, bar.LowAsk, bar.CloseAsk)
	}
	if bar.TickCount != 4 {
		t.Errorf("TickCount = %d, want 4", bar.TickCount)
	}
}

func TestBar_CloseSetsClosedAt(t *testing.T) {
	ts := time.Date(2026, 5, 23, 12, 34, 0, 0, time.UTC)
	bar := NewBar(TF5m, Quote{TS: ts, Bid: 1, Ask: 1})

	bar.Close()

	// TF5m: OpenedAt 는 12:30, ClosedAt 는 12:35.
	wantOpen := time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC)
	wantClose := wantOpen.Add(5 * time.Minute)
	if !bar.OpenedAt.Equal(wantOpen) {
		t.Errorf("OpenedAt = %v, want %v", bar.OpenedAt, wantOpen)
	}
	if !bar.ClosedAt.Equal(wantClose) {
		t.Errorf("ClosedAt = %v, want %v", bar.ClosedAt, wantClose)
	}
}

func TestBar_Contains(t *testing.T) {
	ts := time.Date(2026, 5, 23, 12, 30, 0, 0, time.UTC)
	bar := NewBar(TF5m, Quote{TS: ts, Bid: 1, Ask: 1})

	// 봉 범위: [12:30, 12:35)
	in := time.Date(2026, 5, 23, 12, 34, 59, 0, time.UTC)
	out := time.Date(2026, 5, 23, 12, 35, 0, 0, time.UTC)
	before := time.Date(2026, 5, 23, 12, 29, 59, 0, time.UTC)

	if !bar.Contains(in) {
		t.Error("12:34:59 가 [12:30,12:35) 에 포함되지 않음")
	}
	if bar.Contains(out) {
		t.Error("12:35:00 이 봉 범위에 포함됨 (불포함이어야)")
	}
	if bar.Contains(before) {
		t.Error("12:29:59 가 봉 범위에 포함됨")
	}
}

func TestTimeframe_Validate(t *testing.T) {
	if err := TF1m.Validate(); err != nil {
		t.Errorf("TF1m Validate: %v", err)
	}
	if err := Timeframe("bogus").Validate(); err == nil {
		t.Error("미지원 TF 가 Validate 통과")
	}
}
