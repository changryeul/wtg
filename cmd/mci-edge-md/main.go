// mci-edge-md — FIX 4.4 시세 DMZ gateway. 외부 카운터파티의 MarketDataRequest
// (35=V) 를 받아 MarketDataSnapshotFullRefresh (35=W) 로 응답.
//
// Phase A (skeleton):
//   - Logon + MDR 파싱 → 정적 하드코딩 quote 응답
//   - 정적 counterparty seed (--seed-cp 반복)
//   - 증분 (35=X) / MDR reject (35=Y) / gRPC upstream 은 Phase B~C 확장
//
// 자세히는 docs/fix-gateway-design.md (line 370 mci-edge-md).
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

	"github.com/winwaysystems/wtg/internal/edge/md"
)

func main() {
	var (
		listenPort   = flag.Int("port", 5011, "FIX MD session listen 포트 (edge-fix 5001 과 분리)")
		senderCompID = flag.String("sender", "WTG_MD", "WTG self SenderCompID")
		heartBtInt   = flag.Int("heart", 30, "Heartbeat 주기 (초)")
		logLevel     = flag.String("log-level", "info", "log level: debug/info/warn/error")
		statsAddr    = flag.String("stats", "", "HTTP /stats listen 주소 (예: 127.0.0.1:5012). 빈값=비활성")
		etcdEps      = flag.String("etcd", "", "etcd endpoints (콤마 구분). Phase B 동적 counterparty 등록. 빈값=정적 seed 만")
		etcdPrefix   = flag.String("etcd-counterparties-prefix", "wtg/fix/counterparties/", "etcd counterparty prefix (edge-fix 와 동일 store 재사용)")
	)
	var seedCPs multiCP
	flag.Var(&seedCPs, "seed-cp", "정적 counterparty seed. 형식: 'ID=PASSWORD,SITE,TIER,USID' (반복 가능)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))

	cfg := md.DefaultConfig()
	cfg.ListenPort = *listenPort
	cfg.SenderCompID = *senderCompID
	cfg.HeartBtInt = *heartBtInt
	cfg.Counterparties = seedCPs.parsed
	cfg.EtcdEndpoints = *etcdEps
	cfg.EtcdCounterpartiesPrefix = *etcdPrefix

	srv, err := md.NewServer(cfg, logger)
	if err != nil {
		logger.Error("MD server 초기화 실패", slog.Any("err", err))
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// SIGHUP — Phase C 대비 자리. Phase A 는 재로드해도 seed 그대로.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			logger.Info("SIGHUP — reload")
			if err := srv.Reload(); err != nil {
				logger.Error("reload 실패", slog.Any("err", err))
			}
		}
	}()

	if *statsAddr != "" {
		go startStatsServer(*statsAddr, srv, logger)
	}

	logger.Info("mci-edge-md 부팅",
		slog.Int("port", *listenPort),
		slog.String("sender", *senderCompID),
		slog.Int("counterparties", len(cfg.Counterparties)),
		slog.String("stats", *statsAddr))

	if err := srv.Start(ctx); err != nil {
		logger.Error("서버 종료 — 오류", slog.Any("err", err))
		os.Exit(1)
	}
}

// multiCP — `--seed-cp` 반복 flag. 'ID=PW,SITE,TIER,USID' 형식.
type multiCP struct {
	parsed map[string]md.Counterparty
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
		m.parsed = make(map[string]md.Counterparty)
	}
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("--seed-cp 형식: 'ID=PASSWORD,SITE,TIER,USID' (got %q)", v)
	}
	cid := strings.TrimSpace(parts[0])
	fields := strings.Split(parts[1], ",")
	cp := md.Counterparty{Channel: "FIX"}
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

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
