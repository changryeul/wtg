package chart

import (
	"context"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
)

// Repository 는 quote_bars 영속 저장소의 read-side 추상화.
//
// 구현체:
//   - PgxRepository — TimescaleDB (운영, repository_pgx.go)
//   - fakeRepository — 테스트용 (server_test.go)
//
// QueryBars 의 기간 의미: [from, to) — from 포함, to 불포함.
// limit <= 0 또는 limit > Config.QueryMaxRows 인 경우 호출자가 사전 검증한다.
// 반환 slice 는 opened_at ascending.
type Repository interface {
	QueryBars(ctx context.Context, pair session.Pair, tf quote.Timeframe, from, to time.Time, limit int) ([]quote.Bar, error)
}
