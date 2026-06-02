package price

import (
	"sort"
	"sync"
)

// PairValidator — Phase 2 권한 가드 인터페이스. handleControlMessage 의
// subscribe 가 각 pair 마다 IsAllowed 호출.
//
// nil 구현은 "모든 pair 허용" 으로 간주 (Server 가 nil 체크 — 회귀 없는
// backward compat 경로).
type PairValidator interface {
	IsAllowed(pair string) bool
	// AllowedSnapshot — 현재 허용 set 의 sorted 스냅샷 (디버그 / admin
	// 가시화). 호출 비용이 큰 구현은 nil 반환 가능.
	AllowedSnapshot() []string
}

// MemoryPairValidator — in-memory set. 두 source 결합:
//
//  1. operator seed       (Add 로 초기 주입 — config 의 --quote-seed-pairs)
//  2. passive learning    (consumeQuoteOnce 가 도착 quote.Pair 마다 Add)
//
// 운영자가 시드를 주면 즉시 가드 동작, 안 줘도 첫 quote 도착 후 점진 확장.
// 한 번 허용된 pair 는 제거되지 않음 (PricingTable 에서 sym 비활성 → tick
// 안 옴 → 자연 비활성. 명시적 revoke 가 필요하면 Phase 4 의 admin override).
type MemoryPairValidator struct {
	mu      sync.RWMutex
	allowed map[string]struct{}
}

// NewMemoryPairValidator — 빈 set 으로 시작. operator seed 는 Add 로.
func NewMemoryPairValidator() *MemoryPairValidator {
	return &MemoryPairValidator{allowed: make(map[string]struct{})}
}

// Add — pair 들을 허용 set 에 추가 (idempotent). 빈 문자열은 무시.
func (v *MemoryPairValidator) Add(pairs ...string) {
	if len(pairs) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range pairs {
		if p != "" {
			v.allowed[p] = struct{}{}
		}
	}
}

// Remove — Phase 4b admin override. pair 를 허용 set 에서 제거 (idempotent).
// 호출 후 IsAllowed(pair) = false → 신규 subscribe 차단.
func (v *MemoryPairValidator) Remove(pairs ...string) {
	if len(pairs) == 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range pairs {
		delete(v.allowed, p)
	}
}

// IsAllowed — pair 가 허용 set 에 있는지.
func (v *MemoryPairValidator) IsAllowed(pair string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.allowed[pair]
	return ok
}

// AllowedSnapshot — sorted 스냅샷.
func (v *MemoryPairValidator) AllowedSnapshot() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]string, 0, len(v.allowed))
	for p := range v.allowed {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
