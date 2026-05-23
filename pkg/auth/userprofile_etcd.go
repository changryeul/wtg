package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdUserProfileResolver 는 etcd prefix 아래 per-usid JSON 으로 사용자 프로파일을
// 보관하고 watch 로 hot reload 한다.
//
// 키 컨벤션 (default): "wtg/auth/user-profiles/{usid}" → UserProfile JSON.
//
// 다중 mci-api 인스턴스에서도 mci-admin 의 PUT/DELETE 가 즉시 전파됨.
// Resolve 는 atomic.Pointer 로 lock-free read.
type EtcdUserProfileResolver struct {
	cli    *clientv3.Client
	prefix string
	logger *slog.Logger

	// entries — raw etcd 상태 (key=etcd key → UserProfile).
	mu      sync.Mutex
	entries map[string]UserProfile

	// snap — Resolve 의 hot path 용. usid → UserProfile.
	snap atomic.Pointer[map[string]UserProfile]

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdUserProfileResolverOptions 는 생성 옵션.
type EtcdUserProfileResolverOptions struct {
	Client *clientv3.Client
	Prefix string // default "wtg/auth/user-profiles/"
	Logger *slog.Logger
}

// NewEtcdUserProfileResolver 는 etcd 에서 카탈로그를 1회 로드하고 watch 를 시작한다.
func NewEtcdUserProfileResolver(ctx context.Context, opt EtcdUserProfileResolverOptions) (*EtcdUserProfileResolver, error) {
	if opt.Client == nil {
		return nil, errors.New("auth: etcd Client 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/auth/user-profiles/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	r := &EtcdUserProfileResolver{
		cli:     opt.Client,
		prefix:  prefix,
		logger:  logger,
		entries: make(map[string]UserProfile),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	empty := map[string]UserProfile{}
	r.snap.Store(&empty)

	if err := r.initialLoad(ctx); err != nil {
		return nil, err
	}
	go r.watchLoop(ctx)
	return r, nil
}

func (r *EtcdUserProfileResolver) initialLoad(ctx context.Context) error {
	resp, err := r.cli.Get(ctx, r.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("auth: user-profile etcd 초기 Get: %w", err)
	}
	r.mu.Lock()
	for _, kv := range resp.Kvs {
		var p UserProfile
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			r.logger.Warn("UserProfile JSON 파싱 실패 (skip)",
				slog.String("key", string(kv.Key)),
				slog.Any("error", err),
			)
			continue
		}
		usid := strings.TrimPrefix(string(kv.Key), r.prefix)
		r.entries[usid] = p
	}
	r.mu.Unlock()
	r.rebuild()
	r.logger.Info("UserProfile etcd 초기 로드",
		slog.String("prefix", r.prefix),
		slog.Int("count", len(*r.snap.Load())),
	)
	return nil
}

func (r *EtcdUserProfileResolver) watchLoop(ctx context.Context) {
	defer close(r.doneC)
	wch := r.cli.Watch(ctx, r.prefix, clientv3.WithPrefix())
	for {
		select {
		case <-r.stopC:
			return
		case <-ctx.Done():
			return
		case wresp, ok := <-wch:
			if !ok {
				r.logger.Warn("UserProfile watch 채널 종료 — 재등록")
				wch = r.cli.Watch(ctx, r.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				r.logger.Warn("UserProfile watch 에러", slog.Any("error", err))
				continue
			}
			r.applyEvents(wresp.Events)
		}
	}
}

func (r *EtcdUserProfileResolver) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	r.mu.Lock()
	for _, ev := range events {
		usid := strings.TrimPrefix(string(ev.Kv.Key), r.prefix)
		switch ev.Type {
		case clientv3.EventTypePut:
			var p UserProfile
			if err := json.Unmarshal(ev.Kv.Value, &p); err != nil {
				r.logger.Warn("UserProfile PUT 파싱 실패",
					slog.String("key", string(ev.Kv.Key)),
					slog.Any("error", err),
				)
				continue
			}
			r.entries[usid] = p
		case clientv3.EventTypeDelete:
			delete(r.entries, usid)
		}
	}
	r.mu.Unlock()
	r.rebuild()
}

func (r *EtcdUserProfileResolver) rebuild() {
	r.mu.Lock()
	next := make(map[string]UserProfile, len(r.entries))
	for k, v := range r.entries {
		next[k] = v
	}
	r.mu.Unlock()
	r.snap.Store(&next)
}

// Resolve — 미등록은 ErrUserProfileNotFound.
func (r *EtcdUserProfileResolver) Resolve(_ context.Context, usid string) (UserProfile, error) {
	m := r.snap.Load()
	if m == nil {
		return UserProfile{}, ErrUserProfileNotFound
	}
	if v, ok := (*m)[usid]; ok {
		return v, nil
	}
	return UserProfile{}, ErrUserProfileNotFound
}

// Close 는 watch goroutine 종료 (idempotent). etcd client lifecycle 은 호출자 관리.
func (r *EtcdUserProfileResolver) Close() error {
	r.stopOnce.Do(func() {
		close(r.stopC)
		<-r.doneC
	})
	return nil
}

// 컴파일 보장.
var _ UserProfileResolver = (*EtcdUserProfileResolver)(nil)
