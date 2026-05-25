package quoteid

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncRegistry — Put 호출을 채널로 큐잉, 백그라운드 worker 가 batch + Pipeline
// 으로 묶음 송신. PricingConsumer.OnTick (hot path) 가 Redis RTT 에 블록되지
// 않게 한다.
//
// 흐름:
//
//	PricingConsumer.OnTick → AsyncRegistry.Put → channel send (즉시 반환)
//	                         (queue full → drop + 카운터, hot path 보호)
//	worker goroutine ←─ channel
//	  batch (size N 또는 시간 T) → inner.PutMany → 1 RTT
//
// Get / Lookup / MarkConsumed 는 inner 로 pass-through — 읽기 경로는 그대로
// sync (낮은 latency 보장 필요).
//
// At-least-once 가 아님 — drop 시 record 손실 가능. 운영 정책 (FX Global Code
// 17 의 audit) 상 best-effort 감사 추적이라 허용. drop 률은 메트릭으로
// 모니터링.
type AsyncRegistry struct {
	inner Registry
	queue chan Record

	flushInterval time.Duration
	batchMax      int
	putTimeout    time.Duration

	logger *slog.Logger
	now    func() time.Time

	// 카운터.
	enqueued atomic.Uint64
	dropped  atomic.Uint64 // queue full → drop
	written  atomic.Uint64
	failed   atomic.Uint64 // PutMany error

	stopOnce sync.Once
	doneC    chan struct{}
}

// AsyncRegistryOptions — AsyncRegistry 생성 옵션.
type AsyncRegistryOptions struct {
	// QueueSize — 채널 버퍼. default 10000.
	QueueSize int
	// FlushInterval — batch 가 BatchMax 미만이어도 이 시간 지나면 flush.
	// default 5ms. 짧을수록 latency 짧지만 더 작은 batch.
	FlushInterval time.Duration
	// BatchMax — 한 PutMany 호출의 최대 record 수. default 200.
	BatchMax int
	// PutTimeout — 단일 PutMany 에 부여하는 timeout. default 200ms.
	PutTimeout time.Duration
	// Logger — nil 이면 slog.Default.
	Logger *slog.Logger
}

// NewAsyncRegistry — inner Registry 를 감싸는 비동기 wrapper. inner 가 nil 이면
// panic. worker goroutine 이 즉시 시작.
func NewAsyncRegistry(inner Registry, opt AsyncRegistryOptions) *AsyncRegistry {
	if inner == nil {
		panic("quoteid: AsyncRegistry 에 inner Registry 필수")
	}
	queueSize := opt.QueueSize
	if queueSize <= 0 {
		queueSize = 10000
	}
	flush := opt.FlushInterval
	if flush <= 0 {
		flush = 5 * time.Millisecond
	}
	batchMax := opt.BatchMax
	if batchMax <= 0 {
		batchMax = 200
	}
	putTimeout := opt.PutTimeout
	if putTimeout <= 0 {
		putTimeout = 200 * time.Millisecond
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &AsyncRegistry{
		inner:         inner,
		queue:         make(chan Record, queueSize),
		flushInterval: flush,
		batchMax:      batchMax,
		putTimeout:    putTimeout,
		logger:        logger,
		now:           time.Now,
		doneC:         make(chan struct{}),
	}
	go a.run()
	return a
}

// Put — non-blocking enqueue. queue full 이면 drop (best-effort).
// 호출자 (PricingConsumer hot path) 는 err 를 기대하지 않음 — 항상 nil 반환.
func (a *AsyncRegistry) Put(_ context.Context, rec Record) error {
	if rec.QuoteID == "" || rec.ValidUntil <= rec.IssuedAt {
		return ErrInvalidRecord
	}
	select {
	case a.queue <- rec:
		a.enqueued.Add(1)
	default:
		a.dropped.Add(1)
		// rate-limit 된 log 는 운영 정책 — 여기선 누적 카운터만.
	}
	return nil
}

// PutMany — inner 의 PutMany 를 그대로 호출 (이미 batch 경로). Async 의
// 이중 큐잉은 의미 없음.
func (a *AsyncRegistry) PutMany(ctx context.Context, recs []Record) error {
	return a.inner.PutMany(ctx, recs)
}

// 읽기 path — pass-through.
func (a *AsyncRegistry) Get(ctx context.Context, id QuoteID) (Record, error) {
	return a.inner.Get(ctx, id)
}
func (a *AsyncRegistry) Consumed(ctx context.Context, id QuoteID) (string, bool, error) {
	return a.inner.Consumed(ctx, id)
}
func (a *AsyncRegistry) Lookup(ctx context.Context, id QuoteID) (LookupResult, error) {
	return a.inner.Lookup(ctx, id)
}
func (a *AsyncRegistry) LookupMany(ctx context.Context, ids []QuoteID) ([]LookupResult, error) {
	return a.inner.LookupMany(ctx, ids)
}
func (a *AsyncRegistry) MarkConsumed(ctx context.Context, id QuoteID, consumerID string) (ConsumeResult, error) {
	return a.inner.MarkConsumed(ctx, id, consumerID)
}
func (a *AsyncRegistry) MarkConsumedMany(ctx context.Context, reqs []ConsumeRequest) ([]ConsumeResult, error) {
	return a.inner.MarkConsumedMany(ctx, reqs)
}

// run — worker goroutine. queue 가 닫히면 남은 batch flush 후 종료.
func (a *AsyncRegistry) run() {
	defer close(a.doneC)
	batch := make([]Record, 0, a.batchMax)
	t := time.NewTicker(a.flushInterval)
	defer t.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), a.putTimeout)
		err := a.inner.PutMany(ctx, batch)
		cancel()
		if err != nil {
			a.failed.Add(uint64(len(batch)))
			a.logger.Warn("AsyncRegistry: PutMany 실패",
				slog.Int("batch", len(batch)),
				slog.Any("error", err))
		} else {
			a.written.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case rec, ok := <-a.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= a.batchMax {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// Close — queue 를 닫고 worker 가 남은 batch 를 flush 한 뒤 반환. 중복 호출 안전.
//
// 종료 timeout 은 PricingConsumer 의 graceful shutdown 단계에서 부여 — 운영 시
// SIGTERM 후 일정 시간 안에 미flush record 는 손실 (best-effort audit).
func (a *AsyncRegistry) Close(ctx context.Context) error {
	a.stopOnce.Do(func() { close(a.queue) })
	select {
	case <-a.doneC:
		return nil
	case <-ctx.Done():
		return errors.New("quoteid: AsyncRegistry close timeout")
	}
}

// AsyncStats — 누적 카운터 snapshot.
type AsyncStats struct {
	Enqueued  uint64 `json:"enqueued"`
	Dropped   uint64 `json:"dropped"`
	Written   uint64 `json:"written"`
	Failed    uint64 `json:"failed"`
	QueueLen  int    `json:"queue_len"`
	QueueCap  int    `json:"queue_cap"`
}

func (a *AsyncRegistry) Stats() AsyncStats {
	return AsyncStats{
		Enqueued: a.enqueued.Load(),
		Dropped:  a.dropped.Load(),
		Written:  a.written.Load(),
		Failed:   a.failed.Load(),
		QueueLen: len(a.queue),
		QueueCap: cap(a.queue),
	}
}
