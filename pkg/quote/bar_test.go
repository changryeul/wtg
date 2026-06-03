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
		{TF30s, 30 * time.Second},
		{TF1m, time.Minute},
		{TF5m, 5 * time.Minute},
		{TF15m, 15 * time.Minute},
		{TF1h, time.Hour},
		{TF4h, 4 * time.Hour},
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
	if TF30s.Persistent() {
		t.Error("TF30s 가 Persistent=true (30s 는 메모리 전용 — 1분 미만)")
	}
	if !TF1m.Persistent() {
		t.Error("TF1m 이 Persistent=false")
	}
	if !TF4h.Persistent() {
		t.Error("TF4h 가 Persistent=false")
	}
	if !TF1d.Persistent() {
		t.Error("TF1d 가 Persistent=false")
	}
	if Timeframe("bogus").Persistent() {
		t.Error("미지원 TF 가 Persistent=true")
	}
}

// TF30s / TF4h 의 BucketStart 가 UTC 기준으로 정확히 정렬되는지.
func TestTimeframe_BucketStart_NewTFs(t *testing.T) {
	// 2026-05-23T12:34:56.789Z
	ts := time.Date(2026, 5, 23, 12, 34, 56, 789_000_000, time.UTC)
	// TF30s: 12:34:30 (30s grid)
	if got, want := TF30s.BucketStart(ts), time.Date(2026, 5, 23, 12, 34, 30, 0, time.UTC); !got.Equal(want) {
		t.Errorf("TF30s BucketStart = %v, want %v", got, want)
	}
	// TF4h: 12:00 (4h grid: 00/04/08/12/16/20)
	if got, want := TF4h.BucketStart(ts), time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("TF4h BucketStart = %v, want %v", got, want)
	}
	// TF4h boundary: 16:00 → 16:00, 15:59:59 → 12:00
	ts2 := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	if got, want := TF4h.BucketStart(ts2), ts2; !got.Equal(want) {
		t.Errorf("TF4h 16:00 BucketStart = %v, want %v", got, want)
	}
	ts3 := time.Date(2026, 5, 23, 15, 59, 59, 0, time.UTC)
	if got, want := TF4h.BucketStart(ts3), time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("TF4h 15:59:59 BucketStart = %v, want %v", got, want)
	}
}

// AllTimeframes 가 TF30s / TF4h 포함 + 짧은→긴 순서 정렬.
func TestAllTimeframes_OrderingIncludesNewTFs(t *testing.T) {
	want := []Timeframe{TF1s, TF30s, TF1m, TF5m, TF15m, TF1h, TF4h, TF1d}
	if len(AllTimeframes) != len(want) {
		t.Fatalf("AllTimeframes len=%d, want %d", len(AllTimeframes), len(want))
	}
	for i, tf := range AllTimeframes {
		if tf != want[i] {
			t.Errorf("AllTimeframes[%d]=%s, want %s", i, tf, want[i])
		}
	}
	// 짧은→긴 ordering 검증 — Duration 단조 증가.
	for i := 1; i < len(AllTimeframes); i++ {
		if AllTimeframes[i].Duration() <= AllTimeframes[i-1].Duration() {
			t.Errorf("AllTimeframes 순서 깨짐: %s (%v) ≤ %s (%v)",
				AllTimeframes[i], AllTimeframes[i].Duration(),
				AllTimeframes[i-1], AllTimeframes[i-1].Duration())
		}
	}
}

// PersistentTimeframes 가 1분 미만 (TF1s / TF30s) 제외.
func TestPersistentTimeframes_ExcludesSubMinute(t *testing.T) {
	for _, tf := range PersistentTimeframes {
		if tf == TF1s || tf == TF30s {
			t.Errorf("PersistentTimeframes 에 %s 포함됨 — 1분 미만은 메모리 전용", tf)
		}
		if !tf.Persistent() {
			t.Errorf("%s 는 PersistentTimeframes 에 있지만 Persistent()=false", tf)
		}
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
	if err := TF30s.Validate(); err != nil {
		t.Errorf("TF30s Validate: %v", err)
	}
	if err := TF4h.Validate(); err != nil {
		t.Errorf("TF4h Validate: %v", err)
	}
	if err := Timeframe("bogus").Validate(); err == nil {
		t.Error("미지원 TF 가 Validate 통과")
	}
}
