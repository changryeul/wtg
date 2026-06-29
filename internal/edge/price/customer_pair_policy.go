package price

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// CustomerPairPolicy — customer 별 ws 구독 허용 pair allowlist.
//
// 운영자가 mci-admin UI 에서 임의 부여한 customer ID 와 pair list 를 등록
// → etcd 에 저장 → 본 객체가 watch 로 동기화 → subscribeHandler 의 pair
// filter 단계에서 적용.
//
// 결합 정책 (gateSubscribe):
//   - customer 등록 안 됨 → 글로벌 정책만 적용 (unrestricted, backward compat)
//   - customer 등록 됨    → 글로벌 ∩ customer 허용 set (둘 다 통과해야)
//   - 글로벌 disallow     → 항상 우선 (emergency cut)
//   - 빈 list 등록 ([])   → "전체 차단" 의도 (registered=true, allowed 비어있음)
type CustomerPairPolicy interface {
	// AllowedFor — customer 의 허용 pair set. 등록 안 됐으면 (nil, false).
	// 등록됐으면 (allowed, true) — allowed 가 빈 슬라이스면 "전체 차단".
	AllowedFor(customerID string) (allowed []string, registered bool)
}

// MemoryCustomerPairPolicy — in-memory snapshot. atomic.Pointer 로 hot-swap.
// Lock 없이 다중 reader (subscribeHandler 의 hot path) 가 안전.
type MemoryCustomerPairPolicy struct {
	snapshot atomic.Pointer[map[string][]string]
}

// NewMemoryCustomerPairPolicy — 빈 set 으로 시작.
func NewMemoryCustomerPairPolicy() *MemoryCustomerPairPolicy {
	m := &MemoryCustomerPairPolicy{}
	empty := map[string][]string{}
	m.snapshot.Store(&empty)
	return m
}

// AllowedFor — interface 구현.
func (p *MemoryCustomerPairPolicy) AllowedFor(customerID string) ([]string, bool) {
	s := p.snapshot.Load()
	if s == nil {
		return nil, false
	}
	pairs, ok := (*s)[customerID]
	if !ok {
		return nil, false
	}
	return pairs, true
}

// Replace — 전체 snapshot 교체 (etcd watcher initial load).
func (p *MemoryCustomerPairPolicy) Replace(m map[string][]string) {
	// 정렬 보장 — 동일 입력 → 동일 출력.
	cp := make(map[string][]string, len(m))
	for k, v := range m {
		sv := append([]string(nil), v...)
		sort.Strings(sv)
		cp[k] = sv
	}
	p.snapshot.Store(&cp)
}

// Set — 단일 customer 의 pair list 교체 (etcd PUT event).
// 호출 시 정렬 + dedup.
func (p *MemoryCustomerPairPolicy) Set(customerID string, pairs []string) {
	old := p.snapshot.Load()
	n := make(map[string][]string, len(*old)+1)
	for k, v := range *old {
		n[k] = v
	}
	// dedup + sort.
	seen := make(map[string]struct{}, len(pairs))
	sorted := make([]string, 0, len(pairs))
	for _, p := range pairs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	n[customerID] = sorted
	p.snapshot.Store(&n)
}

// Delete — customer 등록 제거 (etcd DELETE event).
func (p *MemoryCustomerPairPolicy) Delete(customerID string) {
	old := p.snapshot.Load()
	if _, ok := (*old)[customerID]; !ok {
		return
	}
	n := make(map[string][]string, len(*old)-1)
	for k, v := range *old {
		if k == customerID {
			continue
		}
		n[k] = v
	}
	p.snapshot.Store(&n)
}

// Snapshot — 디버그/admin 노출용 전체 스냅샷 (deep copy).
func (p *MemoryCustomerPairPolicy) Snapshot() map[string][]string {
	s := p.snapshot.Load()
	if s == nil {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(*s))
	for k, v := range *s {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// EtcdCustomerPairWatcher — etcd prefix 아래 customer 별 pair list 를 watch
// 해서 MemoryCustomerPairPolicy 에 반영.
//
// etcd schema:
//
//	<prefix>/<customerID> = JSON []string  (예: ["USD/KRW","EUR/USD"])
//
// 시작 시 List → snapshot. 그 후 Watch 로 incremental sync. 끊김 자동 재시도.
type EtcdCustomerPairWatcher struct {
	client *clientv3.Client
	prefix string
	policy *MemoryCustomerPairPolicy
	logger *slog.Logger
	cancel context.CancelFunc
}

// NewEtcdCustomerPairWatcher — prefix 는 trailing "/" 자동 보정.
func NewEtcdCustomerPairWatcher(cli *clientv3.Client, prefix string,
	policy *MemoryCustomerPairPolicy, logger *slog.Logger) *EtcdCustomerPairWatcher {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EtcdCustomerPairWatcher{
		client: cli, prefix: prefix, policy: policy, logger: logger,
	}
}

// Start — initial load + watch goroutine 시작. ctx cancel 시 종료.
func (w *EtcdCustomerPairWatcher) Start(ctx context.Context) error {
	resp, err := w.client.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	init := make(map[string][]string, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		cid := strings.TrimPrefix(string(kv.Key), w.prefix)
		if cid == "" {
			continue
		}
		var pairs []string
		if err := json.Unmarshal(kv.Value, &pairs); err != nil {
			w.logger.Warn("customer-pair JSON parse 실패 — 무시",
				slog.String("customer", cid), slog.Any("error", err))
			continue
		}
		init[cid] = pairs
	}
	w.policy.Replace(init)
	w.logger.Info("CustomerPairPolicy 초기 로드",
		slog.Int("count", len(init)), slog.String("prefix", w.prefix))

	wctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.watchLoop(wctx, resp.Header.Revision+1)
	return nil
}

// Stop — watch goroutine 종료.
func (w *EtcdCustomerPairWatcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *EtcdCustomerPairWatcher) watchLoop(ctx context.Context, rev int64) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		ch := w.client.Watch(ctx, w.prefix,
			clientv3.WithPrefix(),
			clientv3.WithRev(rev))
		for resp := range ch {
			if err := resp.Err(); err != nil {
				w.logger.Warn("customer-pair watch error — 재시도",
					slog.Any("error", err), slog.Duration("backoff", backoff))
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
				}
				break
			}
			backoff = time.Second
			for _, ev := range resp.Events {
				cid := strings.TrimPrefix(string(ev.Kv.Key), w.prefix)
				if cid == "" {
					continue
				}
				switch ev.Type {
				case clientv3.EventTypePut:
					var pairs []string
					if err := json.Unmarshal(ev.Kv.Value, &pairs); err != nil {
						w.logger.Warn("customer-pair JSON parse 실패",
							slog.String("customer", cid), slog.Any("error", err))
						continue
					}
					w.policy.Set(cid, pairs)
					w.logger.Info("customer-pair PUT",
						slog.String("customer", cid),
						slog.Int("pairs", len(pairs)))
				case clientv3.EventTypeDelete:
					w.policy.Delete(cid)
					w.logger.Info("customer-pair DELETE",
						slog.String("customer", cid))
				}
			}
			rev = resp.Header.Revision + 1
		}
	}
}
