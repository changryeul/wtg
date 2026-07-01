package md

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Counterparty — MD counterparty 1개의 인증·라우팅 정보.
//
// etcd store 는 edge-fix 와 동일 key (`wtg/fix/counterparties/<CID>`).
// JSON 태그는 edge-fix.Counterparty 와 forward-compatible — 새 필드
// (MdReqRoleSet / MdAllowedPairs) 는 옵션이라 미지원 클라이언트는 무시.
//
// mci-edge-md 는 초기 로드 시 MdReqRoleSet 에 "MD" 가 있는 항목만 accept.
// 그 외는 edge-fix 전용 등록으로 간주하고 skip.
type Counterparty struct {
	Password string `json:"password"`
	Channel  string `json:"channel"`
	Site     string `json:"site"`
	Tier     string `json:"tier"`
	Usid     string `json:"usid"`

	// MdReqRoleSet — 이 카운터파티가 열 수 있는 채널. 예: ["MD"], ["ORDER","MD"].
	// 비어있으면 mci-edge-md 는 skip (default = ORDER only).
	MdReqRoleSet []string `json:"md_req_role_set,omitempty"`

	// MdAllowedPairs — 구독 허용 pair whitelist. 비어있으면 profile 전체 허용
	// (Phase B 는 필터 skip, Phase C 에서 반영).
	MdAllowedPairs []string `json:"md_allowed_pairs,omitempty"`
}

// AllowsMD — MdReqRoleSet 에 "MD" 가 포함되면 true. 대소문자 무시.
func (c Counterparty) AllowsMD() bool {
	for _, r := range c.MdReqRoleSet {
		if strings.EqualFold(strings.TrimSpace(r), "MD") {
			return true
		}
	}
	return false
}

// CounterpartyPolicy — runtime lookup.
type CounterpartyPolicy interface {
	Lookup(senderCompID string) (Counterparty, bool)
	Snapshot() map[string]Counterparty
}

// MemoryCounterpartyPolicy — atomic.Pointer swap 으로 lock 없는 다중 reader.
// edge-fix.MemoryCounterpartyPolicy 와 동일 패턴.
type MemoryCounterpartyPolicy struct {
	snap atomic.Pointer[map[string]Counterparty]
}

func NewMemoryCounterpartyPolicy() *MemoryCounterpartyPolicy {
	p := &MemoryCounterpartyPolicy{}
	empty := map[string]Counterparty{}
	p.snap.Store(&empty)
	return p
}

func (p *MemoryCounterpartyPolicy) Lookup(cid string) (Counterparty, bool) {
	s := p.snap.Load()
	if s == nil {
		return Counterparty{}, false
	}
	cp, ok := (*s)[cid]
	return cp, ok
}

func (p *MemoryCounterpartyPolicy) Replace(m map[string]Counterparty) {
	cp := make(map[string]Counterparty, len(m))
	for k, v := range m {
		cp[k] = v
	}
	p.snap.Store(&cp)
}

func (p *MemoryCounterpartyPolicy) Set(cid string, cp Counterparty) {
	old := p.snap.Load()
	n := make(map[string]Counterparty, len(*old)+1)
	for k, v := range *old {
		n[k] = v
	}
	n[cid] = cp
	p.snap.Store(&n)
}

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

// staticPolicy — Phase A 호환 — 부팅 seed 만.
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

// EtcdCounterpartyWatcher — etcd prefix 아래 counterparty 등록을 watch.
// edge-fix 와 동일 store (`wtg/fix/counterparties/`) 를 읽되 AllowsMD() 필터.
type EtcdCounterpartyWatcher struct {
	client *clientv3.Client
	prefix string
	policy *MemoryCounterpartyPolicy
	logger *slog.Logger
	cancel context.CancelFunc
}

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

// Start — initial load + watch goroutine.
//
// Filter: AllowsMD() 인 항목만 policy 에 반영. 그 외는 skip (edge-fix 전용).
func (w *EtcdCounterpartyWatcher) Start(ctx context.Context) error {
	resp, err := w.client.Get(ctx, w.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	init := make(map[string]Counterparty, len(resp.Kvs))
	skipped := 0
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
		if !cp.AllowsMD() {
			skipped++
			continue
		}
		init[cid] = cp
	}
	w.policy.Replace(init)
	w.logger.Info("MD CounterpartyPolicy 초기 로드",
		slog.Int("count", len(init)),
		slog.Int("skipped_non_md", skipped),
		slog.String("prefix", w.prefix))

	wctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.watchLoop(wctx, resp.Header.Revision+1)
	return nil
}

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
				w.logger.Warn("MD counterparty watch error — 재시도",
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
						w.logger.Warn("MD counterparty JSON parse 실패",
							slog.String("cid", cid), slog.Any("error", err))
						continue
					}
					if !cp.AllowsMD() {
						// MD role 사라진 경우 기존 있으면 제거.
						w.policy.Delete(cid)
						w.logger.Info("counterparty MD role 제거 — policy 에서 삭제",
							slog.String("cid", cid))
						continue
					}
					w.policy.Set(cid, cp)
					w.logger.Info("MD counterparty PUT",
						slog.String("cid", cid),
						slog.String("profile", cp.Channel+"."+cp.Site+"."+cp.Tier),
						slog.Int("allowed_pairs", len(cp.MdAllowedPairs)))
				case clientv3.EventTypeDelete:
					w.policy.Delete(cid)
					w.logger.Info("MD counterparty DELETE", slog.String("cid", cid))
				}
			}
			rev = resp.Header.Revision + 1
		}
	}
}
