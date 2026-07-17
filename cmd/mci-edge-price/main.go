// mci-edge-price 는 WTG 의 DMZ 측 시세 fan-out edge.
//
// Internal mci-price 의 PriceService gRPC stream 에 접속해서 Tick 을 받고,
// DMZ 측 WebSocket 클라이언트들에게 broadcast 한다.
//
// 사용 예 (기본 — raw tick broadcast 만):
//
//	mci-edge-price --listen=:8083 --upstream=internal-host:50051 --dev=true
//
// Profile-routed quote (마진 적용):
//
//	mci-edge-price ... --quote-stream
//
// Phase 4c — customer-specific 마진 적용 quote (HQ+Site+Customer+Window):
//
//	mci-edge-price ... --quote-stream --customer-stream
//	  ws connect 시 Principal.Usid 를 customer-id 로 mci-price 에 자동 등록.
//	  disconnect 시 자동 해제. mci-price 의 ApplyForCustomer 결과를 본
//	  edge 가 SubscribeCustomerQuote stream 으로 받아 매칭 ws 로 fan-out.
//
// 환경변수: WTG_EPRICE_QUOTE_STREAM, WTG_EPRICE_CUSTOMER_STREAM (둘 다 "1"/"true").
package main

import (
	"context"
	"fmt"
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	edgeprice "github.com/winwaysystems/wtg/internal/edge/price"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/otelinit"
)

func main() {
	cfg, err := edgeprice.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-price: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := logging.Init("mci-edge-price", logging.Options{Level: cfg.LogLevel})
	logger.Info("mci-edge-price 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("upstream", cfg.UpstreamGRPC),
		slog.String("subscriber_id", cfg.SubscriberID),
		slog.Bool("dev", cfg.DevMode),
		slog.Bool("quote_stream", cfg.EnableQuoteStream),
		slog.Bool("customer_stream", cfg.EnableCustomerStream),
		slog.Any("quote_profiles", cfg.QuoteProfileKeys),
		slog.String("envelope_format", cfg.EnvelopeFormat),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if shutdown := otelinit.SetupIfEnabled(ctx, "mci-edge-price",
		cfg.OtelEndpoint, cfg.OtelStdout, cfg.OtelInsecure, cfg.OtelSampleRatio,
		logger); shutdown != nil {
		defer shutdown(ctx)
	}

	srv := edgeprice.NewServer(cfg, logger)

	// JWT 검증 — --jwt-pub 채워지면 ws 가 access_token 을 RS256 검증.
	if cfg.JWTPubFile != "" {
		ver, err := auth.VerifierFromPublicKeyFile(cfg.JWTPubFile)
		if err != nil {
			logger.Error("JWT public key 로드 실패", slog.String("path", cfg.JWTPubFile), slog.Any("error", err))
			os.Exit(2)
		}
		srv.SetJWTVerifier(ver)
		logger.Info("JWT 검증 활성", slog.String("pub", cfg.JWTPubFile))
	}

	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-price 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-price 정상 종료")
}
