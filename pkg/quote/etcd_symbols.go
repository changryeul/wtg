package quote

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

// EtcdSymbolWatcher 는 etcd prefix 아래의 SymbolEntry JSON 들을 모아 SymbolMap
// 을 갱신하는 watcher.
//
// 패턴:
//
//   - prefix (예: "wtg/quote/symbols/") 아래 각 key 는 외부 심볼 1건.
//     value 는 SymbolEntry JSON.
//   - 시작 시 prefix Get → 모든 entry 로 snapshot 빌드 → SymbolMap.Replace
//   - 백그라운드 watch goroutine 이 PUT/DELETE 이벤트 감지 → 누적 후 Replace
//
// 운영 흐름: mci-admin 이 USDKRW 비활성화 PUT → 모든 mci-price 가 즉시 해당
// 심볼 tick drop. 새 통화쌍 추가 → 즉시 처리 시작.
type EtcdSymbolWatcher struct {
	cli    *clientv3.Client
	prefix string
	m      *SymbolMap
	logger *slog.Logger

	// entries 는 raw etcd 상태의 local cache (key=etcd key → entry).
	// watch 이벤트 시 누적 + 전체 snapshot 재빌드.
	mu      sync.Mutex
	entries map[string]SymbolEntry

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdSymbolWatcherOptions 는 EtcdSymbolWatcher 생성 옵션.
type EtcdSymbolWatcherOptions struct {
	// Client 는 호출자가 dial 한 etcd v3 클라이언트. 필수.
	Client *clientv3.Client

	// Prefix (default "wtg/quote/symbols/"). 끝에 "/" 자동 보정.
	Prefix string

	// SymbolMap 갱신 대상. 필수.
	M *SymbolMap

	// Logger — 옵셔널.
	Logger *slog.Logger
}

// NewEtcdSymbolWatcher 는 etcd 에서 심볼 카탈로그를 1회 로드하고 watch 시작.
func NewEtcdSymbolWatcher(ctx context.Context, opt EtcdSymbolWatcherOptions) (*EtcdSymbolWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("quote: etcd Client 필수")
	}
	if opt.M == nil {
		return nil, errors.New("quote: SymbolMap 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/quote/symbols/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	w := &EtcdSymbolWatcher{
		cli:     opt.Client,
		prefix:  prefix,
		m:       opt.M,
		logger:  logger,
		entries: make(map[string]SymbolEntry),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

// initialLoad 는 prefix Get 으로 전체 entry 를 받아 SymbolMap 을 빌드.
func (w *EtcdSymbolWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("quote: etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		var e SymbolEntry
		if err := json.Unmarshal(kv.Value, &e); err != nil {
			w.logger.Warn("SymbolEntry JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)),
				slog.Any("error", err),
			)
			continue
		}
		w.entries[string(kv.Key)] = e
	}
	w.mu.Unlock()
	w.rebuildSnapshot()
	w.logger.Info("SymbolMap etcd 초기 로드",
		slog.String("prefix", w.prefix),
		slog.Int("count", w.m.Size()),
	)
	return nil
}

// watchLoop 은 prefix 변경을 받아 entries 를 갱신 + snapshot 재빌드.
func (w *EtcdSymbolWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("SymbolMap watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("SymbolMap watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdSymbolWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		key := string(ev.Kv.Key)
		switch ev.Type {
		case clientv3.EventTypePut:
			var e SymbolEntry
			if err := json.Unmarshal(ev.Kv.Value, &e); err != nil {
				w.logger.Warn("SymbolEntry PUT 파싱 실패", slog.String("key", key), slog.Any("error", err))
				continue
			}
			w.entries[key] = e
		case clientv3.EventTypeDelete:
			delete(w.entries, key)
		}
	}
	w.mu.Unlock()
	w.rebuildSnapshot()
}

// rebuildSnapshot 은 현재 entries 로 SymbolMap 전체 snapshot 을 atomic 교체.
func (w *EtcdSymbolWatcher) rebuildSnapshot() {
	w.mu.Lock()
	list := make([]SymbolEntry, 0, len(w.entries))
	for _, e := range w.entries {
		list = append(list, e)
	}
	w.mu.Unlock()
	w.m.Replace(list)
}

// Close 는 watch goroutine 종료 (idempotent). etcd client 는 호출자 관리.
func (w *EtcdSymbolWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
