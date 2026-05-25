package quoteid

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistry — PutMany 호출 횟수 / 항목 수 / 인위적 지연 / 인위적 실패를 캡처.
type fakeRegistry struct {
	mu        sync.Mutex
	putRecs   []Record
	putCalls  int
	manyCalls int
	delay     time.Duration
	failPut   atomic.Bool
	failMany  atomic.Bool
}

func (f *fakeRegistry) Put(_ context.Context, rec Record) error {
	f.mu.Lock()
	f.putRecs = append(f.putRecs, rec)
	f.putCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeRegistry) PutMany(_ context.Context, recs []Record) error {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manyCalls++
	if f.failMany.Load() {
		return errFakeFail
	}
	f.putRecs = append(f.putRecs, recs...)
	return nil
}

func (f *fakeRegistry) Get(context.Context, QuoteID) (Record, error) { return Record{}, ErrNotFound }
func (f *fakeRegistry) Consumed(context.Context, QuoteID) (string, bool, error) {
	return "", false, nil
}
func (f *fakeRegistry) Lookup(context.Context, QuoteID) (LookupResult, error) {
	return LookupResult{}, nil
}
func (f *fakeRegistry) LookupMany(context.Context, []QuoteID) ([]LookupResult, error) {
	return nil, nil
}
func (f *fakeRegistry) MarkConsumed(context.Context, QuoteID, string) (ConsumeResult, error) {
	return ConsumeResult{Status: ConsumeNotFound}, nil
}
func (f *fakeRegistry) MarkConsumedMany(context.Context, []ConsumeRequest) ([]ConsumeResult, error) {
	return nil, nil
}

func (f *fakeRegistry) recs() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.putRecs...)
}

func (f *fakeRegistry) manyN() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manyCalls
}

var errFakeFail = newErrFakeFail()

type errFakeFailT struct{}

func (errFakeFailT) Error() string { return "fake fail" }
func newErrFakeFail() error        { return errFakeFailT{} }

func quietAsyncLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAsyncRegistry_Put_BasicFlush(t *testing.T) {
	inner := &fakeRegistry{}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     100,
		FlushInterval: 10 * time.Millisecond,
		BatchMax:      50,
		Logger:        quietAsyncLogger(),
	})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = a.Close(ctx)
	}()

	now := time.Now()
	for i := 0; i < 10; i++ {
		err := a.Put(context.Background(), mkAsyncRec(i, now))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// Flush 대기.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(inner.recs()) >= 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(inner.recs()); got != 10 {
		t.Errorf("inner records=%d, want 10", got)
	}
	s := a.Stats()
	if s.Enqueued != 10 {
		t.Errorf("Enqueued=%d, want 10", s.Enqueued)
	}
	if s.Dropped != 0 {
		t.Errorf("Dropped=%d, want 0", s.Dropped)
	}
}

func TestAsyncRegistry_Put_DropsWhenFull(t *testing.T) {
	// 인위적으로 worker 가 느리게 → queue 가득 → drop.
	inner := &fakeRegistry{delay: 200 * time.Millisecond}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     5,
		FlushInterval: 50 * time.Millisecond,
		BatchMax:      100,
		Logger:        quietAsyncLogger(),
	})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer c()
		_ = a.Close(ctx)
	}()

	now := time.Now()
	// 100 record 빠르게 enqueue — worker 가 첫 batch 처리 중이라 queue 가득.
	for i := 0; i < 100; i++ {
		_ = a.Put(context.Background(), mkAsyncRec(i, now))
	}
	time.Sleep(10 * time.Millisecond)

	s := a.Stats()
	// QueueSize=5 + 한 batch 처리 중 1개 슬롯 비움 = 약 5-6 enqueued, 나머지 drop.
	if s.Enqueued+s.Dropped != 100 {
		t.Errorf("Enqueued + Dropped = %d, want 100", s.Enqueued+s.Dropped)
	}
	if s.Dropped == 0 {
		t.Errorf("Dropped=0, queue 가득인데 drop 없음")
	}
}

func TestAsyncRegistry_Put_Batching(t *testing.T) {
	inner := &fakeRegistry{}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     1000,
		FlushInterval: time.Hour, // 시간 flush 차단 — BatchMax 도달만 trigger
		BatchMax:      10,
		Logger:        quietAsyncLogger(),
	})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = a.Close(ctx)
	}()

	now := time.Now()
	// 30 record → BatchMax=10 으로 3회 PutMany 기대.
	for i := 0; i < 30; i++ {
		_ = a.Put(context.Background(), mkAsyncRec(i, now))
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if inner.manyN() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := inner.manyN(); got != 3 {
		t.Errorf("PutMany 호출=%d, want 3 (= 30/10)", got)
	}
}

