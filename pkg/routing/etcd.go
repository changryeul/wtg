package routing

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdRegistry 는 etcd v3 backed Registry — mci-api ↔ mci-admin 간 룰을 공유.
//
// 동작:
//
//   - 모든 룰을 prefix (default: "wtg/routes/") 아래 alias 로 저장 (JSON value).
//   - 시작 시 prefix Get 으로 로컬 캐시 채움.
//   - 백그라운드 watch goroutine 이 변경을 받아 캐시 갱신 → 다른 인스턴스 변경 즉시 반영.
//   - Get/List 는 로컬 캐시에서 (etcd round-trip 없음) — 핫 패스 빠름.
//   - Put/Delete/SetActive 는 etcd 에 직접 쓰기 → watch 가 self & 다른 인스턴스에 전파.
//
// 운영 토폴로지: 다중 mci-api / 단일 mci-admin 환경에서도 admin 의 변경이
// 모든 api 인스턴스에 자동 전파.
type EtcdRegistry struct {
	cli    *clientv3.Client
	prefix string
	logger *slog.Logger
	now    func() time.Time

	mu    sync.RWMutex
	rules map[string]*Rule

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// EtcdRegistryOptions 는 EtcdRegistry 생성 옵션.
type EtcdRegistryOptions struct {
	// Endpoints 예: ["etcd-0:2379", "etcd-1:2379"]. 필수.
	Endpoints []string
	// Prefix 가 비면 "wtg/routes/" 사용. 끝에 "/" 자동 보정.
	Prefix string
	// DialTimeout 가 0 이면 5s.
	DialTimeout time.Duration
	// Username/Password — 인증된 etcd 사용 시.
	Username string
	Password string
	// TLS — *clientv3.Config 통째 override (호환용; 신규 코드는 TLSConfig 사용 권장).
	TLS *clientv3.Config
	// TLSConfig — clientv3.Config.TLS 에 직접 적용되는 *tls.Config.
	// pkg/tlsutil.LoadClient 결과 그대로 넘기면 된다 (nil = 평문).
	TLSConfig *tls.Config
	// Logger — 옵셔널.
	Logger *slog.Logger
	// Now — 테스트용 시간 주입.
	Now func() time.Time
}

// NewEtcdRegistry 는 클라이언트를 dial 하고 초기 캐시를 채운 뒤 반환한다.
//
// dial 또는 초기 동기화 실패 시 에러. ctx 는 부트스트랩 + 백그라운드 watch
// 모두에 영향 — 호출자 종료 시 ctx cancel 하면 watch 가 정리된다 (Close 도 별도).
func NewEtcdRegistry(ctx context.Context, opt EtcdRegistryOptions) (*EtcdRegistry, error) {
	if len(opt.Endpoints) == 0 && opt.TLS == nil {
		return nil, errors.New("routing: etcd Endpoints 필수")
	}
	cfg := clientv3.Config{
		Endpoints:   opt.Endpoints,
		DialTimeout: opt.DialTimeout,
		Username:    opt.Username,
		Password:    opt.Password,
	}
	if opt.TLS != nil {
		// 호환 path — 통째 override.
		cfg = *opt.TLS
	}
	if opt.TLSConfig != nil {
		// 신규 path — *tls.Config 만 주입 (다른 필드는 위에서 세팅된 그대로).
		cfg.TLS = opt.TLSConfig
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	cli, err := clientv3.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("routing: etcd dial: %w", err)
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg/routes/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}

	r := &EtcdRegistry{
		cli:    cli,
		prefix: prefix,
		logger: logger,
		now:    now,
		rules:  make(map[string]*Rule),
		stopC:  make(chan struct{}),
		doneC:  make(chan struct{}),
	}

	if err := r.initialLoad(ctx); err != nil {
		_ = cli.Close()
		return nil, err
	}
	go r.watchLoop(ctx)
	return r, nil
}

// initialLoad 는 prefix Get 으로 모든 룰을 로컬 캐시에 채운다.
func (r *EtcdRegistry) initialLoad(ctx context.Context) error {
	resp, err := r.cli.Get(ctx, r.prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("routing: etcd 초기 Get: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, kv := range resp.Kvs {
		alias := r.aliasFromKey(string(kv.Key))
		if alias == "" {
			continue
		}
		var rule Rule
		if err := json.Unmarshal(kv.Value, &rule); err != nil {
			r.logger.Warn("routing: etcd 룰 파싱 실패",
				slog.String("key", string(kv.Key)),
				slog.Any("error", err),
			)
			continue
		}
		rule.Alias = alias
		r.rules[alias] = &rule
	}
	r.logger.Info("EtcdRegistry 초기 로드", slog.Int("count", len(r.rules)), slog.String("prefix", r.prefix))
	return nil
}

// watchLoop 은 prefix watch 로 변경을 받아 캐시를 갱신한다.
func (r *EtcdRegistry) watchLoop(ctx context.Context) {
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
				r.logger.Warn("EtcdRegistry watch 채널 종료 — 재등록 시도")
				wch = r.cli.Watch(ctx, r.prefix, clientv3.WithPrefix())
				continue
			}
			if err := wresp.Err(); err != nil {
				r.logger.Warn("EtcdRegistry watch 에러", slog.Any("error", err))
				continue
			}
			r.applyEvents(wresp.Events)
		}
	}
}

