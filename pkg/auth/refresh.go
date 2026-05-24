package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"sync"
	"time"
)

// RefreshToken 은 long-lived refresh token (auth.md §6).
//
// access JWT 가 짧게(15분) 유지되는 동안, refresh token 은 8h 정도 유지되어
// access 재발급 (rotation) 에 사용된다. 단일-사용 (one-time) 정책으로 발급될
// 때마다 새 token 을 반환하고 이전 것은 무효화 — replay 방어.
type RefreshToken struct {
	Token     string // 외부 노출용 불투명 식별자
	SID       string // 연관된 Session.ID (cookie_t 위치)
	Usid      string // 사용자 ID (감사용)
	Channel   string // "WEB" / "ADMIN"
	IssuedAt  time.Time
	ExpiresAt time.Time
}

func (r *RefreshToken) Expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt)
}

var (
	ErrRefreshNotFound = errors.New("auth: refresh token 미존재")
	ErrRefreshExpired  = errors.New("auth: refresh token 만료")
)

// RefreshStore 는 refresh token 저장소.
//
// 운영에서는 Redis-backed 구현으로 차환 (auth.md §7). 인터페이스 동일.
// 모든 메서드는 goroutine-safe.
type RefreshStore interface {
	Put(ctx context.Context, t *RefreshToken) error

	// Consume 는 token 을 조회하면서 동시에 삭제 — single-use rotation 정책.
	// 미존재 / 만료 시 각각 ErrRefreshNotFound / ErrRefreshExpired.
	Consume(ctx context.Context, token string) (*RefreshToken, error)

	// DeleteBySID 는 동일 SID 에 묶인 모든 refresh token 제거 (logout 시).
	DeleteBySID(ctx context.Context, sid string) (int, error)

	Close() error
}

// MemoryRefreshStore 는 sync.RWMutex 기반의 in-memory RefreshStore.
//
// 동일 SID 에 여러 refresh 가 동시 존재할 수 있다 (재발급 도중 race) — sweeper
// 가 만료된 것을 정리하므로 의도적으로 허용.
type MemoryRefreshStore struct {
	now           func() time.Time
	sweepInterval time.Duration

	mu     sync.Mutex
	tokens map[string]*RefreshToken

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// MemoryRefreshStoreOptions 는 MemoryRefreshStore 옵션.
type MemoryRefreshStoreOptions struct {
	SweepInterval time.Duration
	Now           func() time.Time
}

// NewMemoryRefreshStore — 백그라운드 sweeper 시작. Close 시 종료.
func NewMemoryRefreshStore(opt MemoryRefreshStoreOptions) *MemoryRefreshStore {
	if opt.SweepInterval <= 0 {
		opt.SweepInterval = 1 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	s := &MemoryRefreshStore{
		now:           opt.Now,
		sweepInterval: opt.SweepInterval,
		tokens:        make(map[string]*RefreshToken),
		stopC:         make(chan struct{}),
		doneC:         make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

func (s *MemoryRefreshStore) Put(_ context.Context, t *RefreshToken) error {
	if t == nil || t.Token == "" {
		return errors.New("auth: RefreshToken.Token 필수")
	}
	if t.IssuedAt.IsZero() {
		t.IssuedAt = s.now()
	}
	s.mu.Lock()
	s.tokens[t.Token] = t
	s.mu.Unlock()
	return nil
}

func (s *MemoryRefreshStore) Consume(_ context.Context, token string) (*RefreshToken, error) {
	now := s.now()
	s.mu.Lock()
	t, ok := s.tokens[token]
	if ok {
		delete(s.tokens, token)
	}
	s.mu.Unlock()
	if !ok {
		return nil, ErrRefreshNotFound
	}
	if t.Expired(now) {
		return nil, ErrRefreshExpired
	}
	return t, nil
}

func (s *MemoryRefreshStore) DeleteBySID(_ context.Context, sid string) (int, error) {
	if sid == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, t := range s.tokens {
		if t.SID == sid {
			delete(s.tokens, k)
			n++
		}
	}
	return n, nil
}

// Len 은 저장된 토큰 수 (테스트/모니터링용).
func (s *MemoryRefreshStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// Close 는 sweeper 종료 (idempotent).
func (s *MemoryRefreshStore) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopC)
		<-s.doneC
	})
	return nil
}

func (s *MemoryRefreshStore) sweepLoop() {
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

func (s *MemoryRefreshStore) sweepOnce() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.tokens {
		if t.Expired(now) {
			delete(s.tokens, k)
		}
	}
}

// NewRefreshTokenString 은 32-byte CSPRNG → base32 (52자) 토큰 문자열을 만든다.
// SessionID 보다 길게 — 외부 노출 빈도가 더 높고 single-use 라 충돌 영향이 큼.
func NewRefreshTokenString() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}
