// algo-tester — AlgoStream smoke tester.
//
// mci-price 의 PriceService.SubscribeAlgo 를 호출해 latest tick 을 stdout 으로
// dump. Phase A 검증용. Phase B (backfill) 완성 시 --from-seq 옵션도 활성.
//
// 사용:
//
//	./build/bin/algo-tester \
//	    --target 127.0.0.1:50051 \
//	    --client-id hedge-bot-01 \
//	    --symbols USDKRW,EURUSD \
//	    --duration 30s
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func main() {
	var (
		target    = flag.String("target", "127.0.0.1:50051", "mci-price gRPC host:port")
		clientID  = flag.String("client-id", "algo-tester", "clientID (다중 인스턴스 구분)")
		symbols   = flag.String("symbols", "USDKRW", "구독 심볼 CSV (빈값=모든 심볼)")
		fromSeq   = flag.Int64("from-seq", 0, "재접속 backfill (Phase B). Phase A 는 반드시 0")
		sources   = flag.String("sources", "", "원천 필터 CSV (예: SMB,KMB). 빈값=BEST 모드 (excode per-source)")
		tenors    = flag.String("tenors", "", "forward tenor 필터 CSV (예: M01,M03). 빈값=spot(SPT) (effective swap 적용)")
		duration  = flag.Duration("duration", 30*time.Second, "테스트 실행 시간. 0=Ctrl-C 까지")
		slowSleep = flag.Duration("slow-sleep", 0, "매 recv 후 sleep — slow client 시뮬. 예: 100ms")
		jsonOut   = flag.Bool("json", false, "각 AlgoQuote 를 JSON 라인으로 출력 (스크립트 파싱용)")
	)
	flag.Parse()

	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	cli := wtgpb.NewPriceServiceClient(conn)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var d context.CancelFunc
		ctx, d = context.WithTimeout(ctx, *duration)
		defer d()
	}

	req := &wtgpb.AlgoSubscribeRequest{
		ClientId: *clientID,
		Symbols:  splitCSV(*symbols),
		Sources:  splitCSV(*sources),
		Tenors:   splitCSV(*tenors),
		FromSeq:  *fromSeq,
	}
	stream, err := cli.SubscribeAlgo(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "subscribe:", err)
		os.Exit(1)
	}
	fmt.Printf("[algo-tester] subscribe 시작 client_id=%s symbols=%v from_seq=%d\n",
		req.GetClientId(), req.GetSymbols(), req.GetFromSeq())

	// per-symbol seq gap 감지 — Phase A 는 gap=0 이어야 정상.
	lastSeq := map[string]int64{}
	var recv int64
	start := time.Now()

	for {
		q, err := stream.Recv()
		if err == io.EOF {
			fmt.Println("[algo-tester] stream EOF")
			break
		}
		if err != nil {
			fmt.Println("[algo-tester] recv err:", err)
			break
		}
		recv++
		nowNs := time.Now().UnixNano()
		latencyMs := float64(nowNs-q.GetTsWtgUnixNs()) / 1e6
		gap := ""
		if prev, ok := lastSeq[q.GetSym()]; ok {
			if q.GetSeq() != prev+1 {
				gap = fmt.Sprintf(" GAP(prev=%d,got=%d)", prev, q.GetSeq())
			}
		}
		lastSeq[q.GetSym()] = q.GetSeq()

		if *jsonOut {
			fmt.Printf(`{"sym":%q,"source":%q,"tenor":%q,"seq":%d,"bid":%.5f,"ask":%.5f,"mid":%.5f,"last":%.5f,"last_qty":%.0f,"backfill":%v}`+"\n",
				q.GetSym(), q.GetSource(), q.GetTenor(), q.GetSeq(), q.GetBid(), q.GetAsk(),
				q.GetMid(), q.GetLast(), q.GetLastQty(), q.GetIsBackfill())
		} else if recv <= 20 || recv%50 == 0 {
			fmt.Printf("[#%06d] %s src=%s tenor=%s seq=%d bid=%.5f ask=%.5f mid=%.5f last=%.5f lat=%.2fms bf=%v%s\n",
				recv, q.GetSym(), q.GetSource(), q.GetTenor(), q.GetSeq(), q.GetBid(), q.GetAsk(),
				q.GetMid(), q.GetLast(), latencyMs, q.GetIsBackfill(), gap)
		}
		if *slowSleep > 0 {
			time.Sleep(*slowSleep)
		}
	}

	elapsed := time.Since(start).Seconds()
	rate := float64(recv) / elapsed
	fmt.Printf("[algo-tester] 종료 — recv=%d elapsed=%.1fs rate=%.1f tick/s\n",
		recv, elapsed, rate)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
