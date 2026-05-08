package auth

import (
	"context"
	"sync"
	"time"
)

// MemoryStore 는 sync.Map 기반의 in-memory Store 구현.
//
// 용도:
//   - 단위 테스트
//   - dev / 단일 인스턴스 환경
//
// 운영(다중 mci-api 인스턴스) 환경에서는 RedisStore 로 차환해야 한다 —
// 이 구현은 프로세스 경계를 넘어 세션을 공유하지 못한다.
//
// 만료 처리:
//   - Get 시점에 lazy 만료 (해당 세션만 즉시 정리)
//   - 백그라운드 sweeper 가 SweepInterval 간격으로 일괄 정리
type MemoryStore struct {
	now           func() time.Time
	sweepInterval time.Duration

	mu       sync.RWMutex
	sessions map[string]*Session

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// MemoryStoreOptions 는 MemoryStore 생성 옵션.
type MemoryStoreOptions struct {
	// SweepInterval 은 만료 sweep 주기. 0 이면 1분.
	SweepInterval time.Duration
	// Now 는 현재 시각 함수 — 테스트에서 시간 조작용. 0 이면 time.Now.
	Now func() time.Time
}

// NewMemoryStore 는 백그라운드 sweeper 를 시작한 MemoryStore 를 만든다.
// Close 호출 시 sweeper 가 종료된다.
func NewMemoryStore(opt MemoryStoreOptions) *MemoryStore {
	if opt.SweepInterval <= 0 {
		opt.SweepInterval = 1 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	s := &MemoryStore{
		now:           opt.Now,
		sweepInterval: opt.SweepInterval,
		sessions:      make(map[string]*Session),
		stopC:         make(chan struct{}),
		doneC:         make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

// Put 은 세션을 저장한다. IssuedAt/LastSeenAt 이 0 이면 자동 채움.
func (s *MemoryStore) Put(_ context.Context, sess *Session) error {
	if sess.IssuedAt.IsZero() {
		sess.IssuedAt = s.now()
	}
	sess.LastSeenAt = s.now()
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	return nil
}

// Get 은 세션 조회. 만료 세션은 즉시 삭제 + ErrSessionExpired.
func (s *MemoryStore) Get(_ context.Context, id string) (*Session, error) {
	now := s.now()

	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrSessionNotFound
	}
	if sess.Expired(now) {
		s.mu.Lock()
		// 다른 고루틴이 이미 갱신했을 수도 있어 다시 확인.
		if cur, ok := s.sessions[id]; ok && cur == sess {
			delete(s.sessions, id)
		}
		s.mu.Unlock()
		return nil, ErrSessionExpired
	}

	// 슬라이딩 — LastSeenAt 만 갱신. 만료시각 자체는 Put 시 결정.
	s.mu.Lock()
	if cur, ok := s.sessions[id]; ok {
		cur.LastSeenAt = now
	}
	s.mu.Unlock()
	return sess, nil
}

// Delete 는 세션을 즉시 제거. 미존재 무시.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	return nil
}

// Close 는 sweeper 를 종료한다 (idempotent).
func (s *MemoryStore) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopC)
		<-s.doneC
	})
	return nil
}

// Len 은 저장된 세션 수 (테스트/모니터링용).
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// sweepLoop 은 주기적으로 만료된 세션을 일괄 삭제한다.
func (s *MemoryStore) sweepLoop() {
	defer close(s.doneC)
	t := time.NewTicker(s.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopC:
			return
		case <-t.C:
			s.sweepOnce()
		}
	}
}

func (s *MemoryStore) sweepOnce() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.Expired(now) {
			delete(s.sessions, id)
		}
	}
}
