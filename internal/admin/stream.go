package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/winwaysystems/wtg/internal/api/middleware"
)

// Event 는 ws 로 push 되는 단일 메시지.
//
// type 별 의미:
//   - "audit"  : 신규 admin 액션 (data 는 AuditEntry)
//   - "policy" : 정책 변경 (data 는 policy.State)
//   - "route"  : 라우팅 룰 변경 (data 는 {action: "put"|"delete", rule: ...})
//   - "ping"   : 서버 keep-alive (data 는 시각)
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
	At   int64  `json:"at"` // Unix milli
}

// Hub 는 ws 구독자 fan-out.
//
// 구독자별 buffered channel — slow consumer 는 채널이 가득 차면 끊는다 (web fan-out
// 표준 패턴). 정책 / audit 이벤트는 quasi-실시간이라 약간의 drop 은 허용.
type Hub struct {
	logger *slog.Logger

	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
	closed      bool
}

type subscriber struct {
	id    uint64
	usid  string
	out   chan Event
	close chan struct{}
}

// NewHub.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		logger:      logger,
		subscribers: make(map[*subscriber]struct{}),
	}
}

// Broadcast 는 모든 구독자에게 이벤트 전송 (non-blocking).
// 채널 가득 찬 구독자는 강제 종료.
func (h *Hub) Broadcast(typ string, data any) {
	ev := Event{Type: typ, Data: data, At: time.Now().UnixMilli()}
	h.mu.RLock()
	subs := make([]*subscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		subs = append(subs, s)
	}
	h.mu.RUnlock()

	for _, s := range subs {
		select {
		case s.out <- ev:
		default:
			h.logger.Warn("ws subscriber slow — 끊음", slog.Uint64("sub_id", s.id))
			h.removeAndClose(s)
		}
	}
}

// Count 는 활성 구독자 수.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Close — 모든 구독자 종료.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for s := range h.subscribers {
		close(s.close)
	}
	h.subscribers = nil
	h.mu.Unlock()
}

func (h *Hub) add(s *subscriber) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.subscribers[s] = struct{}{}
	return true
}

func (h *Hub) removeAndClose(s *subscriber) {
	h.mu.Lock()
	if _, ok := h.subscribers[s]; ok {
		delete(h.subscribers, s)
		close(s.close)
	}
	h.mu.Unlock()
}

var nextSubID uint64

// StreamHandler — GET /v1/admin/stream. 인증 미들웨어 통과 후 호출됨.
//
// 클라이언트는 ws 로 연결하면 hub 가 broadcast 하는 모든 이벤트를 받는다.
// 30s 마다 ping 이벤트 — keep-alive (브라우저가 ws 끊기지 않게).
func StreamHandler(hub *Hub, logger *slog.Logger) http.HandlerFunc {
	upgrader := &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 8192,
		CheckOrigin:     nil, // 운영 시 화이트리스트 함수 주입
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil || p.Usid == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Warn("ws upgrade 실패", slog.Any("error", err))
			return
		}
		nextSubID++
		s := &subscriber{
			id:    nextSubID,
			usid:  p.Usid,
			out:   make(chan Event, 64),
			close: make(chan struct{}),
		}
		if !hub.add(s) {
			ws.Close()
			return
		}
		logger.Info("ws stream 시작",
			slog.Uint64("sub_id", s.id),
			slog.String("usid", s.usid),
			slog.Int("subscribers", hub.Count()),
		)

		// 초기 hello 이벤트 — 클라이언트가 연결 확인.
		_ = wsSendJSON(ws, Event{
			Type: "hello",
			Data: map[string]any{"usid": s.usid, "subscribers": hub.Count()},
			At:   time.Now().UnixMilli(),
		})

		// write goroutine.
		go streamWrite(ws, s, hub, logger)
		// read goroutine — 클라이언트 메시지 무시 (ping/close 만 받음).
		go streamRead(ws, s, hub, logger)
	}
}

func streamWrite(ws *websocket.Conn, s *subscriber, hub *Hub, logger *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer ws.Close()
	defer hub.removeAndClose(s)

	for {
		select {
		case <-s.close:
			return
		case ev := <-s.out:
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := wsSendJSON(ws, ev); err != nil {
				return
			}
		case <-ticker.C:
			ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := wsSendJSON(ws, Event{
				Type: "ping", At: time.Now().UnixMilli(),
				Data: map[string]any{"subscribers": hub.Count()},
			}); err != nil {
				return
			}
		}
	}
}

func streamRead(ws *websocket.Conn, s *subscriber, hub *Hub, logger *slog.Logger) {
	defer hub.removeAndClose(s)
	ws.SetReadLimit(4 * 1024)
	ws.SetReadDeadline(time.Now().Add(2 * time.Minute))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(2 * time.Minute))
		return nil
	})
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
	}
}

func wsSendJSON(ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.TextMessage, b)
}
