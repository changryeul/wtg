package price

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// Inserter 는 닫힌 봉을 영속 저장소에 batch INSERT 한다.
//
// 구현체:
//   - PgxInserter   — TimescaleDB (운영, archiver_pgx.go)
//   - fakeInserter  — 테스트용 (archiver_test.go)
//
// 호출 패턴:
//   - bars 는 가변 길이 (1 ~ BatchMax). 호출자는 정렬 보장 안 함.
//   - PK (pair, tf, opened_at) 충돌은 ON CONFLICT DO NOTHING 으로 무시.
//   - 일부 실패 시 error 반환 — Archiver 가 metric 카운트 후 다음 batch 진행.
type Inserter interface {
	Insert(ctx context.Context, bars []*quote.Bar) error
}

// ArchiverOptions 는 Archiver 구성 옵션.
type ArchiverOptions struct {
	// QueueSize 는 in-memory 버퍼 크기 (default 10000).
	// 초과 시 새 봉은 drop 되고 metric 카운트 (시세 처리는 무영향).
	QueueSize int
	// FlushInterval 은 누적 봉을 batch INSERT 하는 주기 (default 1s).
	FlushInterval time.Duration
	// BatchMax 는 단일 INSERT batch 의 최대 행 수 (default 500).
	BatchMax int
	// Logger — nil 이면 slog.Default().
	Logger *slog.Logger
}

func (o *ArchiverOptions) defaults() {
	if o.QueueSize <= 0 {
		o.QueueSize = 10000
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.BatchMax <= 0 {
		o.BatchMax = 500
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// ArchiverStats 는 누적 카운터 snapshot.
type ArchiverStats struct {
	Enqueued uint64 `json:"enqueued"`
	Dropped  uint64 `json:"dropped"`  // queue full 로 drop 된 건수
	Inserted uint64 `json:"inserted"` // Inserter.Insert 호출 성공 후 합산 (DB ON CONFLICT 도 포함)
	Failed   uint64 `json:"failed"`   // Insert error 로 실패한 batch 건수
}

// Archiver 는 Aggregator.BarCloseHandler 콜백을 받아 영속 저장소에 batch INSERT 한다.
//
// 흐름:
//
//	Aggregator.onClose(*Bar)  → Archiver.OnBarClose
//	                            └→ Persistent TF 필터 → 채널 enqueue (non-blocking)
//	                                                   └→ Run goroutine 이 소비:
//	                                                       - BatchMax 도달 또는 FlushInterval 경과 시
//	                                                       - Inserter.Insert(batch) 호출
//
// 동시성:
//   - OnBarClose 는 여러 goroutine (Aggregator.OnTick / Sweep) 에서 호출 — 채널이 직렬화.
//   - Run 은 단일 goroutine — batch 누적/flush 는 무경쟁.
//   - Close 는 idempotent.
type Archiver struct {
	ins  Inserter
	opts ArchiverOptions
	q    chan *quote.Bar
	log  *slog.Logger

	enqueued atomic.Uint64
	dropped  atomic.Uint64
	inserted atomic.Uint64
	failed   atomic.Uint64
}

// NewArchiver 는 Archiver 를 구성. Run 을 별도 goroutine 으로 호출해야 동작.
func NewArchiver(ins Inserter, opts ArchiverOptions) *Archiver {
	opts.defaults()
	return &Archiver{
		ins:  ins,
		opts: opts,
		q:    make(chan *quote.Bar, opts.QueueSize),
		log:  opts.Logger,
	}
}

// OnBarClose 는 Aggregator 의 BarCloseHandler 시그니처와 동일.
// TF1s 같은 비영속 timeframe 은 여기서 drop. queue full 시 drop + 카운트.
func (a *Archiver) OnBarClose(b *quote.Bar) {
	if b == nil || !b.TF.Persistent() {
		return
	}
	select {
	case a.q <- b:
		a.enqueued.Add(1)
	default:
		a.dropped.Add(1)
		a.log.Warn("archiver: queue full, 봉 drop",
			slog.String("pair", string(b.Pair)),
			slog.String("tf", string(b.TF)),
			slog.Time("opened_at", b.OpenedAt),
		)
	}
}

// Run 은 ctx 가 종료될 때까지 봉 batch INSERT 루프를 돈다.
// 종료 시 queue 잔여분을 모두 drain 한 후 반환.
func (a *Archiver) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]*quote.Bar, 0, a.opts.BatchMax)

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		// flush 는 짧은 timeout 으로 — ctx 종료 후에도 끝까지 시도.
		fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.ins.Insert(fctx, batch); err != nil {
			a.failed.Add(1)
			a.log.Error("archiver: INSERT 실패",
				slog.String("reason", reason),
				slog.Int("batch_size", len(batch)),
				slog.Any("error", err),
			)
		} else {
			a.inserted.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// drain — 남은 큐 잔여분 처리.
			for {
				select {
				case b := <-a.q:
					batch = append(batch, b)
					if len(batch) >= a.opts.BatchMax {
						flush("drain-batch-full")
					}
				default:
					flush("drain-final")
					return ctx.Err()
				}
			}

		case b := <-a.q:
			batch = append(batch, b)
			if len(batch) >= a.opts.BatchMax {
				flush("batch-full")
			}

		case <-ticker.C:
			flush("interval")
		}
	}
}

// Stats 는 누적 카운터 snapshot.
func (a *Archiver) Stats() ArchiverStats {
	return ArchiverStats{
		Enqueued: a.enqueued.Load(),
		Dropped:  a.dropped.Load(),
		Inserted: a.inserted.Load(),
		Failed:   a.failed.Load(),
	}
}

// ErrInserterClosed 는 Inserter 구현체가 닫혔음을 신호하는 sentinel.
// PgxInserter 같은 실 구현체가 사용한다.
var ErrInserterClosed = errors.New("archiver: inserter closed")
