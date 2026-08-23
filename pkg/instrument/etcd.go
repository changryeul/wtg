package instrument

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdCatalogWatcher 는 etcd prefix 아래 Instrument JSON 들을 모아 Catalog 를
// 갱신하는 watcher. pkg/quote.EtcdSymbolWatcher 와 동일 패턴(초기 Get →
// watch PUT/DELETE 누적 → 전체 snapshot Replace).
//
// 운영 흐름: mci-admin 이 etcd 에 Instrument PUT → 모든 통합 엣지가 즉시 반영
// (신규 심볼 라우팅 가능, active=false 로 즉시 차단).
type EtcdCatalogWatcher struct {
	cli    *clientv3.Client
	prefix string
	c      *Catalog
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]Instrument // etcd key → Instrument

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdCatalogWatcherOptions — 생성 옵션.
type EtcdCatalogWatcherOptions struct {
	Client *clientv3.Client // 필수 (호출자 dial)
	Prefix string           // default "wtg/catalog/instruments/"
	C      *Catalog         // 갱신 대상, 필수
	Logger *slog.Logger     // 옵셔널
}

// NewEtcdCatalogWatcher — 초기 로드 후 watch 시작.
func NewEtcdCatalogWatcher(ctx context.Context, opt EtcdCatalogWatcherOptions) (*EtcdCatalogWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("instrument: etcd Client 필수")
	}
	if opt.C == nil {
		return nil, errors.New("instrument: Catalog 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/catalog/instruments/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdCatalogWatcher{
		cli:     opt.Client,
		prefix:  prefix,
		c:       opt.C,
		logger:  logger,
		entries: make(map[string]Instrument),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

func (w *EtcdCatalogWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("instrument: etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		var it Instrument
		if err := json.Unmarshal(kv.Value, &it); err != nil {
			w.logger.Warn("Instrument JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)), slog.Any("error", err))
			continue
		}
		w.entries[string(kv.Key)] = it
	}
	w.mu.Unlock()
	w.rebuildSnapshot()
	w.logger.Info("Instrument 카탈로그 etcd 초기 로드",
		slog.String("prefix", w.prefix), slog.Int("count", w.c.Size()))
	return nil
}

func (w *EtcdCatalogWatcher) watchLoop(ctx context.Context) {
	defer close(w.doneC)
	wch := w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
	for {
		select {
		case <-w.stopC:
			return
		case <-ctx.Done():
			return
		case wresp, ok := <-wch:
			if !ok {
				w.logger.Warn("Instrument watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("Instrument watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdCatalogWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var it Instrument
			if err := json.Unmarshal(ev.Kv.Value, &it); err != nil {
				w.logger.Warn("Instrument PUT 파싱 실패", slog.String("key", key), slog.Any("error", err))
				continue
			}
			w.entries[key] = it
		case clientv3.EventTypeDelete:
			delete(w.entries, key)
		}
	}
	w.mu.Unlock()
	w.rebuildSnapshot()
}

func (w *EtcdCatalogWatcher) rebuildSnapshot() {
	w.mu.Lock()
	list := make([]Instrument, 0, len(w.entries))
	for _, it := range w.entries {
		list = append(list, it)
	}
	w.mu.Unlock()
	w.c.Replace(list)
}

// Close — watch goroutine 종료 (idempotent). etcd client 는 호출자 관리.
func (w *EtcdCatalogWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
