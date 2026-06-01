package pricing

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

// EtcdCurrencyWatcher — wtg/currency/{code} prefix 를 watch 해 CurrencyMaster
// 를 실시간 갱신. fx-sync 가 PUT 한 변경이 즉시 메모리에 반영.
//
// EtcdSymbolWatcher 와 동일 패턴 — 초기 Get 으로 cache 빌드 + Watch 로 누적.
type EtcdCurrencyWatcher struct {
	cli    *clientv3.Client
	prefix string
	m      *CurrencyMaster
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]Currency // etcd key → Currency

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdCurrencyWatcherOptions — 생성 옵션.
type EtcdCurrencyWatcherOptions struct {
	Client *clientv3.Client
	Prefix string // default "wtg/currency/"
	M      *CurrencyMaster
	Logger *slog.Logger
}

// NewEtcdCurrencyWatcher — 1회 로드 + watch 시작.
func NewEtcdCurrencyWatcher(ctx context.Context, opt EtcdCurrencyWatcherOptions) (*EtcdCurrencyWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("pricing: etcd Client 필수")
	}
	if opt.M == nil {
		return nil, errors.New("pricing: CurrencyMaster 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/currency/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdCurrencyWatcher{
		cli:     opt.Client,
		prefix:  prefix,
		m:       opt.M,
		logger:  logger,
		entries: make(map[string]Currency),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

func (w *EtcdCurrencyWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("pricing: currency etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		var c Currency
		if err := json.Unmarshal(kv.Value, &c); err != nil {
			w.logger.Warn("Currency JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)),
				slog.Any("error", err))
			continue
		}
		w.entries[string(kv.Key)] = c
	}
	w.mu.Unlock()
	w.rebuild()
	w.logger.Info("CurrencyMaster etcd 초기 로드",
		slog.String("prefix", w.prefix),
		slog.Int("count", w.m.Size()))
	return nil
}

func (w *EtcdCurrencyWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("CurrencyMaster watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("CurrencyMaster watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdCurrencyWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var c Currency
			if err := json.Unmarshal(ev.Kv.Value, &c); err != nil {
				w.logger.Warn("Currency PUT 파싱 실패",
					slog.String("key", key), slog.Any("error", err))
				continue
			}
			w.entries[key] = c
		case clientv3.EventTypeDelete:
			delete(w.entries, key)
		}
	}
	w.mu.Unlock()
	w.rebuild()
	w.logger.Debug("CurrencyMaster 갱신", slog.Int("size", w.m.Size()))
}

func (w *EtcdCurrencyWatcher) rebuild() {
	w.mu.Lock()
	list := make([]Currency, 0, len(w.entries))
	for _, c := range w.entries {
		list = append(list, c)
	}
	w.mu.Unlock()
	w.m.Replace(list)
}

// Close — watch goroutine 종료 (idempotent).
func (w *EtcdCurrencyWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
