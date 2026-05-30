package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisRefresh(t *testing.T) (*RedisRefreshStore, *miniredis.Miniredis, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := NewRedisRefreshStore(rdb, RedisRefreshStoreOptions{Prefix: "test"})
	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}
	return store, mr, cleanup
}

func TestRedisRefresh_PutGetConsume_SingleUse(t *testing.T) {
	store, _, cleanup := newTestRedisRefresh(t)
	defer cleanup()

	rt := &RefreshToken{
		Token:     "tok-1",
		SID:       "sid-1",
		Usid:      "trader01",
		Channel:   "WEB",
		IssuedAt:  time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Put(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	got, err := store.Consume(context.Background(), "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "tok-1" || got.Usid != "trader01" || got.Channel != "WEB" {
		t.Errorf("got=%+v", got)
	}
	// 같은 token 두 번째 Consume — single-use 후 not found
	if _, err := store.Consume(context.Background(), "tok-1"); !errors.Is(err, ErrRefreshNotFound) {
		t.Errorf("2nd Consume err=%v, want ErrRefreshNotFound", err)
	}
}

func TestRedisRefresh_Unknown_NotFound(t *testing.T) {
	store, _, cleanup := newTestRedisRefresh(t)
	defer cleanup()
	if _, err := store.Consume(context.Background(), "missing"); !errors.Is(err, ErrRefreshNotFound) {
		t.Errorf("unknown token err=%v, want ErrRefreshNotFound", err)
	}
}

func TestRedisRefresh_TTLExpiration(t *testing.T) {
	store, mr, cleanup := newTestRedisRefresh(t)
	defer cleanup()
	rt := &RefreshToken{
		Token: "tok-expire", SID: "sid", Usid: "u",
		Channel: "WEB", ExpiresAt: time.Now().Add(10 * time.Second),
	}
	if err := store.Put(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	// miniredis 시간 fast-forward — TTL 초과
	mr.FastForward(15 * time.Second)
	if _, err := store.Consume(context.Background(), "tok-expire"); !errors.Is(err, ErrRefreshNotFound) {
		t.Errorf("expired token: err=%v, want ErrRefreshNotFound", err)
	}
}

func TestRedisRefresh_DeleteBySID_RemovesAll(t *testing.T) {
	store, _, cleanup := newTestRedisRefresh(t)
	defer cleanup()
	ctx := context.Background()
	// 같은 SID 에 2 token (rotation 도중 race)
	for _, tok := range []string{"tA", "tB"} {
		_ = store.Put(ctx, &RefreshToken{
			Token: tok, SID: "S1", Usid: "u", Channel: "WEB",
			ExpiresAt: time.Now().Add(time.Hour),
		})
	}
	// 다른 SID 의 token — 영향 받으면 안 됨
	_ = store.Put(ctx, &RefreshToken{
		Token: "tC", SID: "S2", Usid: "u", Channel: "WEB",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	n, err := store.DeleteBySID(ctx, "S1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("DeleteBySID=%d, want 2", n)
	}
	// 두 token 다 Consume 불가
	for _, tok := range []string{"tA", "tB"} {
		if _, err := store.Consume(ctx, tok); !errors.Is(err, ErrRefreshNotFound) {
			t.Errorf("DeleteBySID 후 %q 가 살아있음", tok)
		}
	}
	// 다른 SID 의 token 은 여전히 가용
	if _, err := store.Consume(ctx, "tC"); err != nil {
		t.Errorf("다른 SID 의 token 이 영향 받음: %v", err)
	}
}

func TestRedisRefresh_PutEmptyToken_Error(t *testing.T) {
	store, _, cleanup := newTestRedisRefresh(t)
	defer cleanup()
	err := store.Put(context.Background(), &RefreshToken{SID: "S", ExpiresAt: time.Now().Add(time.Hour)})
	if err == nil {
		t.Errorf("빈 token 인데 nil err")
	}
}

// MemoryRefreshStore 와 동일 인터페이스 — RefreshStore 만족 확인.
func TestRedisRefresh_SatisfiesInterface(t *testing.T) {
	var _ RefreshStore = (*RedisRefreshStore)(nil)
}
