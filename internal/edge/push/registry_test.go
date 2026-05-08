package push

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mock connection — ws 없이 send queue 만.
func mkTestConn(usid string, queueSize int) *Connection {
	return &Connection{
		id:      connIDSeq.Add(1),
		logonID: usid,
		send:    make(chan []byte, queueSize),
		closeC:  make(chan struct{}),
		logger:  discardLogger(),
	}
}

func TestConnectionSendOnClosed(t *testing.T) {
	c := mkTestConn("trader01", 4)
	c.closed.Store(true)
	if err := c.Send([]byte("x")); !errors.Is(err, ErrConnClosed) {
		t.Errorf("ErrConnClosed 기대, got %v", err)
	}
}

func TestConnectionSendQueueFull(t *testing.T) {
	c := mkTestConn("trader01", 1)
	if err := c.Send([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Send([]byte("b")); !errors.Is(err, ErrSendQueueFull) {
		t.Errorf("ErrSendQueueFull 기대, got %v", err)
	}
}

func TestConnectionCloseIdempotent(t *testing.T) {
	c := mkTestConn("trader01", 4)
	var calls atomic.Int32
	c.onClose = func(*Connection) { calls.Add(1) }
	c.Close()
	c.Close()
	if calls.Load() != 1 {
		t.Errorf("onClose 호출: %d, want 1", calls.Load())
	}
}

func TestRegistryAddRemoveCount(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	c3 := mkTestConn("trader01", 4) // 다중 접속

	r.Add(c1)
	r.Add(c2)
	r.Add(c3)

	if r.Count() != 3 {
		t.Errorf("Count: %d", r.Count())
	}
	if r.UserCount() != 2 {
		t.Errorf("UserCount: %d", r.UserCount())
	}

	r.Remove(c1)
	if r.UserCount() != 2 {
		t.Errorf("UserCount after remove c1: %d (c3 가 trader01 유지)", r.UserCount())
	}
	r.Remove(c3)
	if r.UserCount() != 1 {
		t.Errorf("UserCount after remove c3: %d (trader01 모두 사라짐)", r.UserCount())
	}
}

func TestRegistryAddPanicsOnEmptyUsid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("빈 logon_id 에 panic 기대")
		}
	}()
	r := NewRegistry(discardLogger())
	r.Add(mkTestConn("", 4))
}

func TestRegistryFanoutToUser(t *testing.T) {
	r := NewRegistry(discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader01", 4)
	c3 := mkTestConn("trader02", 4)
	r.Add(c1)
	r.Add(c2)
	r.Add(c3)

	sent, failed := r.FanoutToUser("trader01", []byte("x"))
	if sent != 2 || failed != 0 {
		t.Errorf("FanoutToUser: sent=%d failed=%d", sent, failed)
	}
	if len(c3.send) != 0 {
		t.Errorf("trader02 가 잘못 받음")
	}
}

func TestRegistryFanoutSlowConsumerCloses(t *testing.T) {
	r := NewRegistry(discardLogger())
	c := mkTestConn("trader01", 1)
	r.Add(c)

	r.FanoutToUser("trader01", []byte("a")) // OK
	r.FanoutToUser("trader01", []byte("b")) // Full → close

	if !c.IsClosed() {
		t.Error("slow consumer close 안 됨")
	}
}

func TestRegistryFanoutBroadcast(t *testing.T) {
	r := NewRegistry(discardLogger())
	r.Add(mkTestConn("trader01", 4))
	r.Add(mkTestConn("trader02", 4))
	r.Add(mkTestConn("trader03", 4))

	sent, _ := r.FanoutBroadcast([]byte("alert"))
	if sent != 3 {
		t.Errorf("broadcast: sent=%d", sent)
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
		t.Error("CloseAll 후 모두 close")
	}
}
