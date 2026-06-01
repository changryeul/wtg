// fx-sync — 외환 운영 DB 의 마스터 데이터를 WTG etcd 로 미러링하는 CLI.
//
// Step 1 (현재): Currency (TB_FXB_CMG005M) + FileBackend (JSON mock) 만.
// 향후 단계: Pair / Cross 산식 / Swap / Margin / Holiday 등 추가.
//
// 사용:
//
//	# file mock 으로 dev 시드.
//	fx-sync --source=file --source-dir=./etc/db-mirror \
//	        --etcd=127.0.0.1:2379 --etcd-prefix=wtg/ \
//	        --table=currency
//
//	# (향후) Oracle 직접:
//	fx-sync --source=oracle --dsn=oracle://... --table=all
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/internal/fxsync"
)

func main() {
	var (
		source      = flag.String("source", "file", "데이터 source — 'file' (mock JSON) 또는 'oracle' (향후)")
		sourceDir   = flag.String("source-dir", "./etc/db-mirror", "file source 의 JSON 디렉토리")
		etcdAddrs   = flag.String("etcd", "127.0.0.1:2379", "etcd endpoints (콤마 구분)")
		etcdPrefix  = flag.String("etcd-prefix", "wtg/", "etcd key prefix")
		etcdUser    = flag.String("etcd-user", "", "etcd auth username")
		etcdPass    = flag.String("etcd-pass", "", "etcd auth password")
		dialTimeout = flag.Duration("dial-timeout", 5*time.Second, "etcd dial timeout")
		table       = flag.String("table", "currency", "sync 대상 테이블 — 'currency' | 'all'")
		deleteStale = flag.Bool("delete-stale", true, "DB 에 없는 etcd 키 정리")
		logLevel    = flag.String("log-level", "info", "debug | info | warn | error")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	// Backend 구성.
	var backend fxsync.Backend
	switch *source {
	case "file":
		backend = fxsync.NewFileBackend(*sourceDir)
		logger.Info("FileBackend 활성", slog.String("dir", *sourceDir))
	case "oracle":
		logger.Error("oracle backend 는 후속 단계에서 구현 — 현재 미지원")
		os.Exit(2)
	default:
		logger.Error("unknown source", slog.String("source", *source))
		os.Exit(2)
	}

	// etcd 연결.
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdAddrs, ","),
		DialTimeout: *dialTimeout,
		Username:    *etcdUser,
		Password:    *etcdPass,
	})
	if err != nil {
		logger.Error("etcd dial 실패", slog.Any("error", err))
		os.Exit(1)
	}
	defer cli.Close()
	logger.Info("etcd 연결", slog.String("addrs", *etcdAddrs), slog.String("prefix", *etcdPrefix))

	syncer := fxsync.NewSyncer(cli, logger)
	syncer.Prefix = *etcdPrefix
	syncer.DeleteStale = *deleteStale

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 테이블 별 처리.
	tables := strings.Split(*table, ",")
	if *table == "all" {
		tables = []string{"currency", "pair", "swap", "hq_margin", "site_margin"}
	}
	exitCode := 0
	for _, t := range tables {
		switch strings.TrimSpace(t) {
		case "currency":
			if err := syncCurrency(ctx, backend, syncer); err != nil {
				logger.Error("currency sync 실패", slog.Any("error", err))
				exitCode = 1
			}
		case "pair":
			if err := syncPair(ctx, backend, syncer); err != nil {
				logger.Error("pair sync 실패", slog.Any("error", err))
				exitCode = 1
			}
		case "swap":
			if err := syncSwap(ctx, backend, syncer); err != nil {
				logger.Error("swap sync 실패", slog.Any("error", err))
				exitCode = 1
			}
		case "hq_margin":
			if err := syncHQMargin(ctx, backend, syncer); err != nil {
				logger.Error("hq_margin sync 실패", slog.Any("error", err))
				exitCode = 1
			}
		case "site_margin":
			if err := syncSiteMargin(ctx, backend, syncer); err != nil {
				logger.Error("site_margin sync 실패", slog.Any("error", err))
				exitCode = 1
			}
		default:
			logger.Warn("unknown table — skip", slog.String("table", t))
		}
	}
	os.Exit(exitCode)
}

func syncCurrency(ctx context.Context, backend fxsync.Backend, syncer *fxsync.Syncer) error {
	cs, err := backend.LoadCurrencies(ctx)
	if err != nil {
		return fmt.Errorf("LoadCurrencies: %w", err)
	}
	_, err = syncer.SyncCurrencies(ctx, cs)
	return err
}

func syncPair(ctx context.Context, backend fxsync.Backend, syncer *fxsync.Syncer) error {
	ps, err := backend.LoadPairs(ctx)
	if err != nil {
		return fmt.Errorf("LoadPairs: %w", err)
	}
	_, err = syncer.SyncPairs(ctx, ps)
	return err
}

func syncSwap(ctx context.Context, backend fxsync.Backend, syncer *fxsync.Syncer) error {
	sps, err := backend.LoadSwapPoints(ctx)
	if err != nil {
		return fmt.Errorf("LoadSwapPoints: %w", err)
	}
	_, err = syncer.SyncSwapPoints(ctx, sps)
	return err
}

func syncHQMargin(ctx context.Context, backend fxsync.Backend, syncer *fxsync.Syncer) error {
	ms, err := backend.LoadHQMargins(ctx)
	if err != nil {
		return fmt.Errorf("LoadHQMargins: %w", err)
	}
	_, err = syncer.SyncHQMargins(ctx, ms)
	return err
}

func syncSiteMargin(ctx context.Context, backend fxsync.Backend, syncer *fxsync.Syncer) error {
	ms, err := backend.LoadSiteMargins(ctx)
	if err != nil {
		return fmt.Errorf("LoadSiteMargins: %w", err)
	}
	_, err = syncer.SyncSiteMargins(ctx, ms)
	return err
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
