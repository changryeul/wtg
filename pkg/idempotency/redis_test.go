package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisStore(t *testing.T, ttl time.Duration) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store, err := NewRedisStore(RedisOptions{Client: rdb, TTL: ttl})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(); _ = rdb.Close() })
	return store, mr
}

func TestRedisStore_ReserveMiss(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	st, cached, err := s.Reserve(context.Background(), "k1", hash("body"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if st != StatusMiss {
		t.Errorf("status=%v, want Miss", st)
	}
	if cached != nil {
		t.Errorf("cached=%+v, want nil", cached)
	}
}

func TestRedisStore_CommitThenCached(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	if err := s.Commit(context.Background(), "k1", &CachedReply{StatusCode: 200, Body: []byte(`{"ok":true}`)}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	st, cached, err := s.Reserve(context.Background(), "k1", h)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if st != StatusCached {
		t.Errorf("status=%v, want Cached", st)
	}
	if cached == nil || cached.StatusCode != 200 || string(cached.Body) != `{"ok":true}` {
		t.Errorf("cached=%+v", cached)
	}
}

func TestRedisStore_InFlight(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusInFlight {
		t.Errorf("status=%v, want InFlight", st)
	}
}

func TestRedisStore_Conflict(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	_, _, _ = s.Reserve(context.Background(), "k1", hash("A"))
	st, _, _ := s.Reserve(context.Background(), "k1", hash("B"))
	if st != StatusConflict {
		t.Errorf("status=%v, want Conflict", st)
	}
}

func TestRedisStore_RollbackReleasesInFlight(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	if err := s.Rollback(context.Background(), "k1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusMiss {
		t.Errorf("rollback 후 status=%v, want Miss", st)
	}
}

func TestRedisStore_RollbackKeepsCommitted(t *testing.T) {
	s, _ := newTestRedisStore(t, 0)
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	_ = s.Commit(context.Background(), "k1", &CachedReply{StatusCode: 200, Body: []byte(`x`)})
	_ = s.Rollback(context.Background(), "k1")
	st, cached, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusCached || cached == nil {
		t.Errorf("committed reply 가 rollback 으로 사라짐: status=%v cached=%v", st, cached)
	}
}

func TestRedisStore_TTLExpiresEntry(t *testing.T) {
	s, mr := newTestRedisStore(t, 30*time.Millisecond)
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	_ = s.Commit(context.Background(), "k1", &CachedReply{StatusCode: 200, Body: []byte(`x`)})
	// miniredis fast-forward — 실시간 sleep 회피.
	mr.FastForward(100 * time.Millisecond)
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusMiss {
		t.Errorf("TTL 후 status=%v, want Miss (expired)", st)
	}
}

func TestRedisStore_KeyPrefixSeparation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	a, _ := NewRedisStore(RedisOptions{Client: rdb, Prefix: "tenant-a:"})
	b, _ := NewRedisStore(RedisOptions{Client: rdb, Prefix: "tenant-b:"})

	_, _, _ = a.Reserve(context.Background(), "k1", hash("A"))
	// 다른 prefix 의 같은 key 는 독립 — Miss.
	st, _, _ := b.Reserve(context.Background(), "k1", hash("B"))
	if st != StatusMiss {
		t.Errorf("prefix 분리 실패: status=%v, want Miss", st)
	}
}
