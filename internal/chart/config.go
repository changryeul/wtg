// Package chart 는 WTG (Winway Trading Gateway) 의 OHLC 챠트 서비스 (mci-chart).
//
// 책임 범위:
//
//   - TimescaleDB 의 quote_bars 에서 봉 데이터 조회
//   - REST API: GET /v1/chart?pair=...&tf=...&from=...&to=...
//   - 헬스체크 + stats
//
// 책임 밖 (의도적 제외):
//
//   - 봉 생성/INSERT — mci-price 의 Aggregator/Archiver 책임
//   - 실시간 tick stream — mci-edge-price 의 ws 가 별도 담당
//
// 클라이언트는 mci-chart 로 historical 봉을 받고, mci-edge-price 로 라이브
// tick 을 받아 "마지막 봉만 in-progress 표시" 하는 패턴이 표준.
package chart

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config 는 mci-chart 의 런타임 설정.
type Config struct {
	// HTTP listen 주소.
	ListenAddr string

	// TimescaleDB 연결 DSN (예: "postgres://user:pass@host:5432/wtg").
	// pgxpool.New 에 그대로 전달.
	DSN string

	// pgx 연결 풀 크기 (default 10).
	PoolMaxConns int

	// HTTP timeouts.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// 쿼리당 반환 가능한 최대 row 수 (DoS 보호, default 10000).
	QueryMaxRows int

	// 로그 레벨.
	LogLevel string

	// 통계 출력 주기.
	StatsInterval time.Duration
}

// DefaultConfig 는 운영 디폴트.
func DefaultConfig() Config {
	return Config{
		ListenAddr:    ":8086",
		PoolMaxConns:  10,
		ReadTimeout:   10 * time.Second,
		WriteTimeout:  30 * time.Second,
		QueryMaxRows:  10000,
		LogLevel:      "info",
		StatsInterval: 30 * time.Second,
	}
}

// LoadConfig 는 flag + env 를 합쳐 Config 를 채운다.
//
// 환경변수 prefix: WTG_CHART_*
func LoadConfig(args []string) (Config, error) {
	cfg := DefaultConfig()

	if v := os.Getenv("WTG_CHART_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WTG_CHART_DSN"); v != "" {
		cfg.DSN = v
	}
	if v := os.Getenv("WTG_CHART_POOL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMaxConns = n
		}
	}
	if v := os.Getenv("WTG_CHART_MAX_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.QueryMaxRows = n
		}
	}
	if v := os.Getenv("WTG_CHART_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	fs := flag.NewFlagSet("mci-chart", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen 주소")
	fs.StringVar(&cfg.DSN, "dsn", cfg.DSN, "TimescaleDB DSN")
	fs.IntVar(&cfg.PoolMaxConns, "pool", cfg.PoolMaxConns, "pgx pool 최대 connection")
	fs.IntVar(&cfg.QueryMaxRows, "max-rows", cfg.QueryMaxRows, "쿼리당 반환 row 상한")
	fs.DurationVar(&cfg.ReadTimeout, "read-timeout", cfg.ReadTimeout, "HTTP read timeout")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "HTTP write timeout")
	fs.DurationVar(&cfg.StatsInterval, "stats", cfg.StatsInterval, "통계 출력 주기")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "로그 레벨 debug/info/warn/error")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate 는 필수 항목을 검사.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("chart: ListenAddr 필수")
	}
	if c.DSN == "" {
		return fmt.Errorf("chart: DSN 필수 (--dsn 또는 WTG_CHART_DSN)")
	}
	if c.QueryMaxRows <= 0 {
		return fmt.Errorf("chart: QueryMaxRows > 0 필수")
	}
	if c.PoolMaxConns <= 0 {
		return fmt.Errorf("chart: PoolMaxConns > 0 필수")
	}
	return nil
}
