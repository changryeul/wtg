package chart

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// PgxRepository 는 TimescaleDB(PostgreSQL) 의 quote_bars 테이블에서 봉을 조회.
// 스키마: etc/sql/quote_bars.sql / docs/chart-schema.md
type PgxRepository struct {
	pool *pgxpool.Pool
}

// NewPgxRepository 는 PgxRepository 를 생성. pool 의 lifecycle 은 호출자 관리.
func NewPgxRepository(pool *pgxpool.Pool) *PgxRepository {
	return &PgxRepository{pool: pool}
}

// querySQL 은 (pair, tf, opened_at) 인덱스로 즉시 응답하는 단순 select.
const querySQL = `
SELECT opened_at, closed_at,
       open_bid, open_ask, high_bid, high_ask,
       low_bid, low_ask, close_bid, close_ask, tick_count
  FROM quote_bars
 WHERE pair = $1 AND tf = $2
   AND opened_at >= $3 AND opened_at < $4
 ORDER BY opened_at
 LIMIT $5
`

// QueryBars 는 [from, to) 범위의 봉을 opened_at ascending 으로 반환.
// 반환 Bar 의 Pair / TF 는 입력값으로 채운다 (SELECT 절에 포함시키지 않음 — 같은 값 반복).
func (r *PgxRepository) QueryBars(ctx context.Context, pair session.Pair, tf quote.Timeframe, from, to time.Time, limit int) ([]quote.Bar, error) {
	rows, err := r.pool.Query(ctx, querySQL, string(pair), string(tf), from.UTC(), to.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("chart: query bars (%s/%s): %w", pair, tf, err)
	}
	defer rows.Close()

	out := make([]quote.Bar, 0, 256)
	for rows.Next() {
		var b quote.Bar
		b.Pair = pair
		b.TF = tf
		if err := rows.Scan(
			&b.OpenedAt, &b.ClosedAt,
			&b.OpenBid, &b.OpenAsk, &b.HighBid, &b.HighAsk,
			&b.LowBid, &b.LowAsk, &b.CloseBid, &b.CloseAsk,
			&b.TickCount,
		); err != nil {
			return nil, fmt.Errorf("chart: scan bar: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: rows iter: %w", err)
	}
	return out, nil
}
