package quoteid

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

func mkRec(id QuoteID, issued time.Time, validity time.Duration) Record {
	return Record{
		QuoteID:    id,
		Pair:       session.Pair("USD/KRW"),
		Profile:    session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP},
		Tenor:      "SPOT",
		Bid:        1400.10,
		Ask:        1400.15,
		IssuedAt:   issued.UnixNano(),
		ValidUntil: issued.Add(validity).UnixNano(),
		Sequence:   42,
		Issuer:     "A",
	}
}

func TestMemoryRegistry_PutGet(t *testing.T) {
	reg := NewMemoryRegistry(0)
	now := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return now })

	rec := mkRec("A-mq-1", now, 500*time.Millisecond)
	if err := reg.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := reg.Get(context.Background(), rec.QuoteID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Bid != rec.Bid || got.Ask != rec.Ask {
		t.Errorf("bid/ask mismatch: got %v/%v want %v/%v", got.Bid, got.Ask, rec.Bid, rec.Ask)
	}
	if got.Profile.Key() != "WEB.BRANCH.VIP" {
		t.Errorf("Profile.Key mismatch: %s", got.Profile.Key())
	}
}

func TestMemoryRegistry_GetNotFound(t *testing.T) {
	reg := NewMemoryRegistry(0)
	_, err := reg.Get(context.Background(), "A-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotFound 기대, got %v", err)
	}
}

func TestMemoryRegistry_Expiry(t *testing.T) {
	reg := NewMemoryRegistry(0)
	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	rec := mkRec("A-1", t0, 500*time.Millisecond)
	_ = reg.Put(context.Background(), rec)

	// 만료 직전.
	reg.SetNow(func() time.Time { return t0.Add(499 * time.Millisecond) })
	if _, err := reg.Get(context.Background(), rec.QuoteID); err != nil {
		t.Errorf("만료 직전 Get 실패: %v", err)
	}

	// 만료 직후 (grace=0).
	reg.SetNow(func() time.Time { return t0.Add(501 * time.Millisecond) })
	if _, err := reg.Get(context.Background(), rec.QuoteID); !errors.Is(err, ErrNotFound) {
		t.Errorf("만료 후 ErrNotFound 기대, got %v", err)
	}
}

func TestMemoryRegistry_Grace(t *testing.T) {
	reg := NewMemoryRegistry(200 * time.Millisecond)
	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	rec := mkRec("A-1", t0, 500*time.Millisecond)
	_ = reg.Put(context.Background(), rec)

	// ValidUntil 지났지만 grace 안 — 여전히 보존.
	reg.SetNow(func() time.Time { return t0.Add(600 * time.Millisecond) })
	if _, err := reg.Get(context.Background(), rec.QuoteID); err != nil {
		t.Errorf("grace 안 Get 실패: %v", err)
	}

	// grace 도 지남.
	reg.SetNow(func() time.Time { return t0.Add(800 * time.Millisecond) })
	if _, err := reg.Get(context.Background(), rec.QuoteID); !errors.Is(err, ErrNotFound) {
		t.Errorf("grace 후 ErrNotFound 기대, got %v", err)
	}
}

func TestMemoryRegistry_PutInvalid(t *testing.T) {
	reg := NewMemoryRegistry(0)
	t0 := time.Unix(1700000000, 0)

	// ValidUntil <= IssuedAt.
	rec := Record{QuoteID: "A-1", IssuedAt: t0.UnixNano(), ValidUntil: t0.UnixNano()}
	if err := reg.Put(context.Background(), rec); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("ValidUntil=IssuedAt 거부 기대, got %v", err)
	}

	// 빈 QuoteID.
	rec2 := mkRec("", t0, time.Second)
	if err := reg.Put(context.Background(), rec2); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("빈 QuoteID 거부 기대, got %v", err)
	}
}

