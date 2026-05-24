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
	mu       sync.RWMutex
	records  map[QuoteID]Record
	consumed map[QuoteID]string // QuoteID → consumer_id (먼저 MarkConsumed 한 자)
	now      func() time.Time
	grace    time.Duration
}

// NewMemoryRegistry — grace 는 ValidUntil 이후에도 등록을 유지하는 시간.
// 매칭 엔진의 last-look hold-time + 네트워크 지연을 포함해야 한다.
// 0 이면 grace 없이 ValidUntil 도래 즉시 만료.
func NewMemoryRegistry(grace time.Duration) *MemoryRegistry {
	return &MemoryRegistry{
		records:  make(map[QuoteID]Record),
		consumed: make(map[QuoteID]string),
		now:      time.Now,
		grace:    grace,
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

// MarkConsumed — atomic 표시. mutex 가 race 차단.
func (m *MemoryRegistry) MarkConsumed(_ context.Context, id QuoteID, consumerID string) (ConsumeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.records[id]
	if !ok {
		return ConsumeResult{Status: ConsumeNotFound}, nil
	}
	expireAt := time.Unix(0, rec.ValidUntil).Add(m.grace)
	if m.now().After(expireAt) {
		// grace 도 지났음 — record 는 곧 GC 대상. NOT_FOUND 와 동치 처리.
		delete(m.records, id)
		delete(m.consumed, id)
		return ConsumeResult{Status: ConsumeNotFound}, nil
	}
	if !rec.ValidAt(m.now()) {
		// ValidUntil 도래 — grace 안이라 record 는 echo 가능.
		return ConsumeResult{Status: ConsumeExpired, Record: rec}, nil
	}
	if prev, taken := m.consumed[id]; taken {
		return ConsumeResult{
			Status:     ConsumeAlreadyDone,
			Record:     rec,
			ConsumedBy: prev,
		}, nil
	}
	m.consumed[id] = consumerID
	return ConsumeResult{Status: ConsumeOK, Record: rec}, nil
}

// MarkConsumedMany — 단일 mutex 안에서 순차 처리. batch 전체가 일관 snapshot.
func (m *MemoryRegistry) MarkConsumedMany(_ context.Context, reqs []ConsumeRequest) ([]ConsumeResult, error) {
	out := make([]ConsumeResult, len(reqs))
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for i, req := range reqs {
		rec, ok := m.records[req.QuoteID]
		if !ok {
			out[i] = ConsumeResult{Status: ConsumeNotFound}
			continue
		}
		expireAt := time.Unix(0, rec.ValidUntil).Add(m.grace)
		if now.After(expireAt) {
			delete(m.records, req.QuoteID)
			delete(m.consumed, req.QuoteID)
			out[i] = ConsumeResult{Status: ConsumeNotFound}
			continue
		}
		if !rec.ValidAt(now) {
			out[i] = ConsumeResult{Status: ConsumeExpired, Record: rec}
			continue
		}
		if prev, taken := m.consumed[req.QuoteID]; taken {
			out[i] = ConsumeResult{Status: ConsumeAlreadyDone, Record: rec, ConsumedBy: prev}
			continue
		}
		m.consumed[req.QuoteID] = req.ConsumerID
		out[i] = ConsumeResult{Status: ConsumeOK, Record: rec}
	}
	return out, nil
}

// Lookup — record + consumed 정보를 단일 mutex 안에서 일관 조회.
func (m *MemoryRegistry) Lookup(_ context.Context, id QuoteID) (LookupResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lookupLocked(id), nil
}

// LookupMany — Lookup 의 batch. 단일 RLock 안에서 N 항목 — batch 가 같은
// snapshot 시점.
func (m *MemoryRegistry) LookupMany(_ context.Context, ids []QuoteID) ([]LookupResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]LookupResult, len(ids))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, id := range ids {
		out[i] = m.lookupLocked(id)
	}
	return out, nil
}

// lookupLocked — mu 가 잡힌 상태에서 단일 id lookup. expiry 검사 포함.
func (m *MemoryRegistry) lookupLocked(id QuoteID) LookupResult {
	rec, ok := m.records[id]
	if !ok {
		// record 가 없어도 consumed marker 만 남았을 수 있음 — 검사.
		c, taken := m.consumed[id]
		return LookupResult{Found: false, Consumed: taken, ConsumedBy: c}
	}
	expireAt := time.Unix(0, rec.ValidUntil).Add(m.grace)
	if m.now().After(expireAt) {
		// 만료 — lazy evict 는 Sweep 에 양보 (RLock 안이라 write 불가).
		c, taken := m.consumed[id]
		return LookupResult{Found: false, Consumed: taken, ConsumedBy: c}
	}
	c, taken := m.consumed[id]
	return LookupResult{Found: true, Record: rec, Consumed: taken, ConsumedBy: c}
}

// Consumed — read-only 조회.
func (m *MemoryRegistry) Consumed(_ context.Context, id QuoteID) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.consumed[id]
	return c, ok, nil
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
			delete(m.consumed, id) // consumed 도 동반 정리.
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
