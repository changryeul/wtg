// mci-price 는 WTG (Winway Trading Gateway) 의 FX 시세 fan-out 서비스.
//
// MyMQ broker 의 PRICE exchange broadcast 를 unsolicited 모드로 받아서
// 심볼별 conflation 후 다운스트림(edge gRPC stream) 으로 fan-out 한다.
//
// Phase 4 1차: stdout dump TickConsumer 로 동작 검증. Phase 5 에서 gRPC
// 스트림 consumer 로 전환.
//
// 사용 예:
//
//	mci-price --listen=:8082 --broker-host=10.0.0.10 --queue=mci_price \
//	          --exchange=PRICE --print=10
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/winwaysystems/wtg/internal/price"
)

func main() {
	cfg, err := price.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-price: config 에러: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("mci-price 부팅",
		slog.String("listen", cfg.ListenAddr),
		slog.String("broker", fmt.Sprintf("%s:%d", cfg.BrokerHost, cfg.BrokerPort)),
		slog.String("queue", cfg.QueueName),
		slog.String("exchange", cfg.ExchangeName),
		slog.Int("print", cfg.PrintFirstN),
	)

	srv := price.NewServer(cfg, logger)

	// 1차 prototype: stdout dump consumer (처음 N 개 tick 만 출력).
	if cfg.PrintFirstN > 0 {
		var printed atomic.Int32
		srv.AddConsumer(price.TickConsumerFunc(func(t *price.Tick) {
			if printed.Load() >= int32(cfg.PrintFirstN) {
				return
			}
			n := printed.Add(1)
			if n > int32(cfg.PrintFirstN) {
				return
			}
			fmt.Printf("[tick %d] mkid=%d symbol=%q seq=%d type=%d body_len=%d\n",
				n, t.MarketID, t.Symbol, t.SeqNum, t.Type, len(t.Body))
		}))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-price 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-price 정상 종료")
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
