package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdTableWatcher 는 etcd 의 단일 key 에서 PricingTableDoc(JSON) 을 읽어
// pricing.Store 를 갱신하는 watcher.
//
// 패턴:
//
//   - 단일 key (예: "wtg/pricing/table")
//   - 시작 시 Get → ParsePricingTable → store.Replace
//   - 백그라운드 watch goroutine 이 PUT 이벤트 감지 → store.Replace
//   - DELETE 이벤트는 무시 (운영자가 의도적으로 비활성화하는 경우 별도 처리)
//
// 운영 흐름: mci-admin 이 단일 key 에 새 PricingTableDoc JSON 을 PUT 하면 모든
// mci-price 인스턴스가 즉시 새 마진 테이블로 전환된다 (atomic snapshot 교체).
type EtcdTableWatcher struct {
	cli    *clientv3.Client
	key    string
	store  *Store
	logger *slog.Logger

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdTableWatcherOptions 는 EtcdTableWatcher 생성 옵션.
type EtcdTableWatcherOptions struct {
	// Client 는 호출자가 dial 한 etcd v3 클라이언트. 필수. Close 도 호출자 관리.
	Client *clientv3.Client

	// Key 는 PricingTableDoc 이 저장된 단일 key (default "wtg/pricing/table").
	Key string

	// Store 는 갱신 대상. 필수.
	Store *Store

	// Logger — 옵셔널. nil 이면 slog.Default().
	Logger *slog.Logger
}

// NewEtcdTableWatcher 는 etcd 에서 PricingTable 을 1회 로드하고 watch 를 시작.
//
// 초기 로드 실패 시 에러 반환 — 호출자는 fallback 또는 실패 처리 결정.
// key 가 존재하지 않으면 (KV 없음) 빈 PricingTable 로 시작 (정상 동작 — 운영자가
// 아직 publish 안 한 상태).
func NewEtcdTableWatcher(ctx context.Context, opt EtcdTableWatcherOptions) (*EtcdTableWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("pricing: etcd Client 필수")
	}
	if opt.Store == nil {
		return nil, errors.New("pricing: Store 필수")
	}
	key := opt.Key
	if key == "" {
		key = "wtg/pricing/table"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	w := &EtcdTableWatcher{
		cli:    opt.Client,
		key:    key,
		store:  opt.Store,
		logger: logger,
		stopC:  make(chan struct{}),
		doneC:  make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

// initialLoad 는 단일 key 의 현재 값을 읽어 store 에 반영한다.
// key 부재 시 에러 아님 — 빈 PricingTable 유지 (운영 시작 전 상태).
func (w *EtcdTableWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.key)
	if err != nil {
		return fmt.Errorf("pricing: etcd 초기 Get: %w", err)
	}
	if len(resp.Kvs) == 0 {
		w.logger.Info("PricingTable etcd key 없음 — 빈 테이블로 시작",
			slog.String("key", w.key),
		)
		return nil
	}
	tbl, err := ParsePricingTable(resp.Kvs[0].Value)
	if err != nil {
		return fmt.Errorf("pricing: 초기 JSON 파싱: %w", err)
	}
	w.store.Replace(tbl)
	w.logger.Info("PricingTable etcd 초기 로드",
		slog.String("key", w.key),
		slog.Int64("version", tbl.Version),
	)
	return nil
}

// watchLoop 은 key 변경을 받아 store 를 갱신.
func (w *EtcdTableWatcher) watchLoop(ctx context.Context) {
	defer close(w.doneC)
	wch := w.cli.Watch(ctx, w.key)
	for {
		select {
		case <-w.stopC:
			return
		case <-ctx.Done():
			return
		case wresp, ok := <-wch:
			if !ok {
				w.logger.Warn("PricingTable watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.key)
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("PricingTable watch 에러", slog.Any("error", err))
				continue
			}
			w.applyEvents(wresp.Events)
		}
	}
}

func (w *EtcdTableWatcher) applyEvents(events []*clientv3.Event) {
	for _, ev := range events {
		switch ev.Type {
		case clientv3.EventTypePut:
			tbl, err := ParsePricingTable(ev.Kv.Value)
			if err != nil {
				w.logger.Warn("PricingTable PUT 파싱 실패", slog.Any("error", err))
				continue
			}
			w.store.Replace(tbl)
			w.logger.Info("PricingTable 갱신",
				slog.String("key", w.key),
				slog.Int64("version", tbl.Version),
			)
		case clientv3.EventTypeDelete:
			// 의도적 비활성화 — 빈 테이블로 교체할지 정책 결정 필요.
			// 현재는 무시 (마지막으로 알려진 테이블 유지) — 안전한 default.
			w.logger.Warn("PricingTable etcd key DELETE 감지 — 마지막 snapshot 유지",
				slog.String("key", w.key),
			)
		}
	}
}

// Close 는 watch goroutine 을 종료한다 (idempotent).
// etcd client 자체는 호출자가 관리.
func (w *EtcdTableWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
