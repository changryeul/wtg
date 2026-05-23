//go:build integration

// Package pgxtest 는 통합 테스트용 TimescaleDB 컨테이너 헬퍼.
//
// 활성화: `go test -tags=integration ./...`
// 컨테이너 띄우기 → quote_bars hypertable DDL 적용 → *pgxpool.Pool 반환.
// t.Cleanup 으로 자동 정리.
package pgxtest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TimescaleImage 은 통합 테스트에 사용할 timescale 이미지.
// 환경변수 WTG_TIMESCALE_IMAGE 로 override 가능 (CI 등에서 캐시된 이미지 사용).
const defaultImage = "timescale/timescaledb:latest-pg16"

func imageRef() string {
	if v := os.Getenv("WTG_TIMESCALE_IMAGE"); v != "" {
		return v
	}
	return defaultImage
}

// StartTimescale 은 TimescaleDB 컨테이너를 띄우고 quote_bars 스키마를 적용한
// *pgxpool.Pool 을 반환한다. t.Cleanup 으로 자동 정리.
//
// 첫 호출은 이미지 pull + 컨테이너 부팅 + DDL 적용으로 ~10s 가량. 같은 테스트
// 패키지 안에서 여러 번 호출하면 매번 새 컨테이너가 뜨므로 셋업이 무거운 경우
// helper 를 패키지 전체에서 1회만 호출하도록 호출자가 조정.
func StartTimescale(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpostgres.Run(ctx,
		imageRef(),
		tcpostgres.WithDatabase("wtg"),
		tcpostgres.WithUsername("wtg"),
		tcpostgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("postgres container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// quote_bars hypertable DDL 적용 — etc/sql/quote_bars.sql.
	ddl, err := readQuoteBarsDDL()
	if err != nil {
		t.Fatalf("DDL read: %v", err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("DDL apply: %v", err)
	}
	return pool
}

// readQuoteBarsDDL 은 etc/sql/quote_bars.sql 을 읽는다.
// 테스트 실행 위치가 패키지마다 다르므로 runtime caller 로 repo root 찾기.
func readQuoteBarsDDL() ([]byte, error) {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile = .../wtg/test/pgxtest/pgxtest.go
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return os.ReadFile(filepath.Join(repoRoot, "etc", "sql", "quote_bars.sql"))
}
