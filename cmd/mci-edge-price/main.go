// mci-edge-price 는 WTG 의 DMZ 측 시세 fan-out edge.
//
// Internal mci-price 의 PriceService gRPC stream 에 접속해서 Tick 을 받고,
// DMZ 측 WebSocket 클라이언트들에게 broadcast 한다.
//
// 사용 예:
//
//	mci-edge-price --listen=:8083 --upstream=internal-host:50051 --dev=true
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	edgeprice "github.com/winwaysystems/wtg/internal/edge/price"
)

func main() {
	cfg, err := edgeprice.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-price: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-edge-price 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamGRPC),
		slog.String("subscriber_id", cfg.SubscriberID),
		slog.Bool("dev", cfg.DevMode),
		slog.Bool("quote_stream", cfg.EnableQuoteStream),
		slog.Any("quote_profiles", cfg.QuoteProfileKeys),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := edgeprice.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-price 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-price 정상 종료")
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
