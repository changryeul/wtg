package idempotency

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func hash(s string) [32]byte {
	return sha256.Sum256([]byte(s))
}

func TestMemoryStore_ReserveMissFirstTime(t *testing.T) {
	s := NewMemoryStore(Options{})
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

func TestMemoryStore_CachedAfterCommit(t *testing.T) {
	s := NewMemoryStore(Options{TTL: 100 * time.Millisecond})
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	reply := &CachedReply{StatusCode: 200, Body: []byte(`{"ok":true}`)}
	if err := s.Commit(context.Background(), "k1", reply); err != nil {
		t.Fatalf("commit err=%v", err)
	}
	st, cached, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusCached {
		t.Errorf("status=%v, want Cached", st)
	}
	if cached == nil || cached.StatusCode != 200 {
		t.Errorf("cached=%+v, want StatusCode=200", cached)
	}
}

func TestMemoryStore_InFlightSameBody(t *testing.T) {
	s := NewMemoryStore(Options{})
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	// commit 없이 두 번째 reserve.
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusInFlight {
		t.Errorf("status=%v, want InFlight", st)
	}
}

func TestMemoryStore_ConflictDifferentBody(t *testing.T) {
	s := NewMemoryStore(Options{})
	_, _, _ = s.Reserve(context.Background(), "k1", hash("body-A"))
	st, _, _ := s.Reserve(context.Background(), "k1", hash("body-B"))
	if st != StatusConflict {
		t.Errorf("status=%v, want Conflict", st)
	}
}

func TestMemoryStore_RollbackReleasesInFlight(t *testing.T) {
	s := NewMemoryStore(Options{})
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	if err := s.Rollback(context.Background(), "k1"); err != nil {
		t.Fatalf("rollback err=%v", err)
	}
	// 재시도 — Miss 다시.
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusMiss {
		t.Errorf("rollback 후 status=%v, want Miss", st)
	}
}

func TestMemoryStore_RollbackKeepsCommitted(t *testing.T) {
	// Commit 후 Rollback 은 캐시 보존 (운영 안전성 — rollback 호출이 잘못 와도).
	s := NewMemoryStore(Options{})
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	_ = s.Commit(context.Background(), "k1", &CachedReply{StatusCode: 200, Body: []byte(`x`)})
	_ = s.Rollback(context.Background(), "k1")
	st, cached, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusCached || cached == nil {
		t.Errorf("committed reply 가 rollback 으로 사라짐: status=%v cached=%v", st, cached)
	}
}

func TestMemoryStore_TTLExpiresEntry(t *testing.T) {
	s := NewMemoryStore(Options{TTL: 30 * time.Millisecond})
	h := hash("body")
	_, _, _ = s.Reserve(context.Background(), "k1", h)
	_ = s.Commit(context.Background(), "k1", &CachedReply{StatusCode: 200, Body: []byte(`x`)})
	time.Sleep(60 * time.Millisecond)
	st, _, _ := s.Reserve(context.Background(), "k1", h)
	if st != StatusMiss {
		t.Errorf("TTL 후 status=%v, want Miss (expired)", st)
	}
}

func TestMemoryStore_MakeKeyIsolatesUsers(t *testing.T) {
	s := NewMemoryStore(Options{})
	hA := hash("body-A")
	hB := hash("body-B")
	// 다른 user 의 같은 header 는 충돌 안 함.
	stA, _, _ := s.Reserve(context.Background(), MakeKey("alice", "ID-1"), hA)
	stB, _, _ := s.Reserve(context.Background(), MakeKey("bob", "ID-1"), hB)
	if stA != StatusMiss || stB != StatusMiss {
		t.Errorf("user 분리 실패: A=%v B=%v", stA, stB)
	}
	if s.Size() != 2 {
		t.Errorf("size=%d, want 2", s.Size())
	}
}
