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
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/winwaysystems/wtg/internal/api"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := api.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-api: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := logging.Init("mci-api", logging.Options{Level: cfg.LogLevel})
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

	// OTel TracerProvider — Endpoint 비면 비활성 (span 미수집).
	if cfg.OtelEndpoint != "" || cfg.OtelStdout {
		ep := cfg.OtelEndpoint
		if cfg.OtelStdout {
			ep = "stdout"
		}
		shutdown, err := otelinit.Setup(ctx, otelinit.Options{
			ServiceName: "mci-api",
			Endpoint:    ep,
			Insecure:    cfg.OtelInsecure,
			SampleRatio: cfg.OtelSampleRatio,
		})
		if err != nil {
			logger.Warn("OTel Setup 실패 — span 비활성", slog.Any("error", err))
		} else {
			logger.Info("OTel TracerProvider 활성", slog.String("endpoint", ep))
			defer shutdown(ctx)
		}
	}

	srv := api.NewServer(cfg, logger)

	// JWT 발급 — --jwt-key 채워지면 login 이 access_token 발급 + refresh 활성.
	if cfg.JWTKeyFile != "" {
		iss, ver, err := auth.IssuerFromPrivateKeyFile(cfg.JWTKeyFile, "wtg-1")
		if err != nil {
			logger.Error("JWT private key 로드 실패", slog.String("path", cfg.JWTKeyFile), slog.Any("error", err))
			os.Exit(2)
		}
		srv.SetJWT(iss, ver)
		logger.Info("JWT 발급 활성", slog.String("key", cfg.JWTKeyFile))
	}

	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-api 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-api 정상 종료")
}

// newLogger 는 LogLevel 문자열에 따라 slog.Logger 를 생성한다.
// 운영에서는 JSON, 개발에서는 텍스트 핸들러로 시작 (둘 다 stdout).
