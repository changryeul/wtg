// mci-mds-shim — mds query-server (W9500) 대체 wire 호환 AP.
//
// broker 에 mds 조회계의 큐 이름으로 등록해 W950x 고정폭 전문을 받아
// WTG 백엔드로 변환한다. 1차 수직 관통은 W9504A01 (수동 스왑포인트) 1건 —
// trn W2006A01 무수정 호출 → etcd pricing 반영 (mci-price 가 watch).
// 전환 계획: docs/mds-replacement-plan.md Stage 2.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/internal/mdsshim"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func main() {
	brokerHost := flag.String("broker-host", "127.0.0.1", "mymqd 호스트")
	brokerPort := flag.Int("broker-port", 11217, "mymqd 포트")
	appl := flag.String("appl", "mci-mds-shim", "ApplName (≤16B)")
	queue := flag.String("queue", "W9500", "큐 이름 — broker 라우팅 테이블이 W950x 를 보내는 mds 조회계 큐를 승계")
	xchg := flag.String("exchange", "dom", "함께 선언할 exchange (mds 조회계가 속한 exchange)")
	etcdAddrs := flag.String("etcd", "127.0.0.1:2379", "etcd endpoints (콤마 구분)")
	etcdUser := flag.String("etcd-user", "", "etcd 사용자 (옵션)")
	etcdPass := flag.String("etcd-pass", "", "etcd 비밀번호 (옵션)")
	pricingKey := flag.String("pricing-key", "wtg/pricing/table", "PricingTableDoc 이 저장된 etcd key")
	zdiv := flag.Int("zdiv", 0, "수치 스케일 (10^zdiv 로 나눔) — TODO: symbols 카탈로그 연동 전 고정값")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// etcd 연결.
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdAddrs, ","),
		DialTimeout: 5 * time.Second,
		Username:    *etcdUser,
		Password:    *etcdPass,
	})
	if err != nil {
		logger.Error("etcd dial 실패", slog.Any("error", err))
		os.Exit(1)
	}
	defer etcdCli.Close()

	// broker 접속 — mds 조회계 큐 승계 + 자동 재연결.
	cli, err := mymq.Open(ctx, *brokerHost, *brokerPort, mymq.Options{
		ApplName:  *appl,
		Channel:   mymq.ChannelAdmin,
		// QtPublic — 외부 발견 가능한 AP 큐 (broker 라우팅 테이블의 transaction
		// 목적지). mds 조회계 (W9500) 의 등록 형태를 승계한다.
		Queue: &mymq.QueueOptions{
			Name:         *queue,
			Attr:         mymq.QtPublic,
			ExchangeName: *xchg,
			ExchangeType: mymq.ExchangeDirect,
		},
		Reconnect: &mymq.ReconnectOptions{},
	})
	if err != nil {
		logger.Error("broker 접속 실패", slog.Any("error", err))
		os.Exit(1)
	}
	defer cli.Close()

	// rkey 바인딩 — broker 가 이 rkey 의 transaction 을 우리 큐로 라우팅.
	if err := cli.BindService(ctx, *xchg, mdsshim.RkeyW9504A01); err != nil {
		logger.Error("bind_service 실패", slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("mci-mds-shim 기동",
		slog.String("broker", *brokerHost), slog.String("queue", *queue),
		slog.String("pricing_key", *pricingKey))

	applier := etcdApplier(etcdCli, *pricingKey, logger)
	zdivFn := func(string) int { return *zdiv }

	for {
		select {
		case <-ctx.Done():
			logger.Info("종료")
			return
		case u, ok := <-cli.Subscribe():
			if !ok {
				logger.Error("subscribe 채널 종료")
				return
			}
			reply, err := mdsshim.HandleW9504A01(u, zdivFn, applier)
			if err != nil {
				logger.Error("요청 처리 실패", slog.Any("error", err))
			}
			if reply == nil {
				continue // 서비스 불일치 프레임 (heartbeat/기타)
			}
			if err := cli.Send(reply); err != nil {
				logger.Error("응답 송신 실패", slog.Any("error", err))
			} else {
				logger.Info("W9504A01 처리", slog.Uint64("ckey", uint64(reply.Ckey)),
					slog.Uint64("errn", uint64(reply.Errn)))
			}
		}
	}
}

// etcdApplier 는 pricing doc 을 get→변환→CAS put 하는 Applier 를 만든다.
// 동시 writer (mci-admin 등) 와의 경합은 ModRevision 비교로 3회 재시도.
func etcdApplier(cli *clientv3.Client, key string, logger *slog.Logger) mdsshim.Applier {
	return func(pair string, ups []mdsshim.SwapUpdate, clear bool) error {
		for attempt := 0; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			resp, err := cli.Get(ctx, key)
			cancel()
			if err != nil {
				return err
			}
			var cur []byte
			var rev int64
			if len(resp.Kvs) > 0 {
				cur = resp.Kvs[0].Value
				rev = resp.Kvs[0].ModRevision
			}

			next, err := mdsshim.ApplySwapToDoc(cur, pair, ups, clear)
			if err != nil {
				return err
			}

			ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			txn, err := cli.Txn(ctx).
				If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
				Then(clientv3.OpPut(key, string(next))).
				Commit()
			cancel()
			if err != nil {
				return err
			}
			if txn.Succeeded {
				logger.Info("pricing 반영", slog.String("pair", pair),
					slog.Int("updates", len(ups)), slog.Bool("clear", clear))
				return nil
			}
			if attempt >= 2 {
				return context.DeadlineExceeded
			}
			// 경합 — 최신본으로 재시도.
		}
	}
}
