package lpcatalog

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

// EtcdWatcher — etcd prefix 아래 LP JSON 들을 모아 Catalog 를 갱신. pkg/instrument·
// pkg/quote 의 EtcdSymbolWatcher 와 동일 패턴(초기 Get → watch PUT/DELETE → Replace).
// mci-admin 이 wtg/catalog/lp/<code> PUT → 모든 mci-price 즉시 반영(재배포 X).
type EtcdWatcher struct {
	cli    *clientv3.Client
	prefix string
	c      *Catalog
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]LP // etcd key → LP

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdWatcherOptions — 생성 옵션.
type EtcdWatcherOptions struct {
	Client *clientv3.Client // 필수
	Prefix string           // default "wtg/catalog/lp/"
	C      *Catalog         // 갱신 대상, 필수
	Logger *slog.Logger
}

// NewEtcdWatcher — 초기 로드 후 watch 시작.
func NewEtcdWatcher(ctx context.Context, opt EtcdWatcherOptions) (*EtcdWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("lpcatalog: etcd Client 필수")
	}
	if opt.C == nil {
		return nil, errors.New("lpcatalog: Catalog 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/catalog/lp/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdWatcher{
		cli: opt.Client, prefix: prefix, c: opt.C, logger: logger,
		entries: make(map[string]LP),
		stopC:   make(chan struct{}), doneC: make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

func (w *EtcdWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("lpcatalog: etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		var lp LP
		if err := json.Unmarshal(kv.Value, &lp); err != nil {
			w.logger.Warn("LP JSON 파싱 실패 (skip)", slog.String("key", string(kv.Key)), slog.Any("error", err))
			continue
		}
		w.entries[string(kv.Key)] = lp
	}
	w.mu.Unlock()
	w.rebuild()
	w.logger.Info("LP 카탈로그 etcd 초기 로드", slog.String("prefix", w.prefix), slog.Int("count", w.c.Size()))
	return nil
}

func (w *EtcdWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("LP 카탈로그 watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("LP 카탈로그 watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var lp LP
			if err := json.Unmarshal(ev.Kv.Value, &lp); err != nil {
				w.logger.Warn("LP PUT 파싱 실패", slog.String("key", key), slog.Any("error", err))
				continue
			}
			w.entries[key] = lp
		case clientv3.EventTypeDelete:
			delete(w.entries, key)
		}
	}
	w.mu.Unlock()
	w.rebuild()
}

func (w *EtcdWatcher) rebuild() {
	w.mu.Lock()
	list := make([]LP, 0, len(w.entries))
	for _, lp := range w.entries {
		list = append(list, lp)
	}
	w.mu.Unlock()
	w.c.Replace(list)
}

// Close — watch goroutine 종료 (idempotent). etcd client 는 호출자 관리.
func (w *EtcdWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
