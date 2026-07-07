// ws-load-gen — mci-edge-price 의 ws fan-out burst 측정 도구.
//
// 목적: 환율 급변 시 시세 양 폭증 + 동시 ws 고객 N 명의 운영을 시뮬레이션.
// fan-out 측의 backpressure / slow consumer 격리 동작 + per-client 수신 rate
// 가 publisher rate 와 일치하는지 정량 측정.
//
// tick 부하 자체는 별도 — 본 도구는 ws consumer 만 책임. mci-edge-price 가
// 이미 mci-price 로부터 quote stream 을 받고 있다고 가정 (예: dev tickloop /
// load-test.sh 가동 중).
//
// 사용:
//
//	./build/bin/ws-load-gen \
//	    --target ws://127.0.0.1:8083/v1/subscribe?profile=WEB.BRANCH.VIP \
//	    --clients 100 --duration 30s \
//	    --user-prefix test --channel WEB --site BRANCH --tier VIP \
//	    --subscribe USD/KRW,EUR/USD
//
// 출력: stdout 의 per-scenario 표 + 옵션 --csv 로 per-client raw rate.
//
// 정확도 주의:
//   - macOS ulimit -n (default 256) 가 작아 1000+ 동시 연결은 raise 필요:
//     ulimit -n 4096
//   - inter-arrival latency 는 OS 의 ms 해상도 timer 기준 — sub-ms 측정엔 부정확.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	var (
		target      = flag.String("target", "ws://127.0.0.1:8083/v1/subscribe", "mci-edge-price ws URL")
		clients     = flag.Int("clients", 100, "동시 ws client 수")
		duration    = flag.Duration("duration", 30*time.Second, "측정 시간")
		userPrefix  = flag.String("user-prefix", "wsload", "X-WTG-User 헤더 prefix (실제: <prefix>-<idx>)")
		channel     = flag.String("channel", "WEB", "X-WTG-Channel 헤더 (DevMode)")
		site        = flag.String("site", "BRANCH", "X-WTG-Site 헤더")
		tier        = flag.String("tier", "VIP", "X-WTG-Tier 헤더")
		subscribe   = flag.String("subscribe", "", "ws upgrade 직후 subscribe 할 pair (콤마 구분, 비면 all 모드)")
		connTimeout = flag.Duration("conn-timeout", 5*time.Second, "단일 connect timeout")
		csvPath     = flag.String("csv", "", "(옵션) per-client rate CSV")
		warmup      = flag.Duration("warmup", 2*time.Second, "측정 시작 전 warmup (전체 연결 완료 + tick 안정)")
	)
	flag.Parse()

	fmt.Printf("=== ws-load-gen ===\n")
	fmt.Printf("target      : %s\n", *target)
	fmt.Printf("clients     : %d\n", *clients)
	fmt.Printf("duration    : %s (+ warmup %s)\n", *duration, *warmup)
	fmt.Printf("subscribe   : %q (빈값=all)\n", *subscribe)
	fmt.Println()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stats := make([]*clientStats, *clients)
	for i := range stats {
		stats[i] = &clientStats{id: i}
	}

	// 모든 client 연결 + subscribe 까지.
	fmt.Printf("--- 연결 중 (%d) ---\n", *clients)
	var connectFails atomic.Int64
	var connectWG sync.WaitGroup
	for i := 0; i < *clients; i++ {
		connectWG.Add(1)
		go func(idx int) {
			defer connectWG.Done()
			c, err := dialWS(*target, *userPrefix, idx, *channel, *site, *tier, *connTimeout)
			if err != nil {
				connectFails.Add(1)
				stats[idx].connectErr = err.Error()
				return
			}
			stats[idx].conn = c
			if *subscribe != "" {
				m := map[string]any{"type": "subscribe", "pairs": splitCSV(*subscribe)}
				body, _ := json.Marshal(m)
				_ = c.WriteMessage(websocket.TextMessage, body)
			}
		}(i)
	}
	connectWG.Wait()
	connected := *clients - int(connectFails.Load())
	fmt.Printf("연결 성공: %d / %d (실패 %d)\n", connected, *clients, connectFails.Load())
	if connected == 0 {
		fmt.Fprintln(os.Stderr, "연결 모두 실패 — 종료")
		os.Exit(1)
	}

	// warmup — 전체 연결 stabilize.
	time.Sleep(*warmup)

	// 측정 시작.
	measureCtx, measureCancel := context.WithTimeout(ctx, *duration)
	defer measureCancel()
	measureStart := time.Now()
	fmt.Printf("--- 측정 시작 (%s) ---\n", *duration)

	var recvWG sync.WaitGroup
	for i := 0; i < *clients; i++ {
		if stats[i].conn == nil {
			continue
		}
		recvWG.Add(1)
		go func(idx int) {
			defer recvWG.Done()
			runRecvLoop(measureCtx, stats[idx])
		}(i)
	}
	recvWG.Wait()
	elapsed := time.Since(measureStart)

	// 모든 ws close.
	for _, s := range stats {
		if s.conn != nil {
			_ = s.conn.Close()
		}
	}

	// 결과 집계.
	reportResults(stats, elapsed, *clients, connected)
	if *csvPath != "" {
		if err := writeClientCSV(*csvPath, stats, elapsed); err != nil {
			fmt.Fprintf(os.Stderr, "csv 쓰기 실패: %v\n", err)
		} else {
			fmt.Printf("\nper-client CSV: %s\n", *csvPath)
		}
	}
}

