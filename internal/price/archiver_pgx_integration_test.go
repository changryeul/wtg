//go:build integration

package price

import (
	"context"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/test/pgxtest"
)

func TestPgxInserter_InsertAndQuery(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := NewPgxInserter(pool)

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	bars := make([]*quote.Bar, 0, 5)
	for i := 0; i < 5; i++ {
		open := t0.Add(time.Duration(i) * time.Minute)
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1399.0 + float64(i), Ask: 1399.1 + float64(i), TS: open})
		b.Close()
		bars = append(bars, b)
	}

	if err := ins.Insert(ctx, bars); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// 직접 SELECT 로 5건 확인.
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM quote_bars WHERE pair=$1 AND tf=$2", "USD/KRW", "1m").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("INSERT 후 row 수 = %d, want 5", n)
	}
}

func TestPgxInserter_OnConflictDoNothing(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ins := NewPgxInserter(pool)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// 같은 (pair, tf, opened_at) 으로 두 번 INSERT.
	b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1399.5, Ask: 1399.6, TS: t0})
	b.Close()
	if err := ins.Insert(ctx, []*quote.Bar{b}); err != nil {
		t.Fatal(err)
	}
	// 두 번째 — 다른 값으로 덮어쓰기 시도.
	b2 := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 9999, Ask: 9999, TS: t0})
	b2.Close()
	if err := ins.Insert(ctx, []*quote.Bar{b2}); err != nil {
		t.Fatalf("두 번째 Insert (dedup): %v", err)
	}

	// row 1개 + 첫 번째 값 유지 확인.
	var (
		n   int
		bid float64
	)
	if err := pool.QueryRow(ctx, "SELECT count(*), max(open_bid) FROM quote_bars WHERE pair=$1 AND tf=$2", "USD/KRW", "1m").Scan(&n, &bid); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row 수 = %d, want 1 (ON CONFLICT DO NOTHING)", n)
	}
	if bid != 1399.5 {
		t.Errorf("first-write-wins 위반: open_bid = %v, want 1399.5", bid)
	}
}

func TestPgxInserter_EmptyBatch(t *testing.T) {
	pool := pgxtest.StartTimescale(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ins := NewPgxInserter(pool)
	if err := ins.Insert(ctx, nil); err != nil {
		t.Errorf("nil batch: %v", err)
	}
	if err := ins.Insert(ctx, []*quote.Bar{}); err != nil {
		t.Errorf("empty batch: %v", err)
	}
}
