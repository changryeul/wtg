package quoteid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// 권한 상수 — etcd value 의 permissions[] 에 들어가는 토큰. coarse 2-way
// 분리 (audit/운영도구는 read-only, 매칭 엔진은 read+write).
const (
	PermValidate     = "validate"      // Validate / BatchValidate
	PermMarkConsumed = "mark_consumed" // MarkConsumed / BatchMarkConsumed
)

// EngineMeta — engine_id 의 RBAC 메타데이터. etcd value JSON 으로 저장.
//
// 운영 예:
//
//	{"permissions":["validate","mark_consumed"],
//	 "expires_at":"2026-12-31T00:00:00Z",
//	 "contact":"trading-platform@bank.com"}
//
// 빈 value (= "") 면 backward compat 모드: 풀 권한 / no expiry / no contact.
// v1.12 동작과 정확히 호환.
type EngineMeta struct {
	// Permissions — 부여된 권한 토큰. 빈 슬라이스면 풀 권한 (default).
	// 알려진 토큰: PermValidate, PermMarkConsumed.
	Permissions []string `json:"permissions,omitempty"`
	// ExpiresAt — RFC3339 만료시각. 빈 문자열이면 무기한.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Contact — 운영자 식별 (감사 추적용). free-form.
	Contact string `json:"contact,omitempty"`
}

// HasPermission — Permissions 가 비어있으면 풀 권한 (default).
func (m EngineMeta) HasPermission(op string) bool {
	if len(m.Permissions) == 0 {
		return true
	}
	for _, p := range m.Permissions {
		if p == op {
			return true
		}
	}
	return false
}

// ExpiredAt — ExpiresAt 빈값이면 false (무기한). 비교 시각 t 와 RFC3339 파싱
// 실패 시 정책상 "만료 안 됨" 으로 fail-open (잘못된 JSON 으로 운영 중단 회피).
// 운영자가 발견하면 etcd 의 잘못된 값 수정.
func (m EngineMeta) ExpiredAt(t time.Time) bool {
	if m.ExpiresAt == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if err != nil {
		return false
	}
	return !t.Before(exp)
}

// parseEngineMeta — etcd value 를 EngineMeta 로. 빈 value 는 default meta.
// 잘못된 JSON 도 default meta + log (호출자) — 운영 안전성 우선.
func parseEngineMeta(raw []byte) (EngineMeta, error) {
	if len(raw) == 0 {
		return EngineMeta{}, nil
	}
	// JSON 이 아닌 free-form (예: 단순 "active") 도 허용 — backward compat.
	if raw[0] != '{' {
		return EngineMeta{}, nil
	}
	var m EngineMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return EngineMeta{}, fmt.Errorf("quoteid: EngineMeta JSON: %w", err)
	}
	return m, nil
}

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
	onApply func(map[string]EngineMeta)

	mu      sync.Mutex
	engines map[string]EngineMeta // <engine_id> → meta

	stopC chan struct{}
	doneC chan struct{}
}

// EtcdAllowlistWatcherOptions 는 EtcdAllowlistWatcher 생성 옵션.
type EtcdAllowlistWatcherOptions struct {
	Client *clientv3.Client
	// Prefix — 예: "wtg/quoteid/engines/". 끝에 "/" 자동 보정.
	Prefix string
	// OnApply — snapshot 갱신 콜백. nil 이면 사용 불가 (panic).
	// 각 engine_id 에 대한 EngineMeta 가 동봉 (빈 etcd value → 풀 권한 default).
	OnApply func(map[string]EngineMeta)
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
		engines: make(map[string]EngineMeta),
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
		if eng == "" {
			continue
		}
		meta, perr := parseEngineMeta(kv.Value)
		if perr != nil {
			w.logger.Warn("EngineMeta JSON 파싱 실패 — default 권한 적용",
				slog.String("engine_id", eng),
				slog.Any("error", perr))
		}
		w.engines[eng] = meta
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
			meta, perr := parseEngineMeta(ev.Kv.Value)
			if perr != nil {
				w.logger.Warn("EngineMeta JSON 파싱 실패 — default 권한 적용",
					slog.String("engine_id", eng),
					slog.Any("error", perr))
			}
			w.engines[eng] = meta
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

// snapshotLocked — engines map 의 defensive copy.
func (w *EtcdAllowlistWatcher) snapshotLocked() map[string]EngineMeta {
	out := make(map[string]EngineMeta, len(w.engines))
	for k, v := range w.engines {
		out[k] = v
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
