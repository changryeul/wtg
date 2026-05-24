package quoteid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdAllowlistWatcher 는 engine_id allowlist 를 etcd prefix 에서 watch.
//
// 키 모델:
//
//	<prefix>quoteid/engines/<engine_id>    — value 는 무시 (presence = allow)
//
// 흐름:
//
//	시작 시 prefix Get → 모든 engine_id 추출 → callback 호출
//	백그라운드 watch goroutine 이 PUT/DELETE 이벤트 감지 → set 갱신 → callback
//
// callback 은 새 snapshot 을 받아 atomic 갱신 — 일반적으로
// QuoteValidationServer.SetEngineAllowlistMap.
type EtcdAllowlistWatcher struct {
	cli     *clientv3.Client
	prefix  string
	logger  *slog.Logger
	onApply func(map[string]struct{})

	mu      sync.Mutex
	engines map[string]struct{} // <engine_id>

	stopC chan struct{}
	doneC chan struct{}
}

// EtcdAllowlistWatcherOptions 는 EtcdAllowlistWatcher 생성 옵션.
type EtcdAllowlistWatcherOptions struct {
	Client *clientv3.Client
	// Prefix — 예: "wtg/quoteid/engines/". 끝에 "/" 자동 보정.
	Prefix string
	// OnApply — snapshot 갱신 콜백. nil 이면 사용 불가 (panic).
	OnApply func(map[string]struct{})
	Logger  *slog.Logger
}

// NewEtcdAllowlistWatcher — etcd 에서 allowlist 를 1회 로드하고 watch 시작.
func NewEtcdAllowlistWatcher(ctx context.Context, opt EtcdAllowlistWatcherOptions) (*EtcdAllowlistWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("quoteid: etcd Client 필수")
	}
	if opt.OnApply == nil {
		return nil, errors.New("quoteid: OnApply callback 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/quoteid/engines/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdAllowlistWatcher{
		cli:     opt.Client,
		prefix:  prefix,
		logger:  logger,
		onApply: opt.OnApply,
		engines: make(map[string]struct{}),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

// initialLoad — prefix Get 으로 현재 등록된 engine_id 전체 수집 → onApply.
func (w *EtcdAllowlistWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("quoteid: etcd 초기 Get: %w", err)
	}
	w.mu.Lock()
	for _, kv := range resp.Kvs {
		eng := w.engineFromKey(string(kv.Key))
		if eng != "" {
			w.engines[eng] = struct{}{}
		}
	}
	snap := w.snapshotLocked()
	w.mu.Unlock()
	w.onApply(snap)
	w.logger.Info("EngineAllowlist etcd 초기 로드",
		slog.String("prefix", w.prefix),
		slog.Int("count", len(snap)))
	return nil
}

// watchLoop — prefix 변경 → engines 갱신 → callback.
func (w *EtcdAllowlistWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("EngineAllowlist watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("EngineAllowlist watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdAllowlistWatcher) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	w.mu.Lock()
	for _, ev := range events {
		eng := w.engineFromKey(string(ev.Kv.Key))
		if eng == "" {
			continue
		}
		switch ev.Type {
		case clientv3.EventTypePut:
			w.engines[eng] = struct{}{}
		case clientv3.EventTypeDelete:
			delete(w.engines, eng)
		}
	}
	snap := w.snapshotLocked()
	w.mu.Unlock()
	w.onApply(snap)
	w.logger.Info("EngineAllowlist 갱신",
		slog.Int("active", len(snap)),
		slog.Int("events", len(events)))
}

// engineFromKey — "<prefix><engine_id>" → "<engine_id>".
func (w *EtcdAllowlistWatcher) engineFromKey(key string) string {
	if !strings.HasPrefix(key, w.prefix) {
		return ""
	}
	return strings.TrimPrefix(key, w.prefix)
}

// snapshotLocked — engines map 의 copy.
func (w *EtcdAllowlistWatcher) snapshotLocked() map[string]struct{} {
	out := make(map[string]struct{}, len(w.engines))
	for k := range w.engines {
		out[k] = struct{}{}
	}
	return out
}

// Close — watch goroutine 종료.
func (w *EtcdAllowlistWatcher) Close() error {
	select {
	case <-w.stopC:
	default:
		close(w.stopC)
	}
	<-w.doneC
	return nil
}
