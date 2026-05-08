package push

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

// fakeWriter 는 Connection 없이 Registry 동작만 검증하기 위한 mock.
// 실제 Connection 은 ws.Conn 의존이라 단위 테스트가 어렵다 → wrapper 로 대체.
//
// 단순화 위해 본 테스트에서는 *Connection 을 직접 만들되, ws/goroutine 시작은
// 우회하고 send queue 와 closed flag 만 검증한다.

func mkTestConn(usid string, queueSize int) *Connection {
	return &Connection{
		id:      connIDSeq.Add(1),
		logonID: usid,
		send:    make(chan []byte, queueSize),
		closeC:  make(chan struct{}),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRegistryAddAndCount(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	c3 := mkTestConn("trader01", 4) // 동일 사용자 다중 접속

	r.Add(c1)
	r.Add(c2)
	r.Add(c3)

	if got := r.Count(); got != 3 {
		t.Errorf("Count: %d, want 3", got)
	}
	if got := r.UserCount(); got != 2 {
		t.Errorf("UserCount: %d, want 2", got)
	}
}

func TestRegistryRemove(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader01", 4)
	c3 := mkTestConn("trader02", 4)

	r.Add(c1)
	r.Add(c2)
	r.Add(c3)
	r.Remove(c1)

	if r.Count() != 2 {
		t.Errorf("Count after remove: %d", r.Count())
	}
	if r.UserCount() != 2 {
		t.Errorf("UserCount: %d", r.UserCount())
	}

	// 같은 사용자의 마지막 connection 제거 → user 도 삭제.
	r.Remove(c2)
	if r.UserCount() != 1 {
		t.Errorf("UserCount after last removal: %d", r.UserCount())
	}
	if r.Count() != 1 {
		t.Errorf("Count: %d", r.Count())
	}
}

func TestRegistryAddPanicsOnEmptyUsid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("빈 logonID 에 대해 panic 기대")
		}
	}()
	r := NewRegistry(discardLogger())
	c := mkTestConn("", 4)
	r.Add(c)
}

func TestRegistryFanoutToUser(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader01", 4)
	c3 := mkTestConn("trader02", 4)
	r.Add(c1)
	r.Add(c2)
	r.Add(c3)

	sent, failed := r.FanoutToUser("trader01", []byte("hello"))
	if sent != 2 || failed != 0 {
		t.Errorf("FanoutToUser: sent=%d failed=%d", sent, failed)
	}
	// trader02 큐에는 메시지가 안 들어가 있어야 함.
	if len(c3.send) != 0 {
		t.Errorf("trader02 가 잘못 받음: %d", len(c3.send))
	}
	if len(c1.send) != 1 || len(c2.send) != 1 {
		t.Errorf("trader01 큐 상태: c1=%d c2=%d", len(c1.send), len(c2.send))
	}
}

func TestRegistryFanoutToUserMissing(t *testing.T) {
	r := NewRegistry(discardLogger())
	sent, failed := r.FanoutToUser("ghost", []byte("x"))
	if sent != 0 || failed != 0 {
		t.Errorf("미등록 사용자: sent=%d failed=%d", sent, failed)
	}
}

func TestRegistryFanoutSlowConsumerClose(t *testing.T) {
	r := NewRegistry(discardLogger())
	// queue size 1 — 즉시 가득 찰 수 있게.
	c := mkTestConn("trader01", 1)
	r.Add(c)

	// 1번 send → 큐에 들어감.
	r.FanoutToUser("trader01", []byte("a"))
	// 2번째 send → 큐 가득 → ErrSendQueueFull → 자동 Close 트리거.
	// 하지만 Close 가 onClose=nil 이라 panic? Close 자체는 idempotent + onClose nil-safe.
	r.FanoutToUser("trader01", []byte("b"))

	// Close 후 connection 은 closed=true.
	if !c.IsClosed() {
		t.Error("slow consumer 가 close 되지 않음")
	}
}

func TestRegistryFanoutBroadcast(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	c3 := mkTestConn("trader03", 4)
	r.Add(c1)
	r.Add(c2)
	r.Add(c3)

	sent, _ := r.FanoutBroadcast([]byte("alert"))
	if sent != 3 {
		t.Errorf("broadcast: sent=%d, want 3", sent)
	}
}

func TestRegistryConnsForUser(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader01", 4)
	r.Add(c1)
	r.Add(c2)

	got := r.ConnsForUser("trader01")
	if len(got) != 2 {
		t.Errorf("ConnsForUser: %d", len(got))
	}
	// snapshot 이라 외부 변경이 내부에 영향 없음.
	got[0] = nil
	again := r.ConnsForUser("trader01")
	if again[0] == nil {
		t.Error("ConnsForUser 가 internal slice 를 외부와 공유함 (snapshot 보장 깨짐)")
	}

	// 미등록 사용자.
	if len(r.ConnsForUser("ghost")) != 0 {
		t.Error("미등록 사용자에 대한 빈 slice 기대")
	}
}

func TestRegistryCloseAll(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	r.Add(c1)
	r.Add(c2)

	r.CloseAll()

	if !c1.IsClosed() || !c2.IsClosed() {
		t.Error("CloseAll 후 모든 connection 이 closed 여야 함")
	}
}

func TestConnectionSendOnClosed(t *testing.T) {
	c := mkTestConn("trader01", 4)
	c.closed.Store(true)
	if err := c.Send([]byte("x")); err != ErrConnClosed {
		t.Errorf("ErrConnClosed 기대, got %v", err)
	}
}

func TestConnectionSendQueueFull(t *testing.T) {
	c := mkTestConn("trader01", 1)
	if err := c.Send([]byte("a")); err != nil {
		t.Fatalf("첫 send: %v", err)
	}
	if err := c.Send([]byte("b")); err != ErrSendQueueFull {
		t.Errorf("ErrSendQueueFull 기대, got %v", err)
	}
}

func TestConnectionCloseIdempotent(t *testing.T) {
	c := mkTestConn("trader01", 4)
	var closeCount atomic.Int32
	c.onClose = func(*Connection) { closeCount.Add(1) }
	c.Close()
	c.Close() // 두 번째는 no-op
	if closeCount.Load() != 1 {
		t.Errorf("onClose 호출 수: %d, want 1", closeCount.Load())
	}
}
