package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/session"
)

func newTestRedis(t *testing.T) (*RedisStore, *miniredis.Miniredis, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := NewRedisStore(rdb, RedisStoreOptions{Prefix: "test"})
	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}
	return store, mr, cleanup
}

func mkCookie() *mymq.Cookie {
	c := &mymq.Cookie{Clid: 42}
	copy(c.Usid[:], []byte("CRLEE"))
	copy(c.Name[:], []byte("이충래"))
	copy(c.Pcip[:], []byte("10.0.0.1"))
	for i := range c.Coki {
		c.Coki[i] = byte(i)
	}
	return c
}

func mkSessionForRedis() *Session {
	return &Session{
		ID:        "sid-abc",
		Usid:      "CRLEE",
		Channel:   "WEB",
		Cookie:    mkCookie(),
		ExpiresAt: time.Now().Add(time.Hour),
		Profile: session.Profile{
			Channel: session.ChannelWeb,
			Site:    session.SiteBranch,
			Tier:    session.TierVIP,
		},
		LogonID: session.LogonID("logon-1"),
	}
}

func TestRedisStore_PutGet_RoundTrip(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	want := mkSessionForRedis()
	if err := store.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != want.ID || got.Usid != want.Usid || got.Channel != want.Channel {
		t.Errorf("기본 필드 round-trip 실패: got=%+v", got)
	}
	if got.Profile != want.Profile {
		t.Errorf("Profile mismatch: got=%+v want=%+v", got.Profile, want.Profile)
	}
	if got.LogonID != want.LogonID {
		t.Errorf("LogonID mismatch: got=%q want=%q", got.LogonID, want.LogonID)
	}
	if got.Cookie == nil {
		t.Fatal("Cookie 가 nil 로 복원됨")
	}
	if got.Cookie.Clid != want.Cookie.Clid {
		t.Errorf("Cookie.Clid: got=%d want=%d", got.Cookie.Clid, want.Cookie.Clid)
	}
	if got.Cookie.Coki != want.Cookie.Coki {
		t.Errorf("Cookie.Coki round-trip mismatch")
	}
	if got.Cookie.Usid != want.Cookie.Usid {
		t.Errorf("Cookie.Usid round-trip mismatch")
	}
}

func TestRedisStore_SubscribedNotPersisted(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	s := mkSessionForRedis()
	s.Subscribe("USD/KRW")
	s.Subscribe("EUR/KRW")

	if err := store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 구독은 ws-local 이므로 복원 시 빈 상태.
	if got.IsSubscribed("USD/KRW") {
		t.Error("Subscribed 가 Redis 에서 복원됨 (영속화되면 안 됨)")
	}
	if len(got.Subscriptions()) != 0 {
		t.Errorf("Subscriptions 가 비어있어야 함: %v", got.Subscriptions())
	}
}

func TestRedisStore_GetMissing(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	_, err := store.Get(ctx, "does-not-exist")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get missing: err=%v, want ErrSessionNotFound", err)
	}
}

func TestRedisStore_Delete(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	s := mkSessionForRedis()
	_ = store.Put(ctx, s)

	if err := store.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}

	_, err := store.Get(ctx, s.ID)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Delete 후 Get: err=%v, want ErrSessionNotFound", err)
	}

	// 미존재 Delete 는 에러 아님.
	if err := store.Delete(ctx, "another-id"); err != nil {
		t.Errorf("미존재 Delete: %v", err)
	}
}

func TestRedisStore_PutAlreadyExpired(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	s := mkSessionForRedis()
	s.ExpiresAt = time.Now().Add(-time.Hour) // 과거
	err := store.Put(ctx, s)
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("이미 만료된 세션 Put: err=%v, want ErrSessionExpired", err)
	}
}

func TestRedisStore_GetExpired_AutoDelete(t *testing.T) {
	store, mr, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	// 1) clock 을 controlled time 으로 교체
	fakeNow := time.Now()
	store.now = func() time.Time { return fakeNow }

	s := mkSessionForRedis()
	s.ExpiresAt = fakeNow.Add(time.Hour)
	if err := store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}

	// 2) clock 을 90분 전진 → 만료
	fakeNow = fakeNow.Add(90 * time.Minute)
	// Redis TTL 도 같이 전진 (miniredis 의 fastForward).
	mr.FastForward(90 * time.Minute)

	_, err := store.Get(ctx, s.ID)
	if !errors.Is(err, ErrSessionNotFound) && !errors.Is(err, ErrSessionExpired) {
		t.Errorf("만료 세션 Get: err=%v, want ErrSessionExpired or NotFound", err)
	}
}

func TestRedisStore_GetUpdatesLastSeen(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	t0 := time.Now().Truncate(time.Second)
	store.now = func() time.Time { return t0 }

	s := mkSessionForRedis()
	s.ExpiresAt = t0.Add(time.Hour)
	if err := store.Put(ctx, s); err != nil {
		t.Fatal(err)
	}

	// 30초 후 Get
	store.now = func() time.Time { return t0.Add(30 * time.Second) }
	got, err := store.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantSeen := t0.Add(30 * time.Second)
	if !got.LastSeenAt.Equal(wantSeen) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, wantSeen)
	}

	// 만료시각 자체는 변경 안 됨.
	if !got.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("ExpiresAt 가 변경됨: %v", got.ExpiresAt)
	}
}

func TestRedisStore_PutInvalid(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()

	if err := store.Put(context.Background(), nil); err == nil {
		t.Error("nil 세션 Put 통과")
	}
	if err := store.Put(context.Background(), &Session{}); err == nil {
		t.Error("ID 비어있는 세션 Put 통과")
	}
}

func TestRedisStore_PrefixIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	a := NewRedisStore(rdb, RedisStoreOptions{Prefix: "ns-a"})
	b := NewRedisStore(rdb, RedisStoreOptions{Prefix: "ns-b"})

	ctx := context.Background()
	s := mkSessionForRedis()
	if err := a.Put(ctx, s); err != nil {
		t.Fatal(err)
	}
	// 같은 ID 라도 namespace 가 다르면 못 본다.
	if _, err := b.Get(ctx, s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("b.Get: err=%v, want ErrSessionNotFound (네임스페이스 격리 실패)", err)
	}
}

// Store 인터페이스 구현 컴파일 타임 보장.
var _ Store = (*RedisStore)(nil)

// chain 모드 세션 — Cookie 없이 LgnIdntCon/CifNo 만 보관하는 세션의 왕복.
func TestRedisStore_ChainSessionRoundTrip(t *testing.T) {
	store, _, cleanup := newTestRedis(t)
	defer cleanup()
	ctx := context.Background()

	in := &Session{
		ID:         "sid-chain-1",
		Usid:       "hong01",
		Channel:    "WEB",
		Cookie:     nil, // chain 모드 — cookie_t 없음
		LgnIdntCon: "202607201030|AA:BB|10.0.0.7|hong01",
		CifNo:      "1234567890",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := store.Put(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := store.Get(ctx, "sid-chain-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.LgnIdntCon != in.LgnIdntCon {
		t.Errorf("LgnIdntCon=%q, want %q", out.LgnIdntCon, in.LgnIdntCon)
	}
	if out.CifNo != in.CifNo {
		t.Errorf("CifNo=%q, want %q", out.CifNo, in.CifNo)
	}
	if out.Cookie != nil {
		t.Errorf("Cookie 는 nil 이어야 함")
	}
}
