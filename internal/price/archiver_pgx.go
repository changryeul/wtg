package price

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// PgxInserter 는 TimescaleDB(PostgreSQL) 의 quote_bars 테이블에 batch INSERT.
// 스키마: etc/sql/quote_bars.sql / docs/chart-schema.md
//
// 사용:
//
//	pool, _ := pgxpool.New(ctx, dsn)
//	ins := NewPgxInserter(pool)
//	arc := NewArchiver(ins, ArchiverOptions{})
//	go arc.Run(ctx)
type PgxInserter struct {
	pool *pgxpool.Pool
}

// NewPgxInserter 는 PgxInserter 를 생성. pool 은 호출자가 만든 것을 받고
// Close 도 호출자가 관리.
func NewPgxInserter(pool *pgxpool.Pool) *PgxInserter {
	return &PgxInserter{pool: pool}
}

// insertSQL 은 단일 봉 INSERT. ON CONFLICT 로 멀티 인스턴스 dedup 안전.
//
// 컬럼 순서:
//
//	$1  pair
//	$2  tf
//	$3  opened_at
//	$4  closed_at
//	$5  open_bid    $6  open_ask
//	$7  high_bid    $8  high_ask
//	$9  low_bid     $10 low_ask
//	$11 close_bid   $12 close_ask
//	$13 tick_count
const insertSQL = `
INSERT INTO quote_bars (
    pair, tf, opened_at, closed_at,
    open_bid, open_ask, high_bid, high_ask,
    low_bid, low_ask, close_bid, close_ask, tick_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (pair, tf, opened_at) DO NOTHING
`

// Insert 는 bars 를 pgx.Batch 로 pipelining INSERT 한다.
// 첫 번째 statement 에러는 batch 진행을 중단하고 즉시 반환 — Archiver 가 batch
// 단위로 failed 카운트.
func (p *PgxInserter) Insert(ctx context.Context, bars []*quote.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, b := range bars {
		batch.Queue(insertSQL,
			string(b.Pair), string(b.TF),
			b.OpenedAt, b.ClosedAt,
			b.OpenBid, b.OpenAsk,
			b.HighBid, b.HighAsk,
			b.LowBid, b.LowAsk,
			b.CloseBid, b.CloseAsk,
			b.TickCount,
		)
	}

	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()

	// 각 statement 의 결과를 소비해야 connection 이 깨끗하게 풀로 반환된다.
	for i := range bars {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("archiver: INSERT bar[%d] (pair=%s tf=%s opened_at=%s): %w",
				i, bars[i].Pair, bars[i].TF, bars[i].OpenedAt.Format("2006-01-02T15:04:05Z"), err)
		}
	}
	return nil
}
