package quoteid

import (
	"context"
	"sync"
	"time"
)

// MemoryRegistry 는 in-process Registry.
//
// dev / 단위 테스트 / 단일 인스턴스 mci-price 시나리오. lazy expiry —
// Get 시점에 ValidUntil + grace 가 지났으면 ErrNotFound + 즉시 evict.
// 별도 cleanup goroutine 은 운영용 RedisRegistry 에 위임 (TTL).
//
// 메모리 leak 방지를 위해 주기적으로 Sweep() 을 호출하면 일괄 정리 가능.
type MemoryRegistry struct {
	mu      sync.RWMutex
	records map[QuoteID]Record
	now     func() time.Time
	grace   time.Duration
}

// NewMemoryRegistry — grace 는 ValidUntil 이후에도 등록을 유지하는 시간.
// 매칭 엔진의 last-look hold-time + 네트워크 지연을 포함해야 한다.
// 0 이면 grace 없이 ValidUntil 도래 즉시 만료.
func NewMemoryRegistry(grace time.Duration) *MemoryRegistry {
	return &MemoryRegistry{
		records: make(map[QuoteID]Record),
		now:     time.Now,
		grace:   grace,
	}
}

// SetNow — 테스트용 시간 주입.
func (m *MemoryRegistry) SetNow(f func() time.Time) {
	if f != nil {
		m.now = f
	}
}

func (m *MemoryRegistry) Put(_ context.Context, rec Record) error {
	if rec.ValidUntil <= rec.IssuedAt {
		return ErrInvalidRecord
	}
	if rec.QuoteID == "" {
		return ErrInvalidRecord
	}
	m.mu.Lock()
	m.records[rec.QuoteID] = rec
	m.mu.Unlock()
	return nil
}

func (m *MemoryRegistry) Get(_ context.Context, id QuoteID) (Record, error) {
	m.mu.RLock()
	rec, ok := m.records[id]
	m.mu.RUnlock()
	if !ok {
		return Record{}, ErrNotFound
	}
	expireAt := time.Unix(0, rec.ValidUntil).Add(m.grace)
	if m.now().After(expireAt) {
		m.mu.Lock()
		delete(m.records, id)
		m.mu.Unlock()
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// Sweep — 만료된 record 일괄 제거. 운영 인스턴스가 주기적으로 호출.
// 반환값은 제거된 개수.
func (m *MemoryRegistry) Sweep() int {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, rec := range m.records {
		expireAt := time.Unix(0, rec.ValidUntil).Add(m.grace)
		if now.After(expireAt) {
			delete(m.records, id)
			n++
		}
	}
	return n
}

// Len — 현재 보관 중인 record 수 (만료 포함, sweep 전 카운트). 메트릭/디버깅용.
func (m *MemoryRegistry) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.records)
}
