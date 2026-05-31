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
	// entries: customerID(string) → session.Profile
	entries sync.Map

	// count: sync.Map 크기를 정확히 추적 — Snapshot pre-allocate 용.
	// Register/Unregister 가 LoadOrStore / LoadAndDelete 결과에 따라 증감.
	count atomic.Int64
}

// NewCustomerRegistry 는 빈 registry 를 반환한다.
func NewCustomerRegistry() *CustomerRegistry {
	return &CustomerRegistry{}
}

// Register — 신규 customer 등록 또는 기존 entry 갱신.
//
// 같은 customerID 의 재등록 (예: ws 재연결 + 다른 Profile) 시 새 Profile 로
// 갱신. count 는 신규 등록일 때만 증가.
func (r *CustomerRegistry) Register(customerID string, profile session.Profile) {
	if customerID == "" {
		return
	}
	if _, loaded := r.entries.LoadOrStore(customerID, profile); !loaded {
		r.count.Add(1)
		return
	}
	// 이미 존재 — Profile 갱신.
	r.entries.Store(customerID, profile)
}

// Unregister — 등록 해제. 미등록 customerID 호출은 no-op.
func (r *CustomerRegistry) Unregister(customerID string) {
	if customerID == "" {
		return
	}
	if _, loaded := r.entries.LoadAndDelete(customerID); loaded {
		r.count.Add(-1)
	}
}

// Count — 현재 등록된 customer 수. PricingConsumer 가 fast path 분기에 사용.
func (r *CustomerRegistry) Count() int {
	return int(r.count.Load())
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
			Profile:    v.(session.Profile),
		})
		return true
	})
	return out
}

// Range — Snapshot 없이 in-place 순회. 호출자가 callback 에서 false 반환 시 종료.
// publisher 가 슬라이스 alloc 없이 처리하고 싶을 때.
func (r *CustomerRegistry) Range(fn func(customerID string, profile session.Profile) bool) {
	r.entries.Range(func(k, v any) bool {
		return fn(k.(string), v.(session.Profile))
	})
}
