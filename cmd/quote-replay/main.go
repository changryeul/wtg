// quote-replay — mds manual_feed/replay_* 동등의 실 캡처 시세 재생기.
//
// 운영에서 저장한 .trc 캡처 (타임스탬프 prefix + FIX 4.4 원문 라인) 를 읽어
// 35=W/X 시세 메시지를 원본 간격 (또는 배속) 으로 UDP 재송신한다. 목적지를
// 여러 개 주면 mds cooker 와 WTG quote-forwarder 에 byte 동일한 입력을 동시에
// 넣을 수 있다 — 전환 계획 (docs/mds-replacement-plan.md) 의 "같은 입력,
// 같은 출력" 게이트 도구.
//
//	quote-replay --file SMB.trc --dest 127.0.0.1:30044,10.0.0.5:30044
//	quote-replay --file KMB.trc --dest 127.0.0.1:30045 --speed 10 --loop 0
package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
)

// replayConfig 는 1회 재생의 입력이다.
type replayConfig struct {
	FilePath string   // .trc 캡처 파일
	Dests    []string // UDP 목적지 (mds cooker + WTG forwarder 동시 송신). 빈 슬라이스 = dry-run
	Speed    float64  // 배속. 1=원본 타이밍, 0=페이싱 없이 최고속
}

// replayStats 는 1회 재생의 결과 통계다.
type replayStats struct {
	Lines   int // 읽은 라인 수
	Sent    int // 송신한 FIX 메시지 수
	Skipped int // 재생 대상 아님 (비 FIX / 55 없음 / 35=W·X 아님)
	Bytes   int // 송신 총 바이트 (목적지 1곳 기준)
}

// maxLine 은 .trc 한 줄의 상한 — FIX 전량 스냅샷 + 로그 prefix 여유분.
const maxLine = 1 << 20

func replayFile(cfg replayConfig) (replayStats, error) {
	var stats replayStats

	f, err := os.Open(cfg.FilePath)
	if err != nil {
		return stats, err
	}
	defer f.Close()

	conns := make([]net.Conn, 0, len(cfg.Dests))
	for _, dest := range cfg.Dests {
		conn, err := net.Dial("udp", dest)
		if err != nil {
			return stats, fmt.Errorf("dest %s: %w", dest, err)
		}
		defer conn.Close()
		conns = append(conns, conn)
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLine)

	var prev time.Duration
	havePrev := false
	for sc.Scan() {
		stats.Lines++
		line := sc.Bytes()

		msg := extractFIX(line)
		if msg == nil {
			stats.Skipped++
			continue
		}

		if ts, ok := parseStamp(line); ok {
			if havePrev {
				if d := paceDelay(prev, ts, cfg.Speed); d > 0 {
					time.Sleep(d)
				}
			}
			prev, havePrev = ts, true
		}

		for _, conn := range conns {
			if _, err := conn.Write(msg); err != nil {
				return stats, fmt.Errorf("send %s: %w", conn.RemoteAddr(), err)
			}
		}
		stats.Sent++
		stats.Bytes += len(msg)
	}
	return stats, sc.Err()
}

func main() {
	file := flag.String("file", "", ".trc 캡처 파일 (필수)")
	destStr := flag.String("dest", "127.0.0.1:30044",
		"UDP 목적지, 콤마 구분 다중 지정 — mds cooker 와 WTG forwarder 동시 송신용")
	speed := flag.Float64("speed", 1.0, "배속 (1=원본 타이밍, 0=페이싱 없이 최고속)")
	loop := flag.Int("loop", 1, "반복 횟수 (0=무한)")
	dryRun := flag.Bool("dry-run", false, "송신 없이 파싱/필터 통계만")
	flag.Parse()

	logger := logging.Init("quote-replay", logging.Options{})

	if *file == "" {
		flag.Usage()
		os.Exit(2)
	}

	var dests []string
	if !*dryRun {
		for _, d := range strings.Split(*destStr, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dests = append(dests, d)
			}
		}
	}

	for i := 1; ; i++ {
		start := time.Now()
		stats, err := replayFile(replayConfig{FilePath: *file, Dests: dests, Speed: *speed})
		if err != nil {
			logger.Error("replay 실패", slog.String("file", *file), slog.Any("error", err))
			os.Exit(1)
		}
		logger.Info("pass 완료",
			slog.Int("pass", i), slog.Any("lines", stats.Lines), slog.Any("sent", stats.Sent),
			slog.Any("skipped", stats.Skipped), slog.Any("bytes", stats.Bytes),
			slog.Duration("elapsed", time.Since(start).Round(time.Millisecond)), slog.Int("dests", len(dests)))
		if *loop != 0 && i >= *loop {
			break
		}
	}
}
