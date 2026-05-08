// mci-edge-api 는 WTG 의 DMZ 측 REST 프록시.
//
// 외부 web 클라이언트의 HTTP 요청을 받아 인증 후 Internal mci-api 로
// 그대로 forward 한다 (passthrough). 비즈니스 로직 없음.
//
// 사용 예:
//
//	mci-edge-api --listen=:8090 --upstream=http://internal-host:8080 --dev
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	edgeapi "github.com/winwaysystems/wtg/internal/edge/api"
)

func main() {
	cfg, err := edgeapi.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-api: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-edge-api 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamURL),
		slog.Bool("dev", cfg.DevMode),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := edgeapi.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-api 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-api 정상 종료")
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
