// mci-edge-krx 는 WTG 의 DMZ 측 선물시세 WebSocket gateway.
//
// 기존 유안타 선물 피드가 만든 KA(체결)/KB(호가) 전문을 받아 web 용 JSON 으로
// 디코드하고, 종목 구독 web 클라이언트에게 fan-out 한다. 시세 트랙(mci-edge-price)
// 의 종목 구독 모델을 선물용으로 격리 구현 (docs/futures-sise-design.md).
//
// 사용:
//
//	# 단독 시연 (합성 틱)
//	mci-edge-krx --listen=:8085 --demo --demo-codes=101V6000,105V3000
//	# web: ws://host:8085/v1/subscribe → {"type":"subscribe","symbols":["101V6000"]}
//
// 실 피드 배선(0b)은 broker/gRPC ingest 를 srv.Ingest 로 연결 (후속).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	krx "github.com/winwaysystems/wtg/internal/krx"
	"github.com/winwaysystems/wtg/pkg/logging"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := krx.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-krx: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := logging.Init("mci-edge-krx", logging.Options{Level: cfg.LogLevel})
	logger.Info("mci-edge-krx 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.Bool("dev", cfg.DevMode),
		slog.Bool("demo", cfg.Demo),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-edge-krx",
		cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
		logger); shutdown != nil {
		defer shutdown(ctx)
	}

	srv := krx.NewServer(logger)
	if err := srv.Start(ctx, cfg); err != nil {
		logger.Error("mci-edge-krx 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-krx 정상 종료")
}
