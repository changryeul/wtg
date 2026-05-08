package push

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// 사용자별 ws connection 매핑.
//
// edge-push 는 mci-push 의 Registry 와 동일한 패턴 — logon_id ↔ []*Connection.
// 별도 패키지로 두는 이유는:
//   - DMZ 가시성: 코드 review 시 어디 컴포넌트인지 명확
//   - 향후 edge 측 추가 책임(rate limit per user 등) 분리

var (
	ErrConnClosed    = errors.New("edge-push: connection 종료됨")
	ErrSendQueueFull = errors.New("edge-push: send queue 가득")
)

// Connection 은 단일 ws 클라이언트.
type Connection struct {
	id      uint64
	conn    *websocket.Conn
	logonID string
	channel string

	send   chan []byte
	closed atomic.Bool
	closeC chan struct{}
	logger *slog.Logger

	onClose func(*Connection)
}

var connIDSeq atomic.Uint64

// ConnectionOptions.
type ConnectionOptions struct {
	LogonID       string
	Channel       string
	SendQueueSize int
	Logger        *slog.Logger
	OnClose       func(*Connection)
}

// NewConnection 은 ws 핸드셰이크 직후 호출.
func NewConnection(ws *websocket.Conn, opts ConnectionOptions) *Connection {
	if opts.SendQueueSize <= 0 {
		opts.SendQueueSize = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	c := &Connection{
		id:      connIDSeq.Add(1),
		conn:    ws,
		logonID: opts.LogonID,
		channel: opts.Channel,
		send:    make(chan []byte, opts.SendQueueSize),
		closeC:  make(chan struct{}),
		onClose: opts.OnClose,
	}
	c.logger = opts.Logger.With(slog.Uint64("conn_id", c.id), slog.String("usid", c.logonID))
	return c
}

// LogonID 는 매핑된 사용자.
func (c *Connection) LogonID() string { return c.logonID }

// Send 는 send queue 에 enqueue.
func (c *Connection) Send(p []byte) error {
	if c.closed.Load() {
		return ErrConnClosed
	}
	select {
	case c.send <- p:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close 는 idempotent.
func (c *Connection) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	close(c.closeC)
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.onClose != nil {
		c.onClose(c)
	}
}

// IsClosed.
func (c *Connection) IsClosed() bool { return c.closed.Load() }

// Registry 는 logon_id → []*Connection 매핑.
type Registry struct {
	mu     sync.RWMutex
	byUsid map[string][]*Connection
	logger *slog.Logger
}

// NewRegistry.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		byUsid: make(map[string][]*Connection),
		logger: logger,
	}
}

// Add 는 신규 connection 등록 (logon_id 비어있으면 panic).
func (r *Registry) Add(c *Connection) {
	if c.logonID == "" {
		panic("edge-push: empty logon_id")
	}
	r.mu.Lock()
	r.byUsid[c.logonID] = append(r.byUsid[c.logonID], c)
	r.mu.Unlock()
}

// Remove 는 idempotent 제거.
func (r *Registry) Remove(c *Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byUsid[c.logonID]
	for i, cc := range list {
		if cc == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(r.byUsid, c.logonID)
	} else {
		r.byUsid[c.logonID] = list
	}
}

// FanoutToUser 는 단일 사용자의 모든 connection 에 메시지 전송.
// slow consumer 자동 격리.
func (r *Registry) FanoutToUser(usid string, payload []byte) (sent, failed int) {
	r.mu.RLock()
	conns := append([]*Connection(nil), r.byUsid[usid]...)
	r.mu.RUnlock()

	for _, c := range conns {
		err := c.Send(payload)
		if err == nil {
			sent++
			continue
		}
		failed++
		if errors.Is(err, ErrSendQueueFull) {
			r.logger.Warn("slow consumer 격리", slog.Uint64("conn_id", c.id))
			c.Close()
		}
	}
	return sent, failed
}

// FanoutBroadcast 는 모든 connection 에.
func (r *Registry) FanoutBroadcast(payload []byte) (sent, failed int) {
	r.mu.RLock()
	all := make([]*Connection, 0)
	for _, list := range r.byUsid {
		all = append(all, list...)
	}
	r.mu.RUnlock()
	for _, c := range all {
		if err := c.Send(payload); err != nil {
			failed++
			if errors.Is(err, ErrSendQueueFull) {
				c.Close()
			}
		} else {
			sent++
		}
	}
	return sent, failed
}

// Count, UserCount.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, list := range r.byUsid {
		total += len(list)
	}
	return total
}

func (r *Registry) UserCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byUsid)
}

// CloseAll.
func (r *Registry) CloseAll() {
	r.mu.RLock()
	all := make([]*Connection, 0)
	for _, list := range r.byUsid {
		all = append(all, list...)
	}
	r.mu.RUnlock()
	for _, c := range all {
		c.Close()
	}
}
