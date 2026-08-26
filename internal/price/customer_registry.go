package price

import (
	"sync"
	"sync/atomic"

	"github.com/winwaysystems/wtg/pkg/session"
)

// CustomerRegistry — Phase 4a. 활성 customer ID → Profile mapping 보관소.
//
// 운영 시나리오:
//   - mci-edge-price 가 ws 클라이언트 연결 시 customerID + Profile 을 mci-price
//     에 등록 (Phase 4b 에서 gRPC RPC 또는 stream metadata 로 구현).
//   - PricingConsumer 가 매 tick 마다 Snapshot 으로 순회하며 등록된 customer
//     마다 PricingTable.ApplyForCustomer(.., customerID) → CustomerQuotePublisher.
//   - 등록된 customer 가 0 이면 customer-quote 경로 자체가 작동 안 함 (lazy).
//
// 동시성:
//   - Register / Unregister 는 edge-price 의 ws connect/disconnect goroutine 들이
//     호출. PricingConsumer.OnTick 은 broker subscribe goroutine.
//   - 모든 entry 가 sync.Map 키. 운영 customer 수 (수천~수만) 대비 OnTick path
//     이 단일 reader 라 sync.Map 의 zero-allocation read path 가 유리.
type CustomerRegistry struct {
	// entries: customerID(string) → customerReg
	entries sync.Map

	// count: sync.Map 크기를 정확히 추적 — Snapshot pre-allocate 용.
	// Register/Unregister 가 LoadOrStore / LoadAndDelete 결과에 따라 증감.
	count atomic.Int64

	// srcCount: sources(비어있지 않은) 를 가진 customer 수. per-source pricing
	// 경로의 fast-path gate — 0 이면 per-source 처리 자체를 skip.
	srcCount atomic.Int64
}

// customerReg — registry 내부 값. profile + 원하는 LP 원천 집합.
// sources 가 비어있으면 BEST 만 (기존 동작).
type customerReg struct {
	profile session.Profile
	sources map[string]struct{} // nil/빈 = BEST only
}

// NewCustomerRegistry 는 빈 registry 를 반환한다.
func NewCustomerRegistry() *CustomerRegistry {
	return &CustomerRegistry{}
}

// Register — 신규 customer 등록 또는 기존 entry 갱신 (BEST 만, 기존 동작).
func (r *CustomerRegistry) Register(customerID string, profile session.Profile) {
	r.RegisterWithSources(customerID, profile, nil)
}

// RegisterWithSources — 신규 customer 등록 또는 기존 entry 갱신.
//
// 같은 customerID 의 재등록 (예: ws 재연결 + 다른 Profile/sources) 시 갱신.
// count 는 신규 등록일 때만 증가. srcCount 는 sources 유무 변화에 맞춰 조정.
// sources 비어있으면 BEST 만 (기존 동작).
func (r *CustomerRegistry) RegisterWithSources(customerID string, profile session.Profile, sources []string) {
	if customerID == "" {
		return
	}
	var set map[string]struct{}
	if len(sources) > 0 {
		set = make(map[string]struct{}, len(sources))
		for _, s := range sources {
			if s != "" {
				set[s] = struct{}{}
			}
		}
	}
	reg := customerReg{profile: profile, sources: set}
	prev, loaded := r.entries.Swap(customerID, reg)
	if !loaded {
		r.count.Add(1)
		if len(set) > 0 {
			r.srcCount.Add(1)
		}
		return
	}
	// 기존 존재 — srcCount 를 이전/현재 sources 유무 차이만큼 조정.
	prevHad := len(prev.(customerReg).sources) > 0
	nowHas := len(set) > 0
	if prevHad && !nowHas {
		r.srcCount.Add(-1)
	} else if !prevHad && nowHas {
		r.srcCount.Add(1)
	}
}

// Unregister — 등록 해제. 미등록 customerID 호출은 no-op.
func (r *CustomerRegistry) Unregister(customerID string) {
	if customerID == "" {
		return
	}
	if prev, loaded := r.entries.LoadAndDelete(customerID); loaded {
		r.count.Add(-1)
		if len(prev.(customerReg).sources) > 0 {
			r.srcCount.Add(-1)
		}
	}
}

// Count — 현재 등록된 customer 수. PricingConsumer 가 fast path 분기에 사용.
func (r *CustomerRegistry) Count() int {
	return int(r.count.Load())
}

// SourceSubscriberCount — sources(비어있지 않은) 를 가진 customer 수.
// per-source pricing 경로의 fast-path gate — 0 이면 raw per-source tick 처리 skip.
func (r *CustomerRegistry) SourceSubscriberCount() int {
	return int(r.srcCount.Load())
}

// Lookup — 단일 customerID 의 Profile 조회. /v1/customers/{id} 운영 진단용.
// not registered 면 ok=false.
func (r *CustomerRegistry) Lookup(customerID string) (session.Profile, bool) {
	if customerID == "" {
		return session.Profile{}, false
	}
	v, ok := r.entries.Load(customerID)
	if !ok {
		return session.Profile{}, false
	}
	return v.(customerReg).profile, true
}

// CustomerEntry — Snapshot 결과의 단위. publisher 가 (customerID, profile) 쌍
// 으로 ApplyForCustomer 호출.
type CustomerEntry struct {
	CustomerID string
	Profile    session.Profile
}

// Snapshot — 현재 등록된 모든 entry 의 복사본. hot path (OnTick) 에서 매 tick
// 호출되므로 가급적 작은 슬라이스 유지가 운영 가정 (수천 이하). Count() 로
// pre-allocate.
//
// 반환 슬라이스는 호출자 소유 — 자유롭게 수정 가능.
func (r *CustomerRegistry) Snapshot() []CustomerEntry {
	out := make([]CustomerEntry, 0, r.Count())
	r.entries.Range(func(k, v any) bool {
		out = append(out, CustomerEntry{
			CustomerID: k.(string),
			Profile:    v.(customerReg).profile,
		})
		return true
	})
	return out
}

// Range — Snapshot 없이 in-place 순회. 호출자가 callback 에서 false 반환 시 종료.
// publisher 가 슬라이스 alloc 없이 처리하고 싶을 때. (BEST 경로 — profile 만 필요)
func (r *CustomerRegistry) Range(fn func(customerID string, profile session.Profile) bool) {
	r.entries.Range(func(k, v any) bool {
		return fn(k.(string), v.(customerReg).profile)
	})
}

// RangeForSource — 지정 source 를 구독한 customer 만 순회 (per-source pricing 경로).
// sources 가 비어있는(BEST-only) customer 는 건너뛴다. false 반환 시 종료.
func (r *CustomerRegistry) RangeForSource(source string, fn func(customerID string, profile session.Profile) bool) {
	if source == "" {
		return
	}
	r.entries.Range(func(k, v any) bool {
		reg := v.(customerReg)
		if _, ok := reg.sources[source]; !ok {
			return true
		}
		return fn(k.(string), reg.profile)
	})
}
