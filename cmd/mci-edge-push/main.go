// mci-edge-push 는 WTG 의 DMZ 측 WebSocket gateway.
//
// Internal mci-push 의 PushService gRPC stream 에 접속해서 unsolicited
// 메시지를 받고, DMZ 측 WebSocket 클라이언트들에게 사용자별 fan-out 한다.
//
// 사용 예:
//
//	mci-edge-push --listen=:8084 --upstream=internal-host:50052 --dev=true
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	edgepush "github.com/winwaysystems/wtg/internal/edge/push"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := edgepush.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-push: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-edge-push 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamGRPC),
		slog.String("subscriber_id", cfg.SubscriberID),
		slog.Bool("dev", cfg.DevMode),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-edge-push",
		cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
		logger); shutdown != nil {
		defer shutdown(ctx)
	}

	srv := edgepush.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-push 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-push 정상 종료")
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
