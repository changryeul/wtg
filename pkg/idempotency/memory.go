package idempotency

import (
	"context"
	"sync"
	"time"
)

// MemoryStore — 단일 인스턴스 / dev 용 메모리 backend. lazy expire (Reserve
// 시점에 만료 entry 정리). 운영 다중 인스턴스는 Redis backend 필요.
type MemoryStore struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]*memEntry
}

type memEntry struct {
	bodyHash [32]byte
	reply    *CachedReply // nil = in-flight, non-nil = committed
	expires  time.Time
}

// NewMemoryStore — Options 기반 생성.
func NewMemoryStore(opt Options) *MemoryStore {
	return &MemoryStore{
		ttl:     opt.effectiveTTL(),
		entries: make(map[string]*memEntry),
	}
}

// Reserve — key 상태 점검 후 신규는 in-flight 등록.
func (s *MemoryStore) Reserve(_ context.Context, key string, bodyHash [32]byte) (Status, *CachedReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	if e, ok := s.entries[key]; ok {
		if now.After(e.expires) {
			// lazy expire — 만료된 entry 는 없는 것으로 취급.
			delete(s.entries, key)
		} else if e.bodyHash != bodyHash {
			return StatusConflict, nil, nil
		} else if e.reply != nil {
			return StatusCached, e.reply, nil
		} else {
			return StatusInFlight, nil, nil
		}
	}

	s.entries[key] = &memEntry{
		bodyHash: bodyHash,
		expires:  now.Add(s.ttl),
	}
	return StatusMiss, nil, nil
}

// Commit — reservation 의 reply 채움. TTL 갱신 (Reserve 시점 → Commit 시점).
func (s *MemoryStore) Commit(_ context.Context, key string, reply *CachedReply) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		// reservation 없는데 Commit — race 또는 Rollback 후. 새 entry 로 저장.
		s.entries[key] = &memEntry{reply: reply, expires: time.Now().Add(s.ttl)}
		return nil
	}
	e.reply = reply
	e.expires = time.Now().Add(s.ttl)
	return nil
}

// Rollback — reservation 해제. broker 호출 실패 시 재시도 가능하게.
func (s *MemoryStore) Rollback(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && e.reply == nil {
		delete(s.entries, key)
	}
	return nil
}

// Close — memory store 는 cleanup 없음. interface 만족용.
func (s *MemoryStore) Close() error { return nil }

// Size — 현재 entry 수 (test / 디버그용).
func (s *MemoryStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
