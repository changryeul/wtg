package quoteid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*RedisRegistry, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	reg := NewRedisRegistry(rdb, RedisRegistryOptions{Prefix: "test:qid"})
	return reg, mr
}

func TestRedisRegistry_PutGet(t *testing.T) {
	reg, _ := newTestRedis(t)
	ctx := context.Background()

	now := time.Now()
	rec := mkRec("A-1", now, time.Second)
	if err := reg.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := reg.Get(ctx, rec.QuoteID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.QuoteID != rec.QuoteID || got.Bid != rec.Bid {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, rec)
	}
	if got.Profile.Key() != rec.Profile.Key() {
		t.Errorf("Profile.Key: got %s, want %s", got.Profile.Key(), rec.Profile.Key())
	}
}

func TestRedisRegistry_NotFound(t *testing.T) {
	reg, _ := newTestRedis(t)
	_, err := reg.Get(context.Background(), "A-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotFound 기대, got %v", err)
	}
}

func TestRedisRegistry_TTLExpiry(t *testing.T) {
	reg, mr := newTestRedis(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	reg.now = func() time.Time { return now }

	rec := mkRec("A-1", now, 500*time.Millisecond)
	if err := reg.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := reg.Get(ctx, rec.QuoteID); err != nil {
		t.Fatalf("Put 직후 Get 실패: %v", err)
	}

	// miniredis 시간 진행 — TTL 초과.
	mr.FastForward(600 * time.Millisecond)
	if _, err := reg.Get(ctx, rec.QuoteID); !errors.Is(err, ErrNotFound) {
		t.Errorf("TTL 만료 후 ErrNotFound 기대, got %v", err)
	}
}

func TestRedisRegistry_GraceExtendsTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Unix(1700000000, 0)
	reg := NewRedisRegistry(rdb, RedisRegistryOptions{
		Prefix: "test:qid",
		Grace:  300 * time.Millisecond,
		Now:    func() time.Time { return now },
	})

	rec := mkRec("A-1", now, 200*time.Millisecond)
	if err := reg.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// ValidUntil (200ms) 지났지만 grace (+300ms) 안.
	mr.FastForward(400 * time.Millisecond)
	if _, err := reg.Get(context.Background(), rec.QuoteID); err != nil {
		t.Errorf("grace 안 Get 실패: %v", err)
	}
	// grace 도 지남 (총 500ms = 200 + 300).
	mr.FastForward(200 * time.Millisecond)
	if _, err := reg.Get(context.Background(), rec.QuoteID); !errors.Is(err, ErrNotFound) {
		t.Errorf("grace 후 ErrNotFound 기대, got %v", err)
	}
}

func TestRedisRegistry_AlreadyExpiredSkipsWrite(t *testing.T) {
	reg, mr := newTestRedis(t)
	now := time.Unix(1700000000, 0)
	reg.now = func() time.Time { return now }

	// ValidUntil 이 이미 과거 (grace=0).
	rec := mkRec("A-stale", now.Add(-time.Second), 100*time.Millisecond)
	if err := reg.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Redis 에 쓰지 않았으므로 키 없음.
	if mr.Exists("test:qid:q:A-stale") {
		t.Error("이미 만료된 record 는 Redis 에 쓰면 안 됨")
	}
	if _, err := reg.Get(context.Background(), "A-stale"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotFound 기대, got %v", err)
	}
}

func TestRedisRegistry_MarkConsumed_FirstWins(t *testing.T) {
	reg, _ := newTestRedis(t)
	ctx := context.Background()
	now := time.Now()

	rec := mkRec("A-1", now, time.Hour)
	if err := reg.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	r1, err := reg.MarkConsumed(ctx, "A-1", "order-X")
	if err != nil {
		t.Fatalf("MarkConsumed first: %v", err)
	}
	if r1.Status != ConsumeOK {
		t.Errorf("first: %v, want ConsumeOK", r1.Status)
	}

	r2, _ := reg.MarkConsumed(ctx, "A-1", "order-Y")
	if r2.Status != ConsumeAlreadyDone {
		t.Errorf("second: %v, want ConsumeAlreadyDone", r2.Status)
	}
	if r2.ConsumedBy != "order-X" {
		t.Errorf("ConsumedBy = %q, want order-X", r2.ConsumedBy)
	}
}

func TestRedisRegistry_MarkConsumed_NotFound(t *testing.T) {
	reg, _ := newTestRedis(t)
	r, _ := reg.MarkConsumed(context.Background(), "A-nope", "order-1")
	if r.Status != ConsumeNotFound {
		t.Errorf("status=%v, want ConsumeNotFound", r.Status)
	}
}

func TestRedisRegistry_MarkConsumed_Expired(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Unix(1700000000, 0)
	reg := NewRedisRegistry(rdb, RedisRegistryOptions{
		Prefix: "test:qid",
		Grace:  time.Hour, // grace 크게 — record 유지.
		Now:    func() time.Time { return now },
	})

	_ = reg.Put(context.Background(), mkRec("A-1", now, 500*time.Millisecond))

	// ValidUntil 도래 — 하지만 grace 안이라 record 는 살아있음.
	mr.FastForward(2 * time.Second)
	advanced := now.Add(2 * time.Second)
	reg.now = func() time.Time { return advanced }

	r, _ := reg.MarkConsumed(context.Background(), "A-1", "order-1")
	if r.Status != ConsumeExpired {
		t.Errorf("status=%v, want ConsumeExpired", r.Status)
	}
}

func TestRedisRegistry_Consumed(t *testing.T) {
	reg, _ := newTestRedis(t)
	ctx := context.Background()
	rec := mkRec("A-1", time.Now(), time.Hour)
	_ = reg.Put(ctx, rec)

	// 표시 전.
	if _, ok, _ := reg.Consumed(ctx, "A-1"); ok {
		t.Error("MarkConsumed 호출 전인데 Consumed=true")
	}

	_, _ = reg.MarkConsumed(ctx, "A-1", "order-X")

	who, ok, err := reg.Consumed(ctx, "A-1")
	if err != nil {
		t.Fatalf("Consumed: %v", err)
	}
	if !ok || who != "order-X" {
		t.Errorf("Consumed: who=%q ok=%v", who, ok)
	}
}

func TestRedisRegistry_PutInvalid(t *testing.T) {
	reg, _ := newTestRedis(t)
	now := time.Unix(1700000000, 0)

	rec := Record{QuoteID: "A-1", IssuedAt: now.UnixNano(), ValidUntil: now.UnixNano()}
	if err := reg.Put(context.Background(), rec); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("ValidUntil=IssuedAt 거부 기대, got %v", err)
	}
	rec2 := mkRec("", now, time.Second)
	if err := reg.Put(context.Background(), rec2); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("빈 QuoteID 거부 기대, got %v", err)
	}
}