func TestMemoryRegistry_MarkConsumed_FirstWins(t *testing.T) {
	reg := NewMemoryRegistry(time.Hour)
	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	_ = reg.Put(context.Background(), mkRec("A-1", t0, time.Minute))

	r1, err := reg.MarkConsumed(context.Background(), "A-1", "order-X")
	if err != nil {
		t.Fatalf("MarkConsumed first: %v", err)
	}
	if r1.Status != ConsumeOK {
		t.Errorf("first call: %v, want ConsumeOK", r1.Status)
	}

	r2, _ := reg.MarkConsumed(context.Background(), "A-1", "order-Y")
	if r2.Status != ConsumeAlreadyDone {
		t.Errorf("second call: %v, want ConsumeAlreadyDone", r2.Status)
	}
	if r2.ConsumedBy != "order-X" {
		t.Errorf("ConsumedBy = %q, want order-X", r2.ConsumedBy)
	}

	// Consumed read-only.
	who, taken, _ := reg.Consumed(context.Background(), "A-1")
	if !taken || who != "order-X" {
		t.Errorf("Consumed: who=%q taken=%v", who, taken)
	}
}

func TestMemoryRegistry_MarkConsumed_NotFound(t *testing.T) {
	reg := NewMemoryRegistry(0)
	r, _ := reg.MarkConsumed(context.Background(), "A-nope", "order-1")
	if r.Status != ConsumeNotFound {
		t.Errorf("status=%v, want ConsumeNotFound", r.Status)
	}
}

func TestMemoryRegistry_MarkConsumed_Expired(t *testing.T) {
	reg := NewMemoryRegistry(time.Hour) // grace 큼 — record 유지되지만 ValidUntil 도래.
	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	_ = reg.Put(context.Background(), mkRec("A-1", t0, 500*time.Millisecond))

	reg.SetNow(func() time.Time { return t0.Add(2 * time.Second) })
	r, _ := reg.MarkConsumed(context.Background(), "A-1", "order-1")
	if r.Status != ConsumeExpired {
		t.Errorf("status=%v, want ConsumeExpired", r.Status)
	}
}

func TestMemoryRegistry_MarkConsumed_Concurrent(t *testing.T) {
	reg := NewMemoryRegistry(time.Hour)
	_ = reg.Put(context.Background(), mkRec("A-race", time.Now(), time.Hour))

	const N = 64
	results := make(chan ConsumeStatus, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			r, _ := reg.MarkConsumed(context.Background(), "A-race", "order-"+string(rune('A'+i%26)))
			results <- r.Status
		}()
	}
	wg.Wait()
	close(results)

	okCount := 0
	alreadyCount := 0
	for s := range results {
		switch s {
		case ConsumeOK:
			okCount++
		case ConsumeAlreadyDone:
			alreadyCount++
		default:
			t.Errorf("예상 외 status: %v", s)
		}
	}
	if okCount != 1 {
		t.Errorf("OK 카운트 = %d, want 1 (정확히 하나만)", okCount)
	}
	if alreadyCount != N-1 {
		t.Errorf("AlreadyDone 카운트 = %d, want %d", alreadyCount, N-1)
	}
}

func TestMemoryRegistry_Sweep(t *testing.T) {
	reg := NewMemoryRegistry(0)
	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	// 3개 등록, 다른 ValidUntil.
	for i, validity := range []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		rec := mkRec(QuoteID([]byte{'A', '-', byte('0' + i)}), t0, validity)
		_ = reg.Put(context.Background(), rec)
	}
	if reg.Len() != 3 {
		t.Fatalf("초기 len=%d, want 3", reg.Len())
	}

	// 600ms 후 — 2개 만료.
	reg.SetNow(func() time.Time { return t0.Add(600 * time.Millisecond) })
	n := reg.Sweep()
	if n != 2 {
		t.Errorf("Sweep 제거수: got %d, want 2", n)
	}
	if reg.Len() != 1 {
		t.Errorf("Sweep 후 len=%d, want 1", reg.Len())
	}
}