type clientStats struct {
	id         int
	conn       *websocket.Conn
	msgCount   int64
	bytesRecv  int64
	firstRecv  time.Time
	lastRecv   time.Time
	disconnect bool
	disconErr  string
	connectErr string
}

func dialWS(target, userPrefix string, idx int, channel, site, tier string, timeout time.Duration) (*websocket.Conn, error) {
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = timeout
	headers := http.Header{}
	headers.Set("X-WTG-User", fmt.Sprintf("%s-%d", userPrefix, idx))
	headers.Set("X-WTG-Channel", channel)
	headers.Set("X-WTG-Site", site)
	headers.Set("X-WTG-Tier", tier)
	c, resp, err := d.Dial(target, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial: %w (status %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	return c, nil
}

func runRecvLoop(ctx context.Context, s *clientStats) {
	s.conn.SetReadLimit(64 * 1024)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.disconnect = true
			s.disconErr = err.Error()
			return
		}
		now := time.Now()
		if s.firstRecv.IsZero() {
			s.firstRecv = now
		}
		s.lastRecv = now
		atomic.AddInt64(&s.msgCount, 1)
		atomic.AddInt64(&s.bytesRecv, int64(len(data)))
	}
}

func reportResults(stats []*clientStats, elapsed time.Duration, total, connected int) {
	var (
		totalMsgs       int64
		totalBytes      int64
		ratesPerSec     []float64
		disconnectCount int
		zeroRecvCount   int
	)
	for _, s := range stats {
		if s.conn == nil {
			continue
		}
		totalMsgs += s.msgCount
		totalBytes += s.bytesRecv
		if s.disconnect {
			disconnectCount++
		}
		if s.msgCount == 0 {
			zeroRecvCount++
		} else {
			// per-client receive duration — firstRecv 부터 lastRecv 까지의 흐름.
			d := s.lastRecv.Sub(s.firstRecv)
			if d <= 0 {
				d = elapsed
			}
			rate := float64(s.msgCount) / d.Seconds()
			ratesPerSec = append(ratesPerSec, rate)
		}
	}

	fmt.Println()
	fmt.Println("=== 결과 ===")
	fmt.Printf("elapsed                : %.2f s\n", elapsed.Seconds())
	fmt.Printf("clients goal/conn/recv : %d / %d / %d\n", total, connected, connected-zeroRecvCount)
	fmt.Printf("disconnect             : %d (slow consumer 격리 또는 server-side close)\n", disconnectCount)
	fmt.Printf("total msgs             : %d\n", totalMsgs)
	fmt.Printf("total bytes            : %.2f MB\n", float64(totalBytes)/1e6)
	if connected > 0 {
		fmt.Printf("avg msgs / client      : %.1f\n", float64(totalMsgs)/float64(connected))
	}
	if elapsed > 0 {
		fmt.Printf("aggregate msg rate     : %.0f msg/s (모든 client 합)\n", float64(totalMsgs)/elapsed.Seconds())
		fmt.Printf("aggregate bandwidth    : %.2f MB/s\n", float64(totalBytes)/elapsed.Seconds()/1e6)
	}

	if len(ratesPerSec) > 0 {
		// per-client receive rate 의 distribution.
		sortFloats(ratesPerSec)
		fmt.Println()
		fmt.Println("--- per-client receive rate (msg/s) ---")
		fmt.Printf("min   : %8.1f\n", ratesPerSec[0])
		fmt.Printf("p50   : %8.1f\n", ratesPerSec[len(ratesPerSec)*50/100])
		fmt.Printf("p90   : %8.1f\n", ratesPerSec[len(ratesPerSec)*90/100])
		fmt.Printf("p95   : %8.1f\n", ratesPerSec[len(ratesPerSec)*95/100])
		fmt.Printf("p99   : %8.1f\n", ratesPerSec[len(ratesPerSec)*99/100])
		fmt.Printf("max   : %8.1f\n", ratesPerSec[len(ratesPerSec)-1])
	}
}

func sortFloats(xs []float64) {
	// 작은 N (≤ clients) 라 단순 정렬 OK.
	for i := 1; i < len(xs); i++ {
		v := xs[i]
		j := i - 1
		for j >= 0 && xs[j] > v {
			xs[j+1] = xs[j]
			j--
		}
		xs[j+1] = v
	}
}

func splitCSV(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func writeClientCSV(path string, stats []*clientStats, elapsed time.Duration) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"client_id", "msg_count", "bytes", "rate_msg_per_sec", "disconnected", "error"})
	for _, s := range stats {
		rate := float64(0)
		if s.msgCount > 0 && elapsed > 0 {
			rate = float64(s.msgCount) / elapsed.Seconds()
		}
		errStr := ""
		if s.disconnect {
			errStr = s.disconErr
		} else if s.connectErr != "" {
			errStr = s.connectErr
		}
		_ = w.Write([]string{
			strconv.Itoa(s.id),
			strconv.FormatInt(s.msgCount, 10),
			strconv.FormatInt(s.bytesRecv, 10),
			fmt.Sprintf("%.2f", rate),
			strconv.FormatBool(s.disconnect),
			errStr,
		})
	}
	return nil
}
