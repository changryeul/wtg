// mci-edge-fix — FIX 4.4 DMZ gateway. 외부 카운터파티의 NewOrderSingle 을
// WTG 의 /v1/tx alias 로 forward.
//
// Phase A (PoC):
//   - Logon + NewOrderSingle 단방향
//   - 정적 counterparty seed (--seed-cp 반복)
//   - envelope log mode (default) 또는 --tx-forward http://mci-api:8080 으로 forward
//
// 자세히는 docs/fix-gateway-design.md / docs/quickfix-go-spike.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/winwaysystems/wtg/pkg/logging"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/winwaysystems/wtg/internal/edge/fix"
)

func main() {
	var (
		listenPort   = flag.Int("port", 5001, "FIX session listen 포트")
		senderCompID = flag.String("sender", "WTG", "WTG self SenderCompID")
		heartBtInt   = flag.Int("heart", 30, "Heartbeat 주기 (초)")
		txForward    = flag.String("tx-forward", "", "/v1/tx backend base URL (예: http://mci-api:8080). 빈값=envelope log 만")
		logLevel     = flag.String("log-level", "info", "log level: debug/info/warn/error")
		statsAddr    = flag.String("stats", "", "HTTP /stats listen 주소 (예: 127.0.0.1:5002). 빈값=비활성")
		etcdEps      = flag.String("etcd", "", "etcd endpoints (콤마 구분). Phase B 동적 counterparty 등록. 빈값=정적 seed 만")
		etcdPrefix   = flag.String("etcd-counterparties-prefix", "wtg/fix/counterparties/", "etcd counterparty prefix")
		pushSecret   = flag.String("push-secret", "", "POST /v1/internal/exec-report 의 X-Push-Secret 검증 값. 빈값=인증 skip (dev only)")
		storeDir     = flag.String("store-dir", "", "FIX session message store dir. 빈값=memory store (재시작 시 seq 1 부터, dev only). 운영 권장 영속 dir 필수")
	)
	var seedCPs multiCP
	flag.Var(&seedCPs, "seed-cp", "정적 counterparty seed. 형식: 'ID=PASSWORD,SITE,TIER,USID' (반복 가능)")
	flag.Parse()

	logger := logging.Init("mci-edge-fix-ord", logging.Options{Level: *logLevel})

	cfg := fix.DefaultConfig()
	cfg.ListenPort = *listenPort
	cfg.SenderCompID = *senderCompID
	cfg.HeartBtInt = *heartBtInt
	cfg.TxForwardURL = *txForward
	cfg.Counterparties = seedCPs.parsed
	cfg.EtcdEndpoints = *etcdEps
	cfg.EtcdCounterpartiesPrefix = *etcdPrefix
	cfg.StoreDir = *storeDir

	srv, err := fix.NewServer(cfg, logger)
	if err != nil {
		logger.Error("FIX server 초기화 실패", slog.Any("err", err))
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// SIGHUP — Phase C. Reload (acceptor 재시작 + 새 counterparty 등록).
	// 시장 마감 시간 등 안전한 시점에 적용 권장 — 기존 active session 끊김 발생.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			logger.Info("SIGHUP — 새 counterparty 등록 reload")
			if err := srv.Reload(); err != nil {
				logger.Error("reload 실패", slog.Any("err", err))
			}
		}
	}()

	if *statsAddr != "" {
		go startStatsServer(*statsAddr, srv, *pushSecret, logger)
	}

	logger.Info("mci-edge-fix 부팅",
		slog.Int("port", *listenPort),
		slog.String("sender", *senderCompID),
		slog.Int("counterparties", len(cfg.Counterparties)),
		slog.String("tx_forward", *txForward),
		slog.String("stats", *statsAddr))

	if err := srv.Start(ctx); err != nil {
		logger.Error("서버 종료 — 오류", slog.Any("err", err))
		os.Exit(1)
	}
}

// multiCP — `--seed-cp` 의 반복 flag. 'ID=PW,SITE,TIER,USID' 형식.
type multiCP struct {
	parsed map[string]fix.Counterparty
}

func (m *multiCP) String() string {
	if m.parsed == nil {
		return ""
	}
	keys := make([]string, 0, len(m.parsed))
	for k := range m.parsed {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

func (m *multiCP) Set(v string) error {
	if m.parsed == nil {
		m.parsed = make(map[string]fix.Counterparty)
	}
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("--seed-cp 형식: 'ID=PASSWORD,SITE,TIER,USID' (got %q)", v)
	}
	cid := strings.TrimSpace(parts[0])
	fields := strings.Split(parts[1], ",")
	cp := fix.Counterparty{Channel: "FIX"}
	for i, f := range fields {
		f = strings.TrimSpace(f)
		switch i {
		case 0:
			cp.Password = f
		case 1:
			cp.Site = f
		case 2:
			cp.Tier = f
		case 3:
			cp.Usid = f
		}
	}
	m.parsed[cid] = cp
	return nil
}
