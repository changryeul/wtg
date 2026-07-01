package md

import "sync"

// Counterparty — MD counterparty 1개의 인증·라우팅 정보. Phase A 는 최소 필드
// (Password / Usid) 만. Phase B 에서 mdReqRoleSet / mdAllowedPairs 추가 예정.
type Counterparty struct {
	// Password — Logon (35=A) tag 554 검증. 빈값이면 검증 skip (dev only).
	Password string `json:"password"`

	// Profile — Principal.Channel/Site/Tier. Phase B 에서 mci-price 의
	// SubscribeQuote(profile_key) 로 upstream 을 붙일 때 사용.
	Channel string `json:"channel"`
	Site    string `json:"site"`
	Tier    string `json:"tier"`

	// Usid — log / audit 의 일상 ID.
	Usid string `json:"usid"`
}

// CounterpartyPolicy — runtime 카운터파티 정책 조회 (Logon 시).
//
// Phase A 는 staticPolicy 만. Phase B 에서 etcd watch 로 MemoryCounterpartyPolicy
// 추가 예정 (mci-edge-fix 와 동일 store 재사용 검토).
type CounterpartyPolicy interface {
	Lookup(senderCompID string) (Counterparty, bool)
	Snapshot() map[string]Counterparty
}

// staticPolicy — 부팅 시 seed 로 정지된 정책. reload 없이는 안 바뀜.
type staticPolicy struct {
	m map[string]Counterparty
}

func (p *staticPolicy) Lookup(cid string) (Counterparty, bool) {
	if p.m == nil {
		return Counterparty{}, false
	}
	cp, ok := p.m[cid]
	return cp, ok
}

func (p *staticPolicy) Snapshot() map[string]Counterparty {
	out := make(map[string]Counterparty, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

// MemoryCounterpartyPolicy — Phase B 대비 skeleton. etcd watcher 가 Replace 로
// 갱신. Phase A 는 사용 X — staticPolicy 로 우회.
type MemoryCounterpartyPolicy struct {
	mu sync.RWMutex
	m  map[string]Counterparty
}

func NewMemoryCounterpartyPolicy() *MemoryCounterpartyPolicy {
	return &MemoryCounterpartyPolicy{m: make(map[string]Counterparty)}
}

func (p *MemoryCounterpartyPolicy) Replace(next map[string]Counterparty) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m = make(map[string]Counterparty, len(next))
	for k, v := range next {
		p.m[k] = v
	}
}

func (p *MemoryCounterpartyPolicy) Lookup(cid string) (Counterparty, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cp, ok := p.m[cid]
	return cp, ok
}

func (p *MemoryCounterpartyPolicy) Snapshot() map[string]Counterparty {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]Counterparty, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}
