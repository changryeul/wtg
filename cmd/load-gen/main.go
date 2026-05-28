// Command load-gen — mci-price 파이프라인 부하 테스트 생성기.
//
// 사용:
//
//	load-gen --rate 100 --duration 30s
//	load-gen --pairs USDKRW,EURUSD,USDJPY,GBPUSD,AUDUSD --rate 500 --duration 60s
//
// 동작:
//
//	(feed × pair) 개 goroutine 이 각각 --rate tick/sec 으로 UDP FIX 35=W
//	snapshot 패킷을 quote-forwarder 의 UDP 포트로 송신. 총 throughput =
//	feeds × pairs × rate.
//
//	주기적으로 mci-price 의 /v1/price-stats /v1/best-stats 를 폴링해
//	종단 카운터 (received/matched/dropped, BestConsumer cache) 를 수집,
//	stdout 에 진행 라인 + 종료 시 요약 + (옵션) CSV 출력.
//
// 한계 / 주의:
//   - Go time.Ticker 정밀도 한계로 매우 높은 rate (예: >5000/stream) 에선
//     실제 송신율이 약간 낮을 수 있음. 배치 송신으로 완화.
//   - kernel UDP send buffer 가 작으면 send error 가 발생 — `--report` 라인의
//     err 카운터로 확인.
//   - --target 은 본 머신만 가정 (127.0.0.1). 원격으로 부하 줄 때 ports 도 함께.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── feed → UDP port 매핑 (quote-forwarder --multi 의 컨벤션 기본값과 일치) ──
type feedSpec struct {
	name string
	port int
}

func parseFeeds(spec string) ([]feedSpec, error) {
	out := []feedSpec{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colon := strings.IndexByte(part, ':')
		if colon < 0 {
			return nil, fmt.Errorf("feed spec %q: name:port 형식 필요", part)
		}
		port, err := strconv.Atoi(part[colon+1:])
		if err != nil {
			return nil, fmt.Errorf("feed spec %q: port 파싱: %w", part, err)
		}
		out = append(out, feedSpec{name: part[:colon], port: port})
	}
	return out, nil
}

// ── 통화쌍 기본 가격 (사실적인 시뮬레이션을 위해) ──
func basePrice(pair string) float64 {
	switch pair {
	case "USDKRW":
		return 1380.50
	case "EURUSD":
		return 1.0850
	case "USDJPY":
		return 156.20
	case "GBPUSD":
		return 1.2740
	case "AUDUSD":
		return 0.6630
	case "EURKRW":
		return 1500.20
	case "JPYKRW":
		return 8.85
	case "GBPKRW":
		return 1759.30
	default:
		return 100.00
	}
}

// ── FIX 35=W snapshot 패킷 빌더 (burst 와 동일 wire) ──
const fixSOH = "\x01"

func buildFIX(feed, pair string, bid, ask float64) []byte {
	bidS := strconv.FormatFloat(bid, 'f', 4, 64)
	askS := strconv.FormatFloat(ask, 'f', 4, 64)
	return []byte(strings.Join([]string{
		"8=FIX.4.4", "9=80", "35=W", "49=" + feed, "56=SUB", "55=" + pair,
		"268=2", "269=0", "270=" + bidS, "271=1000000",
		"269=1", "270=" + askS, "271=1500000", "10=000", "",
	}, fixSOH))
}

// ── price-stats / best-stats 응답 모델 ──
type priceStats struct {
	Received uint64 `json:"received"`
	Matched  uint64 `json:"matched"`
	Dropped  uint64 `json:"dropped"`
	Conf     struct {
		Symbols uint64 `json:"Symbols"`
		Updates uint64 `json:"Updates"`
		Swaps   uint64 `json:"Swaps"`
	} `json:"conflation"`
}

type bestStats struct {
	Symbols map[string]struct {
		ActiveSources  int     `json:"active_sources"`
		BestBid        float64 `json:"best_bid"`
		BestAsk        float64 `json:"best_ask"`
		CrossedFallbck bool    `json:"crossed_fallback,omitempty"`
	} `json:"symbols"`
}

