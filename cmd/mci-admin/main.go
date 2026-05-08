// mci-admin 은 WTG 의 직원용 control plane.
//
// 사내망 전용 — DMZ 미경유. 운영팀이 broker 상태/세션/클라이언트를 조회하고
// 향후 라우팅/정책 관리.
//
// 사용 예:
//
//	mci-admin --listen=:9090 --broker-host=127.0.0.1 --broker-port=5670 \
//	          --allow-cidrs="10.0.0.0/8,127.0.0.1/32" --dev=true
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/winwaysystems/wtg/internal/admin"
)

func main() {
	cfg, err := admin.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-admin: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-admin 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", cfg.BrokerHost, cfg.BrokerPort)),
		slog.Bool("dev", cfg.DevMode),
		slog.Int("allow_cidrs", len(cfg.AllowCIDRs)),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := admin.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-admin 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-admin 정상 종료")
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
