package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// PolicyDoc 은 etcd 에 저장되는 rate limit 정책의 wire format.
//
// 예시:
//
//	{
//	  "version": 1,
//	  "rules": [
//	    {"pattern": "POST /v1/login", "rate": 5,  "burst": 10},
//	    {"pattern": "POST /v1/tx",    "rate": 50, "burst": 100}
//	  ],
//	  "fallback": {"rate": 100, "burst": 200}
//	}
//
// fallback 이 nil 이면 매칭 안 된 path 는 통과 (한도 X).
type PolicyDoc struct {
	Version  int64        `json:"version,omitempty"`
	Rules    []Rule       `json:"rules"`
	Fallback *FallbackCfg `json:"fallback,omitempty"`
}

// FallbackCfg — fallback 한도. Config 자체를 JSON 으로 노출하지 않으려고 별도.
type FallbackCfg struct {
	Rate  float64 `json:"rate"`
	Burst int     `json:"burst"`
}

// ToConfig — FallbackCfg → ratelimit.Config 변환 (idle/eviction 은 default).
func (f *FallbackCfg) ToConfig() *Config {
	if f == nil {
		return nil
	}
	return &Config{
		RatePerSec: f.Rate,
		Burst:      f.Burst,
	}
}

// EtcdWatcher — 단일 key 의 PolicyDoc JSON 을 watch 해서 RuleSet 을 hot-swap.
//
// 패턴:
//   - 시작 시 key Get → PolicyDoc → rs.Replace
//   - key 미존재 → opt.Defaults 사용 (혹은 Replace 안 함)
//   - 백그라운드 watch goroutine 이 PUT 이벤트마다 rs.Replace
//   - DELETE 이벤트 → opt.Defaults 로 복원 (있으면) 또는 빈 룰셋
type EtcdWatcher struct {
	cli      *clientv3.Client
	key      string
	rs       *RuleSet
	logger   *slog.Logger
	defaults []Rule
	fallback *FallbackCfg

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdWatcherOptions — EtcdWatcher 생성 옵션.
type EtcdWatcherOptions struct {
	// Client — 호출자가 dial 한 etcd v3 client. 필수.
	Client *clientv3.Client
	// Key — 단일 PolicyDoc key (예: "wtg/ratelimit/edge-api"). 필수.
	Key string
	// RuleSet — hot-swap 대상. 필수.
	RuleSet *RuleSet
	// Defaults — etcd 에 PolicyDoc 이 없거나 DELETE 시 적용할 룰 (예:
	// 컴파일 타임 DefaultRateLimitRules). nil 이면 룰 비활성.
	Defaults []Rule
	// Fallback — Defaults 와 함께 적용할 fallback (PolicyDoc.Fallback 미설정 시).
	Fallback *FallbackCfg
	// Logger — 옵셔널.
	Logger *slog.Logger
}

// NewEtcdWatcher — etcd 에서 정책 1회 로드 후 watch 시작.
func NewEtcdWatcher(ctx context.Context, opt EtcdWatcherOptions) (*EtcdWatcher, error) {
	if opt.Client == nil {
		return nil, errors.New("ratelimit: etcd Client 필수")
	}
	if opt.Key == "" {
		return nil, errors.New("ratelimit: Key 필수")
	}
	if opt.RuleSet == nil {
		return nil, errors.New("ratelimit: RuleSet 필수")
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &EtcdWatcher{
		cli:      opt.Client,
		key:      opt.Key,
		rs:       opt.RuleSet,
		logger:   logger,
		defaults: opt.Defaults,
		fallback: opt.Fallback,
		stopC:    make(chan struct{}),
		doneC:    make(chan struct{}),
	}
	if err := w.initialLoad(ctx); err != nil {
		return nil, err
	}
	go w.watchLoop(ctx)
	return w, nil
}

// initialLoad — key Get 후 Replace. key 미존재면 defaults 로.
func (w *EtcdWatcher) initialLoad(ctx context.Context) error {
	resp, err := w.cli.Get(ctx, w.key)
	if err != nil {
		return fmt.Errorf("ratelimit: etcd 초기 Get: %w", err)
	}
	if len(resp.Kvs) == 0 {
		w.logger.Info("ratelimit etcd key 미존재 — defaults 적용",
			slog.String("key", w.key),
			slog.Int("default_rules", len(w.defaults)),
		)
		return w.rs.Replace(w.defaults, w.fallback.ToConfig())
	}
	return w.applyValue(resp.Kvs[0].Value, "initial")
}

// applyValue — PolicyDoc JSON 파싱 → Replace.
func (w *EtcdWatcher) applyValue(val []byte, kind string) error {
	var doc PolicyDoc
	if err := json.Unmarshal(val, &doc); err != nil {
		w.logger.Warn("ratelimit etcd PolicyDoc 파싱 실패 — 무시",
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return nil // 잘못된 doc 으로 운영 중단 회피 — 기존 룰 유지.
	}
	fb := doc.Fallback
	if fb == nil {
		fb = w.fallback
	}
	if err := w.rs.Replace(doc.Rules, fb.ToConfig()); err != nil {
		w.logger.Warn("ratelimit Replace 실패 — 기존 룰 유지",
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return nil
	}
	w.logger.Info("ratelimit 정책 갱신",
		slog.String("kind", kind),
		slog.Int64("version", doc.Version),
		slog.Int("rules", len(doc.Rules)),
		slog.Bool("fallback", fb != nil),
	)
	return nil
}

// watchLoop — key 변경 이벤트 처리.
func (w *EtcdWatcher) watchLoop(ctx context.Context) {
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
				w.logger.Warn("ratelimit watch 채널 종료 — 재등록")
				wch = w.cli.Watch(ctx, w.key)
				continue
			}
			if err := wresp.Err(); err != nil {
				w.logger.Warn("ratelimit watch 에러", slog.Any("error", err))
				continue
			}
			for _, ev := range wresp.Events {
				switch ev.Type {
				case clientv3.EventTypePut:
					_ = w.applyValue(ev.Kv.Value, "watch-put")
				case clientv3.EventTypeDelete:
					w.logger.Info("ratelimit key 삭제 — defaults 복원",
						slog.String("key", w.key))
					_ = w.rs.Replace(w.defaults, w.fallback.ToConfig())
				}
			}
		}
	}
}

// Close — watch goroutine 종료. etcd client 는 호출자 관리.
func (w *EtcdWatcher) Close() error {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
	})
	return nil
}
