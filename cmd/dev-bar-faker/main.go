// dev-bar-faker — mci-chart 의 --upstream 가 호출할 가짜 PriceService gRPC server.
//
// mci-price 를 띄우지 않고 mci-chart 의 라이브 봉 stream / WS fan-out 을 검증할
// 수 있도록, random walk 로 봉을 주기적으로 emit 한다.
//
//	# 1) faker 실행
//	./build/bin/dev-bar-faker --listen :50051
//
//	# 2) mci-chart 를 faker 에 연결
//	./build/bin/mci-chart --listen :8086 \
//	  --dsn 'postgres://wtg:secret@localhost:5432/wtg?sslmode=disable' \
//	  --upstream localhost:50051
//
// 봉 cadence: --interval (기본 2s). 매 tick 마다 모든 pair × ("1m","5m") 에 대해
// 현재 wall-clock bucket 의 봉을 OHLC 누적 + emit. 분 경계가 넘어가면 새 봉이
// 자동 생성된다.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

type pairCfg struct {
	mid    float64
	spread float64
}

var defaultPairs = map[string]*pairCfg{
	"USD/KRW": {mid: 1400.0, spread: 0.05},
	"EUR/KRW": {mid: 1520.0, spread: 0.08},
	"JPY/KRW": {mid: 9.2, spread: 0.005},
}

var timeframes = []struct {
	name string
	dur  time.Duration
}{
	{"1m", time.Minute},
	{"5m", 5 * time.Minute},
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
}

type fakeServer struct {
	wtgpb.UnimplementedPriceServiceServer
	logger  *slog.Logger
	mu      sync.Mutex
	streams []chan *wtgpb.Bar
}

func (f *fakeServer) SubscribeBar(req *wtgpb.BarSubscribeRequest, stream wtgpb.PriceService_SubscribeBarServer) error {
	ch := make(chan *wtgpb.Bar, 256)
	f.mu.Lock()
	f.streams = append(f.streams, ch)
	count := len(f.streams)
	f.mu.Unlock()
	f.logger.Info("SubscribeBar 구독 등록",
		slog.String("subscriber_id", req.GetSubscriberId()),
		slog.Int("active", count))

	defer func() {
		f.mu.Lock()
		for i, c := range f.streams {
			if c == ch {
				f.streams = append(f.streams[:i], f.streams[i+1:]...)
				break
			}
		}
		left := len(f.streams)
		f.mu.Unlock()
		f.logger.Info("SubscribeBar 구독 해제",
			slog.String("subscriber_id", req.GetSubscriberId()),
			slog.Int("active", left))
	}()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case b := <-ch:
			if err := stream.Send(b); err != nil {
				return err
			}
		}
	}
}

func (f *fakeServer) broadcast(b *wtgpb.Bar) {
	f.mu.Lock()
	streams := append([]chan *wtgpb.Bar(nil), f.streams...)
	f.mu.Unlock()
	for _, c := range streams {
		select {
		case c <- b:
		default:
			// drop on backpressure — slow consumer 는 GracefulStop 이후 정리됨.
		}
	}
}

// bar 진행 상태 — (pair, tf) 단위로 현재 bucket 의 OHLC 누적.
type barState struct {
	openedNano int64
	openBid    float64
	openAsk    float64
	highBid    float64
	highAsk    float64
	lowBid     float64
	lowAsk     float64
	closeBid   float64
	closeAsk   float64
	tickCount  int32
}

func emitLoop(ctx context.Context, srv *fakeServer, interval time.Duration) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	state := map[string]*barState{} // key: pair|tf

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for sym, cfg := range defaultPairs {
				// random walk on mid.
				cfg.mid += rng.NormFloat64() * cfg.mid * 0.00012
				bid := cfg.mid - cfg.spread/2
				ask := cfg.mid + cfg.spread/2

				for _, tf := range timeframes {
					bucket := now.Truncate(tf.dur).UnixNano()
					key := sym + "|" + tf.name
					cur, ok := state[key]
					if !ok || cur.openedNano != bucket {
						cur = &barState{
							openedNano: bucket,
							openBid:    bid, openAsk: ask,
							highBid: bid, highAsk: ask,
							lowBid: bid, lowAsk: ask,
							closeBid: bid, closeAsk: ask,
						}
						state[key] = cur
					}
					if bid > cur.highBid {
						cur.highBid = bid
						cur.highAsk = ask
					}
					if bid < cur.lowBid {
						cur.lowBid = bid
						cur.lowAsk = ask
					}
					cur.closeBid = bid
					cur.closeAsk = ask
					cur.tickCount++

					srv.broadcast(&wtgpb.Bar{
						Pair:           sym,
						Tf:             tf.name,
						OpenedUnixNano: cur.openedNano,
						ClosedUnixNano: cur.openedNano + tf.dur.Nanoseconds(),
						OpenBid:        cur.openBid,
						OpenAsk:        cur.openAsk,
						HighBid:        cur.highBid,
						HighAsk:        cur.highAsk,
						LowBid:         cur.lowBid,
						LowAsk:         cur.lowAsk,
						CloseBid:       cur.closeBid,
						CloseAsk:       cur.closeAsk,
						TickCount:      cur.tickCount,
					})
				}
			}
		}
	}
}

func main() {
	addr := flag.String("listen", ":50051", "gRPC listen address")
	interval := flag.Duration("interval", 2*time.Second, "봉 emit 주기")
	flag.Parse()

	logger := logging.Init("dev-bar-faker", logging.Options{})

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *addr, err)
		os.Exit(1)
	}

	srv := &fakeServer{logger: logger}
	gs := grpc.NewServer()
	wtgpb.RegisterPriceServiceServer(gs, srv)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go emitLoop(ctx, srv, *interval)

	logger.Info("dev-bar-faker 시작",
		slog.String("listen", *addr),
		slog.Duration("interval", *interval))

	go func() {
		<-ctx.Done()
		logger.Info("graceful 종료")
		gs.GracefulStop()
	}()

	if err := gs.Serve(lis); err != nil {
		logger.Error("serve 실패", slog.Any("error", err))
		os.Exit(1)
	}
}
