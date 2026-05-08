package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

func mkSession(id, usid string, ttl time.Duration) *Session {
	now := time.Now()
	return &Session{
		ID:        id,
		Usid:      usid,
		Channel:   "WEB",
		Cookie:    &mymq.Cookie{Clid: 0xCAFE},
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
	}
}

func TestMemoryStorePutGetDelete(t *testing.T) {
	s := NewMemoryStore(MemoryStoreOptions{SweepInterval: time.Hour})
	defer s.Close()

	ctx := context.Background()
	sess := mkSession("s1", "trader01", 10*time.Minute)
	if err := s.Put(ctx, sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Usid != "trader01" || got.Cookie.Clid != 0xCAFE {
		t.Errorf("Get 결과 불일치: %+v", got)
	}

	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "s1"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("삭제 후 Get: want ErrSessionNotFound, got %v", err)
	}
}

func TestMemoryStoreNotFound(t *testing.T) {
	s := NewMemoryStore(MemoryStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("미존재: want ErrSessionNotFound, got %v", err)
	}
}

func TestMemoryStoreExpiredOnGet(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	s := NewMemoryStore(MemoryStoreOptions{SweepInterval: time.Hour, Now: now})
	defer s.Close()
	ctx := context.Background()

	sess := &Session{ID: "s1", Usid: "u", ExpiresAt: now().Add(5 * time.Minute)}
	if err := s.Put(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// 시간 진행 → 만료.
	clock.Add(int64(10 * 60))

	if _, err := s.Get(ctx, "s1"); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("만료된 Get: want ErrSessionExpired, got %v", err)
	}
	// 만료 시 자동 삭제 확인.
	if s.Len() != 0 {
		t.Errorf("만료된 세션이 자동 삭제되지 않음: len=%d", s.Len())
	}
}

func TestMemoryStoreSweeper(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	s := NewMemoryStore(MemoryStoreOptions{
		SweepInterval: 10 * time.Millisecond,
		Now:           now,
	})
	defer s.Close()
	ctx := context.Background()

	for i, ttl := range []time.Duration{1 * time.Minute, 1 * time.Hour} {
		id := []string{"short", "long"}[i]
		s.Put(ctx, &Session{ID: id, Usid: "u", ExpiresAt: now().Add(ttl)})
	}
	if s.Len() != 2 {
		t.Fatalf("초기 len=%d, want 2", s.Len())
	}

	// short 만 만료시킴.
	clock.Add(int64(5 * 60))

	// sweeper 가 동작할 시간 부여.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && s.Len() > 1 {
		time.Sleep(15 * time.Millisecond)
	}
	if s.Len() != 1 {
		t.Errorf("sweeper 후 len=%d, want 1", s.Len())
	}
}

func TestMemoryStoreSlidingLastSeen(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	s := NewMemoryStore(MemoryStoreOptions{SweepInterval: time.Hour, Now: now})
	defer s.Close()
	ctx := context.Background()

	s.Put(ctx, &Session{ID: "s1", Usid: "u", ExpiresAt: now().Add(time.Hour)})
	first, _ := s.Get(ctx, "s1")
	firstSeen := first.LastSeenAt

	clock.Add(30)
	second, _ := s.Get(ctx, "s1")
	if !second.LastSeenAt.After(firstSeen) {
		t.Errorf("LastSeenAt 갱신되지 않음: first=%v second=%v", firstSeen, second.LastSeenAt)
	}
}

func TestMemoryStoreConcurrent(t *testing.T) {
	s := NewMemoryStore(MemoryStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "s" + string(rune('A'+i%26))
			s.Put(ctx, &Session{ID: id, Usid: "u", ExpiresAt: time.Now().Add(time.Hour)})
			s.Get(ctx, id)
			s.Delete(ctx, id)
		}(i)
	}
	wg.Wait()
}

func TestNewSessionIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Errorf("중복 ID: %s", id)
		}
		seen[id] = struct{}{}
		if len(id) < 32 {
			t.Errorf("ID 너무 짧음: %s (len=%d)", id, len(id))
		}
	}
}

func TestSessionExpired(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"미래", now.Add(time.Hour), false},
		{"과거", now.Add(-time.Hour), true},
		{"제로(만료없음)", time.Time{}, false},
		{"동일시각", now, false}, // After 가 false
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{ExpiresAt: tc.expiresAt}
			if got := s.Expired(now); got != tc.want {
				t.Errorf("Expired=%v, want %v", got, tc.want)
			}
		})
	}
}
