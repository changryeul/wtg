// mci-edge-tcp — 레거시 cs (HTS/EMP) 용 raw TCP 전문 gateway.
//
// 지속 TCP 연결 + [4B length][전문] 프레임 + 빈 프레임 heartbeat (mymq 컨벤션)
// 를 받아 내부 mci-api 의 raw 전문 모드 (POST /v1/tx octet-stream) 로 변환.
// 레거시 클라이언트는 접속 좌표만 바꾸면 무수정으로 WTG 를 경유한다.
//
// 사용 (dev):
//
//	mci-edge-tcp --listen :5021 --upstream http://127.0.0.1:8080 --api-user hts01
//
// 검증은 cmd/tcp-tester (연결 유지 + 주기 heartbeat + 전문 송수신) 로.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	edgetcp "github.com/winwaysystems/wtg/internal/edge/tcp"
)

func main() {
	var (
		listen   = flag.String("listen", ":5021", "raw TCP listen 주소")
		upstream = flag.String("upstream", "http://127.0.0.1:8080", "내부 mci-api base URL")
		apiUser  = flag.String("api-user", "", "DevMode X-WTG-User (dev 전용 — 운영은 --api-token)")
		apiToken = flag.String("api-token", "", "JWT access token (Authorization Bearer)")
		channel  = flag.String("channel", "HTS", "X-WTG-Channel 값 (HTS | EMP)")
		stats    = flag.String("stats", "", "진단 HTTP listen 주소 (예: 127.0.0.1:5022). 빈값=비활성")
		idle     = flag.Duration("idle-timeout", 90*time.Second, "무트래픽 (heartbeat 포함) 연결 종료 시간")
		selectIP = flag.String("select-server-ip", "", "cs select-server(FC 0x01) 응답 IP. 빈값=conn LocalAddr (cs 는 ip1==ip2 만 확인)")
		logLevel = flag.String("log-level", "info", "로그 레벨 (debug|info|warn|error)")
	)
	flag.Parse()

	logger := newLogger(*logLevel)
	srv, err := edgetcp.NewServer(edgetcp.Config{
		ListenAddr:     *listen,
		UpstreamURL:    *upstream,
		APIUser:        *apiUser,
		APIToken:       *apiToken,
		Channel:        *channel,
		StatsAddr:      *stats,
		IdleTimeout:    *idle,
		SelectServerIP: *selectIP,
	}, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mci-edge-tcp: config 에러: %v\n", err)
		os.Exit(2)
	}
	if *apiUser == "" && *apiToken == "" {
		logger.Warn("--api-user 도 --api-token 도 없음 — upstream 이 전 요청을 401 로 거부함")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		logger.Error("mci-edge-tcp 종료", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-edge-tcp 정상 종료")
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
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