func TestAsyncRegistry_Close_DrainsQueue(t *testing.T) {
	inner := &fakeRegistry{}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     100,
		FlushInterval: time.Hour, // 시간 flush 차단
		BatchMax:      1000,      // size flush 차단
		Logger:        quietAsyncLogger(),
	})

	now := time.Now()
	for i := 0; i < 7; i++ {
		_ = a.Put(context.Background(), mkAsyncRec(i, now))
	}

	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(inner.recs()); got != 7 {
		t.Errorf("Close 후 inner=%d, want 7 (drain)", got)
	}
}

func TestAsyncRegistry_Put_InvalidRecord(t *testing.T) {
	inner := &fakeRegistry{}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{Logger: quietAsyncLogger()})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = a.Close(ctx)
	}()

	// 빈 QuoteID → ErrInvalidRecord, queue 추가 안 됨.
	if err := a.Put(context.Background(), Record{}); err == nil {
		t.Errorf("빈 record 통과")
	}
	if got := a.Stats().Enqueued; got != 0 {
		t.Errorf("Enqueued=%d, want 0 (invalid record)", got)
	}
}

func TestAsyncRegistry_FailureCounters(t *testing.T) {
	inner := &fakeRegistry{}
	inner.failMany.Store(true)
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     100,
		FlushInterval: 10 * time.Millisecond,
		BatchMax:      100,
		Logger:        quietAsyncLogger(),
	})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = a.Close(ctx)
	}()

	now := time.Now()
	for i := 0; i < 5; i++ {
		_ = a.Put(context.Background(), mkAsyncRec(i, now))
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a.Stats().Failed >= 5 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.Stats().Failed; got != 5 {
		t.Errorf("Failed=%d, want 5 (모두 실패)", got)
	}
	if got := a.Stats().Written; got != 0 {
		t.Errorf("Written=%d, want 0", got)
	}
}

func TestAsyncRegistry_MetricsHook(t *testing.T) {
	inner := &fakeRegistry{}
	a := NewAsyncRegistry(inner, AsyncRegistryOptions{
		QueueSize:     2,
		FlushInterval: 5 * time.Millisecond,
		BatchMax:      1, // 즉시 flush — written 카운터 빠르게 채움.
		Logger:        quietAsyncLogger(),
	})
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = a.Close(ctx)
	}()

	var enq, drop, wrote, fail atomic.Uint64
	a.SetMetricsHook(AsyncMetricsHook{
		Enqueued: func(n uint64) { enq.Add(n) },
		Dropped:  func(n uint64) { drop.Add(n) },
		Written:  func(n uint64) { wrote.Add(n) },
		Failed:   func(n uint64) { fail.Add(n) },
	})

	now := time.Now()
	// 3 enqueue — queue size 2 + worker drain 으로 1개 drop 가능.
	for i := 0; i < 3; i++ {
		_ = a.Put(context.Background(), mkAsyncRec(i, now))
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if enq.Load()+drop.Load() == 3 && wrote.Load() >= enq.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if enq.Load()+drop.Load() != 3 {
		t.Errorf("enq+drop=%d (enq=%d drop=%d), want 3",
			enq.Load()+drop.Load(), enq.Load(), drop.Load())
	}
	if wrote.Load() != enq.Load() {
		t.Errorf("wrote=%d enq=%d (worker 가 enqueued 전부 처리 못함)",
			wrote.Load(), enq.Load())
	}
	if fail.Load() != 0 {
		t.Errorf("fail=%d, want 0", fail.Load())
	}

	// QueueLen — 모두 flush 후 0.
	if a.QueueLen() != 0 {
		t.Errorf("QueueLen=%d, want 0", a.QueueLen())
	}
}

func mkAsyncRec(i int, ts time.Time) Record {
	idBytes := []byte("A-")
	idBytes = append(idBytes, byte('0'+i/10))
	idBytes = append(idBytes, byte('0'+i%10))
	return Record{
		QuoteID:    QuoteID(idBytes),
		Pair:       "USD/KRW",
		Bid:        1400.0,
		Ask:        1400.05,
		IssuedAt:   ts.UnixNano(),
		ValidUntil: ts.Add(time.Hour).UnixNano(),
		Sequence:   uint64(i),
		Issuer:     "A",
	}
}