func (r *EtcdRegistry) applyEvents(events []*clientv3.Event) {
	if len(events) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range events {
		alias := r.aliasFromKey(string(ev.Kv.Key))
		if alias == "" {
			continue
		}
		switch ev.Type {
		case clientv3.EventTypePut:
			var rule Rule
			if err := json.Unmarshal(ev.Kv.Value, &rule); err != nil {
				r.logger.Warn("routing: watch put 파싱 실패", slog.Any("error", err))
				continue
			}
			rule.Alias = alias
			r.rules[alias] = &rule
		case clientv3.EventTypeDelete:
			delete(r.rules, alias)
		}
	}
}

func (r *EtcdRegistry) aliasFromKey(key string) string {
	if !strings.HasPrefix(key, r.prefix) {
		return ""
	}
	return key[len(r.prefix):]
}

func (r *EtcdRegistry) keyOf(alias string) string {
	return r.prefix + alias
}

// Get — 캐시에서 조회.
func (r *EtcdRegistry) Get(alias string) (*Rule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, ok := r.rules[alias]
	if !ok {
		return nil, ErrRouteNotFound
	}
	cp := *rule
	return &cp, nil
}

// List — 캐시 정렬 반환.
func (r *EtcdRegistry) List() []*Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Rule, 0, len(r.rules))
	for _, v := range r.rules {
		cp := *v
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// Put — etcd 에 쓰고 watch 가 캐시 갱신을 책임 (즉시 일관성을 위해 로컬도 즉시 업데이트).
func (r *EtcdRegistry) Put(rule *Rule, updatedBy string) error {
	if rule == nil {
		return ErrAliasRequired
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	cp := *rule
	cp.UpdatedAt = r.now()
	cp.UpdatedBy = updatedBy

	value, err := json.Marshal(&cp)
	if err != nil {
		return fmt.Errorf("routing: etcd marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.cli.Put(ctx, r.keyOf(cp.Alias), string(value)); err != nil {
		return fmt.Errorf("routing: etcd Put: %w", err)
	}
	// 로컬 캐시 즉시 반영 — watch 가 도착하기 전이라도 caller 가 Get 하면 보임.
	r.mu.Lock()
	r.rules[cp.Alias] = &cp
	r.mu.Unlock()
	return nil
}

// Delete — etcd 에서 삭제.
func (r *EtcdRegistry) Delete(alias string) error {
	// 미존재 검증을 위해 캐시 확인.
	r.mu.RLock()
	_, ok := r.rules[alias]
	r.mu.RUnlock()
	if !ok {
		return ErrRouteNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.cli.Delete(ctx, r.keyOf(alias)); err != nil {
		return fmt.Errorf("routing: etcd Delete: %w", err)
	}
	r.mu.Lock()
	delete(r.rules, alias)
	r.mu.Unlock()
	return nil
}

// SetActive — Get + 수정 + Put. etcd transaction (compare-and-swap) 으로 race 안전화 가능하지만,
// 1차 prototype 은 단순 read-modify-write. 운영에서 동시 수정 충돌 발생 시 watch 가 결국 안정.
func (r *EtcdRegistry) SetActive(alias string, active bool, updatedBy string) error {
	r.mu.RLock()
	cur, ok := r.rules[alias]
	r.mu.RUnlock()
	if !ok {
		return ErrRouteNotFound
	}
	cp := *cur
	cp.Active = active
	return r.Put(&cp, updatedBy)
}

// Close — watch 정리, etcd 클라이언트 종료.
func (r *EtcdRegistry) Close() error {
	r.stopOnce.Do(func() {
		close(r.stopC)
		// watch 가 ctx 또는 stopC 로 종료되도록 doneC 대기.
		select {
		case <-r.doneC:
		case <-time.After(2 * time.Second):
			// 강제 진행.
		}
	})
	return r.cli.Close()
}