func fetchJSON(url string, out any) error {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── sample — 한 시점의 카운터 스냅샷 ──
type sample struct {
	T          time.Time
	Sent       uint64
	SendErrs   uint64
	Recv       uint64
	Match      uint64
	Drop       uint64
	ConfSwaps  uint64
	CrossCount int // best-stats 의 crossed_fallback=true symbol 수
}

func main() {
	target := flag.String("target", "127.0.0.1", "UDP host (보통 본 머신)")
	feedSpecStr := flag.String("feeds", "SMB:30044,KMB:30045,EBS:30046,REUT:30051",
		"feed:port comma — quote-forwarder --multi 와 동일 컨벤션")
	pairsStr := flag.String("pairs", "USDKRW,EURUSD,USDJPY,GBPUSD",
		"comma-separated 통화쌍")
	rate := flag.Int("rate", 100, "stream 당 ticks/sec (stream = feed × pair)")
	duration := flag.Duration("duration", 30*time.Second, "총 실행 시간")
	sampleInt := flag.Duration("sample", 5*time.Second, "stats 폴링 주기")
	priceStatsURL := flag.String("stats", "http://127.0.0.1:8082/v1/price-stats",
		"mci-price stats endpoint (빈값=skip)")
	bestStatsURL := flag.String("best", "http://127.0.0.1:8082/v1/best-stats",
		"best-stats endpoint (빈값=skip)")
	csvPath := flag.String("csv", "", "샘플 CSV 출력 경로 (빈값=stdout 만)")
	flag.Parse()

	feeds, err := parseFeeds(*feedSpecStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(2)
	}
	pairs := []string{}
	for _, p := range strings.Split(*pairsStr, ",") {
		if p = strings.TrimSpace(p); p != "" {
			pairs = append(pairs, p)
		}
	}
	streams := len(feeds) * len(pairs)
	totalRate := streams * *rate
	fmt.Printf("load-gen: %d streams (%d feeds × %d pairs) × %d tick/s/stream = %d tick/s 총\n",
		streams, len(feeds), len(pairs), *rate, totalRate)
	fmt.Printf("duration=%s sample=%s\n\n", *duration, *sampleInt)

	// UDP 연결 — feed 당 1개. multiple goroutine 이 같은 conn 으로 쓰지만
	// UDP DialUDP 결과는 짧은 메시지에 thread-safe (Linux/macOS).
	conns := map[string]*net.UDPConn{}
	for _, f := range feeds {
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", *target, f.port))
		if err != nil {
			fmt.Fprintln(os.Stderr, "ResolveUDPAddr:", err)
			os.Exit(2)
		}
		c, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "DialUDP:", err)
			os.Exit(2)
		}
		defer c.Close()
		conns[f.name] = c
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── baseline 카운터 — delta 계산용 (이번 test 가 mci-price 에 미친 효과만) ──
	var baseRecv, baseDrop uint64
	if *priceStatsURL != "" {
		var ps priceStats
		if err := fetchJSON(*priceStatsURL, &ps); err == nil {
			baseRecv = ps.Received
			baseDrop = ps.Dropped
		}
	}

	var (
		sent     atomic.Uint64
		sendErrs atomic.Uint64
		wg       sync.WaitGroup
	)

	// ── 송신 goroutine: (feed × pair) 당 1개. ──
	//
	// rate 별 정밀도 보정:
	//   rate ≤ 1000  → period = 1s/rate, 매 tick 1 packet (정확)
	//   rate > 1000  → period = 10ms, batch = rate/100 (Go ticker 한계)
	tickPeriod := 10 * time.Millisecond
	batchPerTick := *rate / 100
	if *rate <= 1000 {
		tickPeriod = time.Second / time.Duration(*rate)
		batchPerTick = 1
	}
	if batchPerTick < 1 {
		batchPerTick = 1
	}

	for _, f := range feeds {
		conn := conns[f.name]
		for _, pair := range pairs {
			wg.Add(1)
			go func(feed, pair string) {
				defer wg.Done()
				px := basePrice(pair)
				r := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(len(feed)*len(pair))))
				t := time.NewTicker(tickPeriod)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						for i := 0; i < batchPerTick; i++ {
							bid := px - 0.02
							ask := px + 0.02
							if _, err := conn.Write(buildFIX(feed, pair, bid, ask)); err != nil {
								sendErrs.Add(1)
							} else {
								sent.Add(1)
							}
							// random walk (±0.01)
							px += (r.Float64() - 0.5) * 0.02
						}
					}
				}
			}(f.name, pair)
		}
	}

	// ── sampling goroutine ──
	samples := []sample{}
	var smu sync.Mutex
	go func() {
		t := time.NewTicker(*sampleInt)
		defer t.Stop()
		var prev sample
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s := sample{
					T:        now,
					Sent:     sent.Load(),
					SendErrs: sendErrs.Load(),
				}
				if *priceStatsURL != "" {
					var ps priceStats
					if err := fetchJSON(*priceStatsURL, &ps); err == nil {
						s.Recv = ps.Received
						s.Match = ps.Matched
						s.Drop = ps.Dropped
						s.ConfSwaps = ps.Conf.Swaps
					}
				}
				if *bestStatsURL != "" {
					var bs bestStats
					if err := fetchJSON(*bestStatsURL, &bs); err == nil {
						for _, sym := range bs.Symbols {
							if sym.CrossedFallbck {
								s.CrossCount++
							}
						}
					}
				}
				dt := sampleInt.Seconds()
				sentRate := float64(s.Sent-prev.Sent) / dt
				recvRate := float64(s.Recv-prev.Recv) / dt
				dropRate := float64(s.Drop-prev.Drop) / dt
				dropPct := 0.0
				if s.Recv > 0 {
					dropPct = float64(s.Drop) / float64(s.Recv) * 100
				}
				fmt.Printf("[%s] sent=%-8d (%.0f/s) recv=%-8d (%.0f/s) drop=%-6d (%.0f/s, %.1f%%) err=%d cross=%d\n",
					now.Format("15:04:05"), s.Sent, sentRate, s.Recv, recvRate, s.Drop, dropRate, dropPct,
					s.SendErrs, s.CrossCount)
				smu.Lock()
				samples = append(samples, s)
				smu.Unlock()
				prev = s
			}
		}
	}()

	// duration 후 종료
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, *duration)
	defer timeoutCancel()
	<-timeoutCtx.Done()
	cancel()
	wg.Wait()

	// ── 최종 요약 ──
	finalSent := sent.Load()
	finalErrs := sendErrs.Load()
	achievedRate := float64(finalSent) / duration.Seconds()
	achievedPct := achievedRate / float64(totalRate) * 100

	var finalRecv, finalDrop uint64
	if *priceStatsURL != "" {
		var ps priceStats
		if err := fetchJSON(*priceStatsURL, &ps); err == nil {
			finalRecv = ps.Received
			finalDrop = ps.Dropped
		}
	}
	deltaRecv := finalRecv - baseRecv
	deltaDrop := finalDrop - baseDrop
	deliveryPct := 0.0
	if finalSent > 0 {
		deliveryPct = float64(deltaRecv) / float64(finalSent) * 100
	}
	dropPct := 0.0
	if deltaRecv > 0 {
		dropPct = float64(deltaDrop) / float64(deltaRecv) * 100
	}

	fmt.Println("\n=== Summary ===")
	fmt.Printf("Duration:         %s\n", *duration)
	fmt.Printf("Target rate:      %d tick/s 총 (%d stream × %d/stream)\n", totalRate, streams, *rate)
	fmt.Printf("Achieved rate:    %.0f tick/s (%.1f%% of target)\n", achievedRate, achievedPct)
	fmt.Printf("Sent (UDP):       %d (errs %d)\n", finalSent, finalErrs)
	fmt.Printf("Received Δ:       %d (delivery %.1f%% of sent — Δ from price-stats)\n", deltaRecv, deliveryPct)
	fmt.Printf("Dropped Δ:        %d (%.2f%% of Δ recv)\n", deltaDrop, dropPct)

	// CSV 출력 — 시간별 샘플. 다른 run 과 비교 시 외부 툴로 처리.
	if *csvPath != "" {
		smu.Lock()
		defer smu.Unlock()
		f, err := os.Create(*csvPath)
		if err == nil {
			defer f.Close()
			fmt.Fprintln(f, "ts,sent,send_errs,recv,match,drop,conf_swaps,cross_count")
			for _, s := range samples {
				fmt.Fprintf(f, "%s,%d,%d,%d,%d,%d,%d,%d\n",
					s.T.Format(time.RFC3339Nano),
					s.Sent, s.SendErrs, s.Recv, s.Match, s.Drop, s.ConfSwaps, s.CrossCount)
			}
			fmt.Printf("CSV:              %s (%d samples)\n", *csvPath, len(samples))
		}
	}
}
