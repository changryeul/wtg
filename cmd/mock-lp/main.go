// Command mock-lp — 시나리오 기반 mock LP(유동성공급자) 시세 생성기.
//
// LP(원천: SMB/KMB/EBS/CMB…)별 **결정적** 호가/체결을 FIX 4.4 35=W 로 UDP 송신해
// quote-forwarder → BestConsumer → CrossRateConsumer → AlgoStream 경로를 값까지
// 결정적으로 e2e 검증한다. (load-gen 은 랜덤 부하용, mock-lp 는 시나리오 검증용.)
//
// 사용:
//
//	# 내장 데모 시나리오 1회 송신 (BEST/cross/last 를 한 번에 자극)
//	mock-lp --once
//
//	# 시나리오 파일을 200ms 주기로 반복
//	mock-lp --scenario scn.json --interval 200ms
//
//	# LP→포트 매핑 지정 (quote-forwarder --multi 컨벤션)
//	mock-lp --feeds SMB:127.0.0.1:30044,KMB:127.0.0.1:30045 --once
//
// 시나리오 JSON:
//
//	{"quotes":[
//	  {"lp":"SMB","pair":"USDKRW","bid":1380.10,"ask":1380.20,"last":1380.15},
//	  {"lp":"KMB","pair":"USDKRW","bid":1380.05,"ask":1380.30},
//	  {"lp":"SMB","pair":"USDCNH","bid":7.10,"ask":7.11}
//	]}
package main

import (
	"flag"
	"fmt"
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// defaultScenario — BEST(SMB bid 우위 / KMB ask 우위) + 체결(SMB USDKRW last) +
// cross(USDCNH 로 CNH/KRW 합성) 를 한 번에 자극하는 내장 데모.
const defaultScenario = `{"quotes":[
  {"lp":"SMB","pair":"USDKRW","bid":1380.10,"ask":1380.25,"last":1380.15,"last_qty":500000},
  {"lp":"KMB","pair":"USDKRW","bid":1380.05,"ask":1380.20},
  {"lp":"SMB","pair":"USDCNH","bid":7.1000,"ask":7.1030},
  {"lp":"KMB","pair":"USDCNH","bid":7.0995,"ask":7.1025}
]}`

func main() {
	feedsSpec := flag.String("feeds",
		"SMB:127.0.0.1:30044,KMB:127.0.0.1:30045,EBS:127.0.0.1:30046,CMB:127.0.0.1:30047",
		"LP→UDP dest 매핑 (LP:host:port,…) — quote-forwarder --multi 컨벤션")
	scenarioPath := flag.String("scenario", "", "시나리오 JSON 파일 (빈값=내장 데모)")
	interval := flag.Duration("interval", 0, "반복 주기 (0=1회 송신)")
	count := flag.Int("count", 0, "반복 횟수 (0=interval>0 이면 무한)")
	once := flag.Bool("once", false, "1회만 송신 (--interval 무시)")
	flag.Parse()

	logger := logging.Init("mock-lp", logging.Options{})

	feeds, err := parseFeeds(*feedsSpec)
	if err != nil {
		logger.Error("--feeds 파싱 실패", slog.Any("error", err))
		os.Exit(1)
	}

	raw := []byte(defaultScenario)
	if *scenarioPath != "" {
		raw, err = os.ReadFile(*scenarioPath)
		if err != nil {
			logger.Error("시나리오 읽기 실패", slog.Any("error", err))
			os.Exit(1)
		}
	}
	sc, err := parseScenario(raw)
	if err != nil {
		logger.Error("초기화 실패", slog.Any("error", err))
		os.Exit(1)
	}
	if len(sc.Quotes) == 0 {
		logger.Error("시나리오에 quote 없음")
		os.Exit(1)
	}

	// dest 별 UDP conn 준비 (재사용).
	conns := map[string]*net.UDPConn{}
	for lp, dest := range feeds {
		addr, err := net.ResolveUDPAddr("udp", dest)
		if err != nil {
			logger.Error("dest 해석 실패", slog.String("dest", dest), slog.String("lp", lp), slog.Any("error", err))
			os.Exit(1)
		}
		c, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			logger.Error("dial 실패", slog.String("dest", dest), slog.Any("error", err))
			os.Exit(1)
		}
		defer c.Close()
		conns[lp] = c
	}

	// 시나리오에 있으나 --feeds 에 없는 LP 경고 (오타/누락 조기 발견).
	for _, q := range sc.Quotes {
		if _, ok := feeds[q.LP]; !ok {
			logger.Warn("LP 가 --feeds 에 없음 — quote skip", slog.String("lp", q.LP), slog.String("pair", q.Pair))
		}
	}

	send := func() (int, int) {
		sent, skipped := 0, 0
		for _, q := range sc.Quotes {
			c, ok := conns[q.LP]
			if !ok {
				skipped++
				continue
			}
			if _, err := c.Write(buildSnapshot(q)); err != nil {
				logger.Warn("송신 실패", slog.String("lp", q.LP), slog.String("pair", q.Pair), slog.Any("error", err))
				skipped++
				continue
			}
			sent++
		}
		return sent, skipped
	}

	if *once || *interval <= 0 {
		sent, skipped := send()
		fmt.Printf("mock-lp: 1회 송신 완료 — sent=%d skipped=%d (LP: %s)\n",
			sent, skipped, feedsList(feeds))
		return
	}

	ctx := make(chan os.Signal, 1)
	signal.Notify(ctx, os.Interrupt, syscall.SIGTERM)
	t := time.NewTicker(*interval)
	defer t.Stop()
	fmt.Printf("mock-lp: %s 주기 반복 송신 (count=%d, 0=무한). Ctrl-C 종료.\n", *interval, *count)
	rounds := 0
	totalSent := 0
	for {
		select {
		case <-ctx:
			fmt.Printf("\nmock-lp: 종료 — rounds=%d total_sent=%d\n", rounds, totalSent)
			return
		case <-t.C:
			s, _ := send()
			totalSent += s
			rounds++
			if *count > 0 && rounds >= *count {
				fmt.Printf("mock-lp: %d rounds 완료 — total_sent=%d\n", rounds, totalSent)
				return
			}
		}
	}
}

func feedsList(feeds map[string]string) string {
	out := ""
	for lp, d := range feeds {
		if out != "" {
			out += ", "
		}
		out += lp + "→" + d
	}
	return out
}
