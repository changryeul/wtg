package quote

import (
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

func mkQuote(pair session.Pair, sec int64, bid float64) Quote {
	return Quote{
		Pair: pair,
		Bid:  bid,
		Ask:  bid + 0.10,
		TS:   time.Unix(sec, 0),
	}
}

func TestRingBuffer_AddAndSnapshot_NotFull(t *testing.T) {
	r := NewRingBuffer(5)
	r.Add(mkQuote("USD/KRW", 1, 1.0))
	r.Add(mkQuote("USD/KRW", 2, 2.0))
	r.Add(mkQuote("USD/KRW", 3, 3.0))

	got := r.Snapshot("USD/KRW", 0)
	if len(got) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(got))
	}
	for i, want := range []float64{1.0, 2.0, 3.0} {
		if got[i].Bid != want {
			t.Errorf("got[%d].Bid = %v, want %v", i, got[i].Bid, want)
		}
	}
}

func TestRingBuffer_AddAndSnapshot_Wrap(t *testing.T) {
	r := NewRingBuffer(3)
	for i := 1; i <= 5; i++ {
		r.Add(mkQuote("USD/KRW", int64(i), float64(i)))
	}
	// ring 은 마지막 3건만 보유 (3,4,5).
	got := r.Snapshot("USD/KRW", 0)
	if len(got) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(got))
	}
	for i, want := range []float64{3.0, 4.0, 5.0} {
		if got[i].Bid != want {
			t.Errorf("wrap got[%d].Bid = %v, want %v", i, got[i].Bid, want)
		}
	}
}

func TestRingBuffer_SnapshotMaxLimit(t *testing.T) {
	r := NewRingBuffer(5)
	for i := 1; i <= 5; i++ {
		r.Add(mkQuote("USD/KRW", int64(i), float64(i)))
	}
	// 최근 2건만.
	got := r.Snapshot("USD/KRW", 2)
	if len(got) != 2 {
		t.Fatalf("max=2 snapshot len = %d", len(got))
	}
	if got[0].Bid != 4.0 || got[1].Bid != 5.0 {
		t.Errorf("max=2 chronological = %v, %v", got[0].Bid, got[1].Bid)
	}
}

func TestRingBuffer_SnapshotMaxBeyondHave(t *testing.T) {
	r := NewRingBuffer(5)
	r.Add(mkQuote("USD/KRW", 1, 1.0))
	r.Add(mkQuote("USD/KRW", 2, 2.0))

	// max=100 이지만 가용 2건만 반환.
	got := r.Snapshot("USD/KRW", 100)
	if len(got) != 2 {
		t.Fatalf("max=100 snapshot len = %d, want 2", len(got))
	}
}

func TestRingBuffer_MissingPair(t *testing.T) {
	r := NewRingBuffer(5)
	if got := r.Snapshot("XAU/USD", 0); got != nil {
		t.Errorf("missing pair Snapshot = %v, want nil", got)
	}
	if r.Size("XAU/USD") != 0 {
		t.Errorf("missing pair Size != 0")
	}
}

func TestRingBuffer_Size(t *testing.T) {
	r := NewRingBuffer(3)
	r.Add(mkQuote("USD/KRW", 1, 1.0))
	r.Add(mkQuote("USD/KRW", 2, 2.0))
	if got := r.Size("USD/KRW"); got != 2 {
		t.Errorf("size before full = %d, want 2", got)
	}
	r.Add(mkQuote("USD/KRW", 3, 3.0))
	r.Add(mkQuote("USD/KRW", 4, 4.0))
	// full.
	if got := r.Size("USD/KRW"); got != 3 {
		t.Errorf("size after wrap = %d, want 3", got)
	}
}

func TestRingBuffer_MultiPair(t *testing.T) {
	r := NewRingBuffer(3)
	r.Add(mkQuote("USD/KRW", 1, 100))
	r.Add(mkQuote("EUR/KRW", 1, 200))
	r.Add(mkQuote("USD/KRW", 2, 101))

	usd := r.Snapshot("USD/KRW", 0)
	eur := r.Snapshot("EUR/KRW", 0)
	if len(usd) != 2 || len(eur) != 1 {
		t.Errorf("usd len=%d eur len=%d", len(usd), len(eur))
	}
	if eur[0].Bid != 200 {
		t.Errorf("EUR ring contaminated by USD: %v", eur[0].Bid)
	}

	pairs := r.Pairs()
	if len(pairs) != 2 {
		t.Errorf("Pairs len = %d", len(pairs))
	}
}

func TestRingBuffer_ZeroCapPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("cap=0 에서 panic 안 함")
		}
	}()
	NewRingBuffer(0)
}

// 1 writer + N reader 동시성 (go test -race).
func TestRingBuffer_ConcurrentReadWrite(t *testing.T) {
	r := NewRingBuffer(100)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			r.Add(mkQuote("USD/KRW", int64(i), float64(i)))
		}
		close(stop)
	}()

	// readers
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = r.Snapshot("USD/KRW", 50)
					_ = r.Size("USD/KRW")
				}
			}
		}()
	}

	wg.Wait()
}
