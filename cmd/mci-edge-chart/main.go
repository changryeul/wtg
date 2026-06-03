// mci-edge-chart 는 WTG 의 DMZ 측 챠트 reverse proxy.
//
// Internal mci-chart 의 / (UI), /v1/chart (REST), /v1/chart/stream (WS) 을 그대로
// 외부에 노출하면서 TLS termination + 인증 + IP 화이트리스트 + rate-limit 만 입힌다.
//
// 사용 예:
//
//	mci-edge-chart --listen=:8087 --upstream=http://internal-chart:8086 --dev
//
//	# 운영 — TLS termination + JWT 검증
//	mci-edge-chart --listen=:8443 \
//	  --upstream=http://internal-chart:8086 \
//	  --tls-cert=/etc/wtg/edge-chart.crt --tls-key=/etc/wtg/edge-chart.key \
//	  --jwt-pub=/etc/wtg/jwt-public.pem \
//	  --allow-cidrs=10.0.0.0/8,172.16.0.0/12
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	edgechart "github.com/winwaysystems/wtg/internal/edge/chart"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := edgechart.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-chart: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-edge-chart 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamURL),
		slog.Bool("dev", cfg.DevMode),
		slog.Bool("tls", cfg.TLSCertFile != ""),
		slog.Bool("jwt", cfg.JWTPubFile != ""),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-edge-chart",
		cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
		logger); shutdown != nil {
		defer shutdown(ctx)
	}

	srv := edgechart.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-chart 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-chart 정상 종료")
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
