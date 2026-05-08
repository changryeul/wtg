package push

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 연결 단위 에러.
var (
	ErrConnClosed    = errors.New("push: ws connection 종료됨")
	ErrSendQueueFull = errors.New("push: send queue 가득 — slow consumer")
)

// Connection 은 단일 WebSocket 클라이언트의 lifecycle 을 관리한다.
//
// 책임:
//   - 송신 큐 (chan []byte) — Dispatcher 가 무차별 push 해도 backpressure 격리
//   - 별도 writer goroutine 이 send queue 를 ws.Conn 에 직렬 송신
//   - ping 주기적 송신 + pong watchdog
//   - 큐 가득 시 slow consumer 로 간주하고 끊음 (다른 사용자 영향 차단)
//   - registry 에서 자동 정리 (Close 시)
type Connection struct {
	id      uint64 // 디버깅용 식별자
	conn    *websocket.Conn
	logonID string
	channel string

	send   chan []byte
	closed atomic.Bool
	closeC chan struct{}

	pingInterval time.Duration
	pongTimeout  time.Duration

	logger *slog.Logger

	// onClose 는 Connection 이 종료될 때 호출 (registry 정리용).
	onClose func(*Connection)
}

// ConnectionOptions 는 Connection 생성 시 주입되는 의존성.
type ConnectionOptions struct {
	LogonID       string
	Channel       string
	SendQueueSize int
	PingInterval  time.Duration
	PongTimeout   time.Duration
	Logger        *slog.Logger
	OnClose       func(*Connection)
}

var connIDSeq atomic.Uint64

// NewConnection 은 Connection 을 구성하고 read/write goroutine 을 가동한다.
// 호출자는 ws upgrade 직후 즉시 호출하면 된다.
func NewConnection(ws *websocket.Conn, opts ConnectionOptions) *Connection {
	if opts.SendQueueSize <= 0 {
		opts.SendQueueSize = 256
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = 30 * time.Second
	}
	if opts.PongTimeout <= 0 {
		opts.PongTimeout = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	c := &Connection{
		id:           connIDSeq.Add(1),
		conn:         ws,
		logonID:      opts.LogonID,
		channel:      opts.Channel,
		send:         make(chan []byte, opts.SendQueueSize),
		closeC:       make(chan struct{}),
		pingInterval: opts.PingInterval,
		pongTimeout:  opts.PongTimeout,
		onClose:      opts.OnClose,
	}
	c.logger = opts.Logger.With(slog.Uint64("conn_id", c.id), slog.String("usid", c.logonID))

	// pong handler — 클라이언트의 pong 으로 deadline 갱신.
	c.conn.SetReadDeadline(time.Now().Add(c.pongTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(c.pongTimeout))
		return nil
	})

	go c.writeLoop()
	go c.readLoop()
	return c
}

// LogonID 는 이 connection 이 매핑된 사용자 ID.
func (c *Connection) LogonID() string { return c.logonID }

// Send 는 단일 메시지를 send queue 에 enqueue 한다.
// 큐가 가득 차면 ErrSendQueueFull 반환 — 호출자 (dispatcher) 가 결정:
// drop, close, alert 등.
func (c *Connection) Send(payload []byte) error {
	if c.closed.Load() {
		return ErrConnClosed
	}
	select {
	case c.send <- payload:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close 는 Connection 을 정리한다 (idempotent).
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

// IsClosed 는 외부에서 상태 조회용.
func (c *Connection) IsClosed() bool {
	return c.closed.Load()
}

// writeLoop 은 send queue 를 ws.Conn 에 직렬 송신하면서 주기적으로 ping 도 보낸다.
func (c *Connection) writeLoop() {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()
	defer c.Close()

	for {
		select {
		case <-c.closeC:
			return
		case payload, ok := <-c.send:
			if !ok {
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.logger.Debug("ws write 실패 — 연결 종료", slog.Any("error", err))
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Debug("ws ping 실패 — 연결 종료", slog.Any("error", err))
				return
			}
		}
	}
}

// readLoop 은 클라이언트 측 메시지를 소비한다.
//
// mci-push 는 본질적으로 server→client 단방향 push 서비스이므로 클라이언트
// 메시지는 무시한다. 단, websocket pong / close 프레임 처리를 위해 읽기는
// 계속해야 한다 (gorilla/websocket 는 ReadMessage 안에서 pong/close 처리).
func (c *Connection) readLoop() {
	defer c.Close()
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// 외부 helper — 서버 종료 시 일괄 close 등에 사용.
func (c *Connection) Drain(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-c.closeC:
	}
}
