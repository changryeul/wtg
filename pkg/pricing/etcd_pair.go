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

// EtcdPairWatcher — wtg/pair/{id} 를 watch 해 PairMaster 갱신.
// EtcdCurrencyWatcher 와 동일 패턴.
//
// 변경 발생 시 OnChange 콜백 호출 (옵션) — CrossRateConsumer 의
// ReplaceFormulas 같은 후속 동작 wire-up 용.
type EtcdPairWatcher struct {
	cli      *clientv3.Client
	prefix   string
	m        *PairMaster
	onChange func(*PairMaster)
	logger   *slog.Logger

	mu      sync.Mutex
	entries map[string]Pair

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

type EtcdPairWatcherOptions struct {
	Client *clientv3.Client
	Prefix string // default "wtg/pair/"
	M      *PairMaster
	Logger *slog.Logger

	// OnChange — Master 갱신 직후 호출. nil 가능. 호출은 watch goroutine 에서
	// 동기적으로 — 빠르게 끝나거나 자체 goroutine 분리해야 watch 지연 X.
	OnChange func(*PairMaster)
}

// NewEtcdPairWatcher — 1회 로드 + watch 시작.
func NewEtcdPairWatcher(ctx context.Context, opt EtcdPairWatcherOptions) (*EtcdPairWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("pricing: etcd Client 필수")
	}
	if opt.M == nil {
		return nil, errors.New("pricing: PairMaster 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/pair/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdPairWatcher{
		cli:      opt.Client,
		prefix:   prefix,
		m:        opt.M,
		onChange: opt.OnChange,
		logger:   logger,
		entries:  make(map[string]Pair),
		stopC:    make(chan struct{}),
		doneC:    make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

func (w *EtcdPairWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("pricing: pair etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		var p Pair
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			w.logger.Warn("Pair JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)), slog.Any("error", err))
			continue
		}
		w.entries[string(kv.Key)] = p
	}
	w.mu.Unlock()
	w.rebuild()
	w.logger.Info("PairMaster etcd 초기 로드",
		slog.String("prefix", w.prefix), slog.Int("count", w.m.Size()))
	return nil
}

func (w *EtcdPairWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("PairMaster watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("PairMaster watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdPairWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var p Pair
			if err := json.Unmarshal(ev.Kv.Value, &p); err != nil {
				w.logger.Warn("Pair PUT 파싱 실패",
					slog.String("key", key), slog.Any("error", err))
				continue
			}
			w.entries[key] = p
		case clientv3.EventTypeDelete:
			delete(w.entries, key)
		}
	}
	w.mu.Unlock()
	w.rebuild()
	w.logger.Debug("PairMaster 갱신", slog.Int("size", w.m.Size()))
}

func (w *EtcdPairWatcher) rebuild() {
	w.mu.Lock()
	list := make([]Pair, 0, len(w.entries))
	for _, p := range w.entries {
		list = append(list, p)
	}
	w.mu.Unlock()
	w.m.Replace(list)
	if w.onChange != nil {
		w.onChange(w.m)
	}
}

// Close — watch goroutine 종료 (idempotent).
func (w *EtcdPairWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
