// mci-api 는 WTG (Winway Trading Gateway) 의 REST API 서비스.
//
// FX 트레이딩 web 클라이언트의 sync RPC 진입점. JSON HTTP 요청을 받아
// MyMQ broker 로 트랜잭션을 보내고, 응답을 JSON 으로 회신한다.
//
// 비즈니스 권한 검증은 매매 엔진에 위임 (auth.md 의 위임 모델).
// 자세한 구현 계획은 docs/roadmap.md Phase 2 참조.
//
// 사용 예:
//
//	mci-api --listen=:8080 --broker-host=10.0.0.10 --dev=true
//	WTG_API_BROKER_HOST=10.0.0.10 mci-api --dev
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/winwaysystems/wtg/internal/api"
)

func main() {
	cfg, err := api.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-api: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-api 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", cfg.BrokerHost, cfg.BrokerPort)),
		slog.String("appl", cfg.ApplName),
		slog.Int("instance", cfg.Instance),
		slog.Bool("dev", cfg.DevMode),
	)

	// SIGINT / SIGTERM 처리.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := api.NewServer(cfg, logger)
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-api 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-api 정상 종료")
}

// newLogger 는 LogLevel 문자열에 따라 slog.Logger 를 생성한다.
// 운영에서는 JSON, 개발에서는 텍스트 핸들러로 시작 (둘 다 stdout).
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
