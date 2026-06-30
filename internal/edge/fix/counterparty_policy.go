package fix

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// CounterpartyPolicy — runtime counterparty 등록 lookup.
//
// quickfix.Acceptor 의 settings 는 startup 시 fix 라 runtime 변경 불가능.
// Phase B 의 etcd watch 기반 dynamic 등록은 settings 를 와일드카드 (`*`)
// 으로 두고 본 policy 가 FromAdmin 단계에서 검증하는 모델.
//
// 정책:
//   - 미등록 / disabled 카운터파티 → Logon reject (BusinessMessageReject)
//   - password 불일치 → Logon reject
//   - 등록 + enabled + password 일치 → Principal 주입 + Logon 허용
type CounterpartyPolicy interface {
	// Lookup — SenderCompID 의 counterparty 정보. 미등록이면 (Counterparty{}, false).
	Lookup(senderCompID string) (Counterparty, bool)

	// Snapshot — 전체 등록 list. 운영 진단 / admin 노출용.
	Snapshot() map[string]Counterparty
}

// MemoryCounterpartyPolicy — in-memory snapshot. atomic.Pointer 로 hot-swap.
// Lock 없이 다중 reader (FromAdmin 의 hot path) 안전.
type MemoryCounterpartyPolicy struct {
	snap atomic.Pointer[map[string]Counterparty]
}

// NewMemoryCounterpartyPolicy — 빈 set 으로 시작.
func NewMemoryCounterpartyPolicy() *MemoryCounterpartyPolicy {
	p := &MemoryCounterpartyPolicy{}
	empty := map[string]Counterparty{}
	p.snap.Store(&empty)
	return p
}

// Lookup — interface 구현.
func (p *MemoryCounterpartyPolicy) Lookup(cid string) (Counterparty, bool) {
	s := p.snap.Load()
	if s == nil {
		return Counterparty{}, false
	}
	cp, ok := (*s)[cid]
	return cp, ok
}

// Replace — 전체 snapshot 교체 (etcd watcher initial load).
func (p *MemoryCounterpartyPolicy) Replace(m map[string]Counterparty) {
	cp := make(map[string]Counterparty, len(m))
	for k, v := range m {
		cp[k] = v
	}
	p.snap.Store(&cp)
}

// Set — 단일 counterparty 등록 (etcd PUT event).
func (p *MemoryCounterpartyPolicy) Set(cid string, cp Counterparty) {
	old := p.snap.Load()
	n := make(map[string]Counterparty, len(*old)+1)
	for k, v := range *old {
		n[k] = v
	}
	n[cid] = cp
	p.snap.Store(&n)
}

// Delete — counterparty 제거 (etcd DELETE event).
func (p *MemoryCounterpartyPolicy) Delete(cid string) {
	old := p.snap.Load()
	if _, ok := (*old)[cid]; !ok {
		return
	}
	n := make(map[string]Counterparty, len(*old)-1)
	for k, v := range *old {
		if k == cid {
			continue
		}
		n[k] = v
	}
	p.snap.Store(&n)
}

// Snapshot — 디버그/admin 노출용 전체 스냅샷 (deep copy).
func (p *MemoryCounterpartyPolicy) Snapshot() map[string]Counterparty {
	s := p.snap.Load()
	if s == nil {
		return map[string]Counterparty{}
	}
	out := make(map[string]Counterparty, len(*s))
	for k, v := range *s {
		out[k] = v
	}
	return out
}

// EtcdCounterpartyWatcher — etcd prefix 아래 counterparty 등록을 watch 해서
// MemoryCounterpartyPolicy 에 반영.
//
// etcd schema:
//
//	<prefix><SenderCompID> = JSON Counterparty
//	  예: {"password":"...","channel":"FIX","site":"HQ","tier":"VIP","usid":"ECN_X"}
//
// 시작 시 List → snapshot. 그 후 Watch 로 incremental sync. 끊김 자동 재시도.
type EtcdCounterpartyWatcher struct {
	client *clientv3.Client
	prefix string
	policy *MemoryCounterpartyPolicy
	logger *slog.Logger
	cancel context.CancelFunc
}

// NewEtcdCounterpartyWatcher — prefix 는 trailing "/" 자동 보정.
func NewEtcdCounterpartyWatcher(cli *clientv3.Client, prefix string,
	policy *MemoryCounterpartyPolicy, logger *slog.Logger) *EtcdCounterpartyWatcher {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EtcdCounterpartyWatcher{
		client: cli, prefix: prefix, policy: policy, logger: logger,
	}
}

// Start — initial load + watch goroutine 시작. ctx cancel 시 종료.
func (w *EtcdCounterpartyWatcher) Start(ctx context.Context) error {
	resp, err := w.client.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	init := make(map[string]Counterparty, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		cid := strings.TrimPrefix(string(kv.Key), w.prefix)
		if cid == "" {
			continue
		}
		var cp Counterparty
		if err := json.Unmarshal(kv.Value, &cp); err != nil {
			w.logger.Warn("counterparty JSON parse 실패 — 무시",
				slog.String("cid", cid), slog.Any("error", err))
			continue
		}
		init[cid] = cp
	}
	w.policy.Replace(init)
	w.logger.Info("CounterpartyPolicy 초기 로드",
		slog.Int("count", len(init)), slog.String("prefix", w.prefix))

	wctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.watchLoop(wctx, resp.Header.Revision+1)
	return nil
}

// Stop — watch goroutine 종료.
func (w *EtcdCounterpartyWatcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *EtcdCounterpartyWatcher) watchLoop(ctx context.Context, rev int64) {
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
				w.logger.Warn("counterparty watch error — 재시도",
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
					var cp Counterparty
					if err := json.Unmarshal(ev.Kv.Value, &cp); err != nil {
						w.logger.Warn("counterparty JSON parse 실패",
							slog.String("cid", cid), slog.Any("error", err))
						continue
					}
					w.policy.Set(cid, cp)
					w.logger.Info("counterparty PUT", slog.String("cid", cid),
						slog.String("profile", cp.Channel+"."+cp.Site+"."+cp.Tier))
				case clientv3.EventTypeDelete:
					w.policy.Delete(cid)
					w.logger.Info("counterparty DELETE", slog.String("cid", cid))
				}
			}
			rev = resp.Header.Revision + 1
		}
	}
}

// staticPolicy — Phase A 의 정적 seed 와 Phase B 의 policy 를 통일하기 위한
// adapter. cfg.Counterparties 가 채워졌으면 그 안에서 lookup.
type staticPolicy struct {
	m map[string]Counterparty
}

func (s *staticPolicy) Lookup(cid string) (Counterparty, bool) {
	cp, ok := s.m[cid]
	return cp, ok
}
func (s *staticPolicy) Snapshot() map[string]Counterparty {
	out := make(map[string]Counterparty, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}
