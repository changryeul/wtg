// mci-push 는 WTG (Winway Trading Gateway) 의 WebSocket fan-out 서비스.
//
// MyMQ broker 의 unsolicited 메시지(체결/주문상태/알림)를 받아 사용자 단말의
// WebSocket 으로 그대로 전달한다. 메시지 종류별 transformer 를 두지 않고,
// broker → 사용자 raw passthrough 한다.
//
// 사용 예:
//
//	mci-push --listen=:8081 --broker-host=10.0.0.10 --dev=true
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/winwaysystems/wtg/internal/push"
)

func main() {
	cfg, err := push.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-push: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-push 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", cfg.BrokerHost, cfg.BrokerPort)),
		slog.String("queue", cfg.QueueName),
		slog.String("appl", cfg.ApplName),
		slog.Int("instance", cfg.Instance),
		slog.Bool("dev", cfg.DevMode),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := push.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-push 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-push 정상 종료")
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
