//go:build integration

package chart

import (
	"context"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/price"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/test/pgxtest"
)

// 실 TimescaleDB 에 봉을 INSERT 한 후 PgxRepository.QueryBars 로 round-trip 검증.
func TestPgxRepository_RoundTrip(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := price.NewPgxInserter(pool)
	repo := NewPgxRepository(pool)

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	const N = 10
	bars := make([]*quote.Bar, 0, N)
	for i := 0; i < N; i++ {
		open := t0.Add(time.Duration(i) * time.Minute)
		b := quote.NewBar(quote.TF1m, quote.Quote{
			Pair: "USD/KRW",
			Bid:  1399.0 + float64(i)*0.1,
			Ask:  1399.1 + float64(i)*0.1,
			TS:   open,
		})
		// 봉당 tick 1건 더 누적 → high/low/close 변화.
		b.Update(quote.Quote{
			Pair: "USD/KRW", Bid: 1400.0 + float64(i)*0.1, Ask: 1400.1 + float64(i)*0.1,
			TS: open.Add(30 * time.Second),
		})
		b.Close()
		bars = append(bars, b)
	}
	if err := ins.Insert(ctx, bars); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// 전체 범위 SELECT.
	got, err := repo.QueryBars(ctx, session.Pair("USD/KRW"), quote.TF1m,
		t0, t0.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("QueryBars: %v", err)
	}
	if len(got) != N {
		t.Fatalf("len = %d, want %d", len(got), N)
	}

	// ASC 정렬 + 값 round-trip 검증.
	for i, b := range got {
		want := bars[i]
		if !b.OpenedAt.Equal(want.OpenedAt) {
			t.Errorf("[%d] OpenedAt = %v, want %v", i, b.OpenedAt, want.OpenedAt)
		}
		if b.OpenBid != want.OpenBid || b.CloseAsk != want.CloseAsk {
			t.Errorf("[%d] OHLC mismatch: open_bid=%v close_ask=%v",
				i, b.OpenBid, b.CloseAsk)
		}
		if b.TickCount != want.TickCount {
			t.Errorf("[%d] TickCount = %d, want %d", i, b.TickCount, want.TickCount)
		}
		if b.Pair != session.Pair("USD/KRW") || b.TF != quote.TF1m {
			t.Errorf("[%d] Pair/TF: %s/%s", i, b.Pair, b.TF)
		}
	}
}

// 시간 범위 필터: [from, to) 경계 정확성.
func TestPgxRepository_TimeRangeBoundaries(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := price.NewPgxInserter(pool)
	repo := NewPgxRepository(pool)

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	bars := make([]*quote.Bar, 0, 5)
	for i := 0; i < 5; i++ {
		open := t0.Add(time.Duration(i) * time.Minute)
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1.1, TS: open})
		b.Close()
		bars = append(bars, b)
	}
	if err := ins.Insert(ctx, bars); err != nil {
		t.Fatal(err)
	}

	// [t0+1m, t0+4m): 3건 (1m, 2m, 3m).
	got, err := repo.QueryBars(ctx, "USD/KRW", quote.TF1m,
		t0.Add(time.Minute), t0.Add(4*time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3 (boundary [from, to))", len(got))
	}
	// to 정확히 일치하는 봉은 제외 (exclusive).
	for _, b := range got {
		if !b.OpenedAt.Before(t0.Add(4 * time.Minute)) {
			t.Errorf("to 경계 위반: OpenedAt = %v", b.OpenedAt)
		}
	}
}

// limit 정확성.
func TestPgxRepository_Limit(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := price.NewPgxInserter(pool)
	repo := NewPgxRepository(pool)

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	bars := make([]*quote.Bar, 0, 10)
	for i := 0; i < 10; i++ {
		open := t0.Add(time.Duration(i) * time.Minute)
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1.1, TS: open})
		b.Close()
		bars = append(bars, b)
	}
	if err := ins.Insert(ctx, bars); err != nil {
		t.Fatal(err)
	}

	got, err := repo.QueryBars(ctx, "USD/KRW", quote.TF1m, t0, t0.Add(time.Hour), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("limit=3 인데 len = %d", len(got))
	}
}

// 다른 pair / 다른 tf 가 섞여있어도 필터 정확.
func TestPgxRepository_FiltersByPairAndTF(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := price.NewPgxInserter(pool)
	repo := NewPgxRepository(pool)

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	mk := func(pair string, tf quote.Timeframe, offset time.Duration) *quote.Bar {
		b := quote.NewBar(tf, quote.Quote{Pair: session.Pair(pair), Bid: 1, Ask: 1.1, TS: t0.Add(offset)})
		b.Close()
		return b
	}
	bars := []*quote.Bar{
		mk("USD/KRW", quote.TF1m, 0),
		mk("USD/KRW", quote.TF5m, 0),
		mk("EUR/KRW", quote.TF1m, 0),
	}
	if err := ins.Insert(ctx, bars); err != nil {
		t.Fatal(err)
	}

	// USD/KRW + 1m → 1건.
	got, _ := repo.QueryBars(ctx, "USD/KRW", quote.TF1m, t0, t0.Add(time.Hour), 100)
	if len(got) != 1 {
		t.Errorf("USD/KRW 1m: len = %d, want 1", len(got))
	}
	// EUR/KRW + 5m → 0건.
	got, _ = repo.QueryBars(ctx, "EUR/KRW", quote.TF5m, t0, t0.Add(time.Hour), 100)
	if len(got) != 0 {
		t.Errorf("EUR/KRW 5m: len = %d, want 0", len(got))
	}
}

// 빈 결과 (no rows) 정상 처리.
func TestPgxRepository_EmptyResult(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := NewPgxRepository(pool)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	got, err := repo.QueryBars(ctx, "USD/KRW", quote.TF1m, t0, t0.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("빈 결과 err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
