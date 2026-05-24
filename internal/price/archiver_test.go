package price

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// fakeInserter 는 호출 내역만 기록하는 Inserter 테스트 더블.
type fakeInserter struct {
	mu      sync.Mutex
	batches [][]quote.Bar
	err     error
	delay   time.Duration
	calls   int
}

func (f *fakeInserter) Insert(ctx context.Context, bars []*quote.Bar) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	snapshot := make([]quote.Bar, len(bars))
	for i, b := range bars {
		snapshot[i] = *b
	}
	f.batches = append(f.batches, snapshot)
	return nil
}

func (f *fakeInserter) totalRows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.batches {
		n += len(b)
	}
	return n
}

func TestArchiver_FiltersNonPersistentTF(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{QueueSize: 10})

	// TF1s 는 비영속 → drop.
	b1s := quote.NewBar(quote.TF1s, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1.1, TS: time.Now()})
	b1s.Close()

	arc.OnBarClose(b1s)

	if got := arc.Stats().Enqueued; got != 0 {
		t.Errorf("TF1s 봉이 enqueue 됨: %d", got)
	}
}

func TestArchiver_EnqueuesPersistentTF(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{QueueSize: 10})

	b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1.1, TS: time.Now()})
	b.Close()
	arc.OnBarClose(b)

	if got := arc.Stats().Enqueued; got != 1 {
		t.Errorf("Enqueued = %d, want 1", got)
	}
}

func TestArchiver_DropsWhenQueueFull(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{QueueSize: 2})

	for i := 0; i < 5; i++ {
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1.1, TS: time.Unix(int64(i*60), 0)})
		b.Close()
		arc.OnBarClose(b)
	}
	s := arc.Stats()
	if s.Enqueued != 2 {
		t.Errorf("Enqueued = %d, want 2 (queue size)", s.Enqueued)
	}
	if s.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", s.Dropped)
	}
}

func TestArchiver_FlushOnInterval(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{
		QueueSize:     100,
		FlushInterval: 20 * time.Millisecond,
		BatchMax:      1000,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { _ = arc.Run(ctx); close(done) }()

	for i := 0; i < 3; i++ {
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: float64(i), Ask: 1, TS: time.Unix(int64(i*60), 0)})
		b.Close()
		arc.OnBarClose(b)
	}

	// flush 발생 대기.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ins.totalRows() == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := ins.totalRows(); got != 3 {
		t.Errorf("총 INSERT row = %d, want 3", got)
	}
	cancel()
	<-done

	if arc.Stats().Inserted != 3 {
		t.Errorf("Inserted = %d, want 3", arc.Stats().Inserted)
	}
}

func TestArchiver_FlushOnBatchFull(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{
		QueueSize:     100,
		FlushInterval: 10 * time.Second, // 거의 안 일어남
		BatchMax:      3,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = arc.Run(ctx); close(done) }()

	for i := 0; i < 3; i++ {
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: float64(i), Ask: 1, TS: time.Unix(int64(i*60), 0)})
		b.Close()
		arc.OnBarClose(b)
	}

	// BatchMax 도달로 즉시 flush 되어야 함.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ins.totalRows() == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := ins.totalRows(); got != 3 {
		t.Errorf("BatchMax flush — INSERT row = %d, want 3", got)
	}
	cancel()
	<-done
}

func TestArchiver_DrainsOnShutdown(t *testing.T) {
	ins := &fakeInserter{}
	arc := NewArchiver(ins, ArchiverOptions{
		QueueSize:     100,
		FlushInterval: 10 * time.Second,
		BatchMax:      1000,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = arc.Run(ctx); close(done) }()

	for i := 0; i < 5; i++ {
		b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1, TS: time.Unix(int64(i*60), 0)})
		b.Close()
		arc.OnBarClose(b)
	}

	cancel()
	<-done

	if got := ins.totalRows(); got != 5 {
		t.Errorf("shutdown drain INSERT row = %d, want 5", got)
	}
}

func TestArchiver_CountsFailedInserts(t *testing.T) {
	ins := &fakeInserter{err: errors.New("simulated db down")}
	arc := NewArchiver(ins, ArchiverOptions{
		QueueSize:     10,
		FlushInterval: 20 * time.Millisecond,
		BatchMax:      1000,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = arc.Run(ctx); close(done) }()

	b := quote.NewBar(quote.TF1m, quote.Quote{Pair: "USD/KRW", Bid: 1, Ask: 1, TS: time.Now()})
	b.Close()
	arc.OnBarClose(b)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if arc.Stats().Failed > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if arc.Stats().Failed == 0 {
		t.Error("Failed counter 가 증가하지 않음")
	}
	if arc.Stats().Inserted != 0 {
		t.Errorf("Inserted = %d, want 0 (모두 실패)", arc.Stats().Inserted)
	}
	cancel()
	<-done
}
