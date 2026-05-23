// mci-chart 는 WTG (Winway Trading Gateway) 의 OHLC 챠트 서비스.
//
// TimescaleDB 의 quote_bars 에서 historical 봉을 조회해 REST 로 제공한다.
// 봉 생성/INSERT 는 mci-price 의 Aggregator/Archiver 가 담당 — mci-chart 는
// read-only.
//
// 사용 예:
//
//	mci-chart --listen=:8086 --dsn="postgres://wtg:secret@db:5432/wtg" --pool=10
//
//	curl 'http://localhost:8086/v1/chart?pair=USD/KRW&tf=1m&from=2026-05-23T00:00:00Z&to=2026-05-23T06:00:00Z'
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/winwaysystems/wtg/internal/chart"
)

func main() {
	cfg, err := chart.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-chart: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-chart 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.Int("pool_max", cfg.PoolMaxConns),
		slog.Int("max_rows", cfg.QueryMaxRows),
	)

	srv := chart.NewServer(cfg, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-chart 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-chart 정상 종료")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
