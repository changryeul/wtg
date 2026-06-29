// spot-load-gen — mci-price 의 GET /v1/quote/spot endpoint 부하 + latency
// percentile 측정 도구.
//
// 목적: mds 의 SHM 직접 read (μs) 대비 WTG 의 in-process conflation +
// HTTP REST round-trip 의 latency 가 SLA 안에 들어오는지 데이터로 확인.
//
// 사용:
//
//	./build/bin/spot-load-gen \
//	    --target http://127.0.0.1:8082 \
//	    --pair USD/KRW \
//	    --profile WEB.BRANCH.VIP \
//	    --rate 1000 --duration 30s --concurrency 32
//
// 출력: stdout 의 percentile 표 + 옵션 --csv 으로 raw latency CSV.
//
// 정확도 주의:
//   - GC pause / scheduler jitter 가 p99 / p999 에 영향. percentile 만 신뢰
//     (mean 은 outlier 에 약함).
//   - 같은 호스트 loopback 부하 — real network round-trip 시뮬 아님.
//     운영 latency 는 + (1~5 ms NIC + TLS handshake) 가산 필요.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		target      = flag.String("target", "http://127.0.0.1:8082", "mci-price base URL")
		pair        = flag.String("pair", "USD/KRW", "pair (콤마 구분 다중 가능)")
		profile     = flag.String("profile", "WEB.BRANCH.VIP", "profile key")
		customerID  = flag.String("customer-id", "", "(옵션) 5-Layer 적용 customer ID")
		rate        = flag.Int("rate", 1000, "초당 요청 수 (RPS 목표)")
		duration    = flag.Duration("duration", 30*time.Second, "측정 시간")
		concurrency = flag.Int("concurrency", 32, "병렬 client goroutine 수")
		timeout     = flag.Duration("timeout", 2*time.Second, "단일 요청 timeout")
		csvPath     = flag.String("csv", "", "(옵션) raw latency CSV 출력 경로")
	)
	flag.Parse()

	q := url.Values{}
	q.Set("pair", *pair)
	q.Set("profile", *profile)
	if *customerID != "" {
		q.Set("customer_id", *customerID)
	}
	reqURL := *target + "/v1/quote/spot?" + q.Encode()

	fmt.Printf("=== spot-load-gen ===\n")
	fmt.Printf("target      : %s\n", reqURL)
	fmt.Printf("rate goal   : %d req/s\n", *rate)
	fmt.Printf("duration    : %s\n", *duration)
	fmt.Printf("concurrency : %d goroutines\n", *concurrency)
	fmt.Println()

	// 1회 smoke 요청 — endpoint 가 살아있는지 확인.
	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	if err := smoke(client, reqURL); err != nil {
		fmt.Fprintf(os.Stderr, "smoke 요청 실패: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, *duration)
	defer deadline()

	// 각 client 가 RPS 에 맞춰 요청 발사. tick 단위로 균등 분산.
	perWorkerRate := float64(*rate) / float64(*concurrency)
	interval := time.Duration(float64(time.Second) / perWorkerRate)

	var (
		latencies = make([][]time.Duration, *concurrency)
		errCount  atomic.Int64
		okCount   atomic.Int64
		wg        sync.WaitGroup
	)
	for i := 0; i < *concurrency; i++ {
		latencies[i] = make([]time.Duration, 0, int(perWorkerRate*duration.Seconds())+128)
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			local := latencies[workerID][:0]
			for {
				select {
				case <-ctx.Done():
					latencies[workerID] = local
					return
				case <-tick.C:
					lat, err := doRequest(client, reqURL)
					if err != nil {
						errCount.Add(1)
						continue
					}
					okCount.Add(1)
					local = append(local, lat)
				}
			}
		}(i)
	}

	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)

	// 모든 latency 합쳐 정렬.
	all := make([]time.Duration, 0, int(okCount.Load()))
	for _, ls := range latencies {
		all = append(all, ls...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	ok := okCount.Load()
	er := errCount.Load()
	actualRPS := float64(ok) / elapsed.Seconds()

	fmt.Println("=== 결과 ===")
	fmt.Printf("elapsed     : %.2f s\n", elapsed.Seconds())
	fmt.Printf("ok / err    : %d / %d\n", ok, er)
	fmt.Printf("actual RPS  : %.0f (goal %d)\n", actualRPS, *rate)
	fmt.Println()
	fmt.Println("--- latency percentile ---")
	if len(all) > 0 {
		printPercentile("min   ", all[0])
		printPercentile("p50   ", all[len(all)*50/100])
		printPercentile("p90   ", all[len(all)*90/100])
		printPercentile("p95   ", all[len(all)*95/100])
		printPercentile("p99   ", all[len(all)*99/100])
		printPercentile("p99.9 ", all[len(all)*999/1000])
		printPercentile("max   ", all[len(all)-1])
	}

	if *csvPath != "" {
		if err := writeCSV(*csvPath, all); err != nil {
			fmt.Fprintf(os.Stderr, "csv 쓰기 실패: %v\n", err)
		} else {
			fmt.Printf("\nraw latency CSV: %s\n", *csvPath)
		}
	}
}

func smoke(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("smoke status=%d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func doRequest(c *http.Client, url string) (time.Duration, error) {
	t0 := time.Now()
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("status=%d", resp.StatusCode)
	}
	return time.Since(t0), nil
}

func printPercentile(label string, d time.Duration) {
	fmt.Printf("%s: %7.2f μs  (%6.3f ms)\n",
		label, float64(d.Microseconds()), float64(d.Microseconds())/1000)
}

func writeCSV(path string, all []time.Duration) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"index", "latency_ns", "latency_us"})
	for i, d := range all {
		_ = w.Write([]string{
			strconv.Itoa(i),
			strconv.FormatInt(int64(d), 10),
			strconv.FormatInt(d.Microseconds(), 10),
		})
	}
	return nil
}
