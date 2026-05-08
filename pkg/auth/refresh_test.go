package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkRefresh(token, sid string, ttl time.Duration) *RefreshToken {
	now := time.Now()
	return &RefreshToken{
		Token: token, SID: sid, Usid: "u", Channel: "WEB",
		IssuedAt: now, ExpiresAt: now.Add(ttl),
	}
}

func TestMemoryRefreshStorePutConsume(t *testing.T) {
	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	ctx := context.Background()

	if err := s.Put(ctx, mkRefresh("rt-1", "sid-1", 8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Consume(ctx, "rt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SID != "sid-1" {
		t.Errorf("SID: %q", got.SID)
	}
	// Consume 후 단일-사용 — 다시 조회하면 미존재.
	if _, err := s.Consume(ctx, "rt-1"); !errors.Is(err, ErrRefreshNotFound) {
		t.Errorf("재사용 가능: %v", err)
	}
}

func TestMemoryRefreshStoreNotFound(t *testing.T) {
	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	if _, err := s.Consume(context.Background(), "nope"); !errors.Is(err, ErrRefreshNotFound) {
		t.Errorf("err=%v", err)
	}
}

func TestMemoryRefreshStoreExpired(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{SweepInterval: time.Hour, Now: now})
	defer s.Close()
	ctx := context.Background()

	s.Put(ctx, &RefreshToken{Token: "rt", SID: "s", ExpiresAt: now().Add(time.Minute)})
	clock.Add(120) // 2분 경과 → 만료

	if _, err := s.Consume(ctx, "rt"); !errors.Is(err, ErrRefreshExpired) {
		t.Errorf("err=%v, want ErrRefreshExpired", err)
	}
}

func TestMemoryRefreshStoreDeleteBySID(t *testing.T) {
	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	ctx := context.Background()

	s.Put(ctx, mkRefresh("rt-a", "sid-1", time.Hour))
	s.Put(ctx, mkRefresh("rt-b", "sid-1", time.Hour))
	s.Put(ctx, mkRefresh("rt-c", "sid-2", time.Hour))

	n, err := s.DeleteBySID(ctx, "sid-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("삭제 개수=%d, want 2", n)
	}
	if s.Len() != 1 {
		t.Errorf("나머지 개수=%d, want 1", s.Len())
	}
	// sid-2 는 남아있어야.
	if _, err := s.Consume(ctx, "rt-c"); err != nil {
		t.Errorf("sid-2 의 토큰: %v", err)
	}
}

func TestMemoryRefreshStoreSweeper(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{
		SweepInterval: 10 * time.Millisecond, Now: now,
	})
	defer s.Close()
	ctx := context.Background()

	s.Put(ctx, &RefreshToken{Token: "short", SID: "s", ExpiresAt: now().Add(time.Minute)})
	s.Put(ctx, &RefreshToken{Token: "long", SID: "s", ExpiresAt: now().Add(time.Hour)})

	clock.Add(int64(5 * 60)) // 5분 경과 → short 만 만료
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && s.Len() > 1 {
		time.Sleep(15 * time.Millisecond)
	}
	if s.Len() != 1 {
		t.Errorf("sweeper 후 len=%d, want 1", s.Len())
	}
}

func TestMemoryRefreshStoreConcurrent(t *testing.T) {
	s := NewMemoryRefreshStore(MemoryRefreshStoreOptions{SweepInterval: time.Hour})
	defer s.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, _ := NewRefreshTokenString()
			s.Put(ctx, &RefreshToken{Token: tok, SID: "s", ExpiresAt: time.Now().Add(time.Hour)})
			s.Consume(ctx, tok)
		}(i)
	}
	wg.Wait()
}

func TestNewRefreshTokenStringUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		t1, err := NewRefreshTokenString()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[t1]; dup {
			t.Errorf("중복: %s", t1)
		}
		seen[t1] = struct{}{}
		if len(t1) < 40 {
			t.Errorf("너무 짧음: %d", len(t1))
		}
	}
}
