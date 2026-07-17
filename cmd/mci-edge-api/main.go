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
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	edgeapi "github.com/winwaysystems/wtg/internal/edge/api"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := edgeapi.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-api: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := logging.Init("mci-edge-api", logging.Options{Level: cfg.LogLevel})
	logger.Info("mci-edge-api 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamURL),
		slog.Bool("dev", cfg.DevMode),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if cfg.OtelEndpoint != "" || cfg.OtelStdout {
		ep := cfg.OtelEndpoint
		if cfg.OtelStdout {
			ep = "stdout"
		}
		shutdown, err := otelinit.Setup(ctx, otelinit.Options{
			ServiceName: "mci-edge-api",
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

	srv := edgeapi.NewServer(cfg, logger)
	// JWT 검증기 — 운영 인증 경로. public key 파일이 주어지면 Bearer 검증 활성.
	if cfg.JWTPublicKeyFile != "" {
		ver, err := auth.VerifierFromPublicKeyFile(cfg.JWTPublicKeyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mci-edge-api: JWT public key 로드 실패: %v\n", err)
			os.Exit(2)
		}
		srv.SetJWTVerifier(ver)
		logger.Info("JWT 검증 활성", slog.String("public_key", cfg.JWTPublicKeyFile))
	} else if !cfg.DevMode {
		logger.Warn("JWT public key 도 DevMode 도 아님 — 모든 요청이 401 로 거부됨")
	}
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-api 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-api 정상 종료")
}
