package push

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/winwaysystems/wtg/internal/api/middleware"
)

// HandlerDeps 는 mci-push 의 HTTP 핸들러가 공유하는 의존성.
type HandlerDeps struct {
	Registry   *Registry
	Dispatcher *Dispatcher // /v1/push-stats 가 카운터 노출에 사용. nil 허용.
	Logger     *slog.Logger

	SendQueueSize int
	PingInterval  time.Duration
	PongTimeout   time.Duration

	// CheckOrigin 은 Origin 헤더 검증. nil 이면 same-origin 만 허용 (gorilla 기본).
	// 운영 환경에서는 도메인 화이트리스트 함수 주입.
	CheckOrigin func(*http.Request) bool

	// StartedAt — push-stats 의 uptime 계산용. 비어있으면 노출 안 됨.
	StartedAt time.Time
}

// newUpgrader 는 HTTP → WebSocket upgrade 를 수행하는 gorilla upgrader.
func newUpgrader(deps *HandlerDeps) *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     deps.CheckOrigin,
	}
}

// SubscribeHandler 는 GET /v1/subscribe — WebSocket 핸드셰이크 + Registry 등록.
//
// 흐름:
//  1. middleware/auth 의 Principal 추출 (DevMode: X-WTG-User, 운영: JWT)
//  2. websocket.Upgrade
//  3. Connection 생성 → Registry.Add
//  4. Connection 의 read/write goroutine 이 lifecycle 관리
//  5. 종료 시 onClose 콜백으로 Registry.Remove
func SubscribeHandler(deps *HandlerDeps) http.HandlerFunc {
	upgrader := newUpgrader(deps)
	return func(w http.ResponseWriter, r *http.Request) {
		p := middleware.PrincipalFromContext(r.Context())
		if p == nil || p.Usid == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "인증되지 않은 요청")
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// upgrader.Upgrade 가 이미 응답을 작성했으므로 추가 응답 X.
			deps.Logger.WarnContext(r.Context(), "ws upgrade 실패",
				slog.String("usid", p.Usid),
				slog.Any("error", err),
			)
			return
		}

		conn := NewConnection(ws, ConnectionOptions{
			LogonID:       p.Usid,
			Channel:       p.Channel,
			SendQueueSize: deps.SendQueueSize,
			PingInterval:  deps.PingInterval,
			PongTimeout:   deps.PongTimeout,
			Logger:        deps.Logger,
			OnClose: func(c *Connection) {
				deps.Registry.Remove(c)
			},
		})
		deps.Registry.Add(conn)

		// 핸들러는 여기서 즉시 반환 — Connection goroutine 이 lifecycle 관리.
	}
}

// StatsResponse 는 운영 모니터링용 응답 포맷.
//
// connections / users 는 ws Registry 의 실시간 게이지.
// dispatcher 의 카운터는 broker → ws fan-out 단의 누적:
//   - received       : broker 에서 받은 unsolicited 총수
//   - delivered      : ws Send 까지 도달한 fan-out 합
//   - dropped        : sent=0 인 fan-out 총합 (drop_* 사유 4 종 합)
//   - drop_unsupp    : Func 가 FCCast/FCPush/FCSignal 아님
//   - drop_envelope  : envelope JSON marshal 실패
//   - drop_unknown_user: LogonID 명시 됐는데 conn 없음
//   - drop_no_broadcast: LogonID 빈값 + 등록 conn 0
//   - send_failed    : fan-out 안 일부 conn send 실패 (slow / closed)
//
// uptime 은 mci-push 부팅 후 경과 시간 (초).
type StatsResponse struct {
	Connections           int    `json:"connections"`
	Users                 int    `json:"users"`
	UptimeSec             int64  `json:"uptime_sec,omitempty"`
	DispatcherReceived    uint64 `json:"dispatcher_received,omitempty"`
	DispatcherDeliver     uint64 `json:"dispatcher_delivered,omitempty"`
	DispatcherDropped     uint64 `json:"dispatcher_dropped,omitempty"`
	DropUnsupp            uint64 `json:"dispatcher_drop_unsupp,omitempty"`
	DropEnvelope          uint64 `json:"dispatcher_drop_envelope,omitempty"`
	DropUnknownUser       uint64 `json:"dispatcher_drop_unknown_user,omitempty"`
	DropNoBroadcast       uint64 `json:"dispatcher_drop_no_broadcast,omitempty"`
	DispatcherSendFailed  uint64 `json:"dispatcher_send_failed,omitempty"`
}

// StatsHandler 는 GET /v1/push-stats — registry + dispatcher 모니터링 endpoint.
func StatsHandler(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := &StatsResponse{
			Connections: deps.Registry.Count(),
			Users:       deps.Registry.UserCount(),
		}
		if !deps.StartedAt.IsZero() {
			resp.UptimeSec = int64(time.Since(deps.StartedAt).Seconds())
		}
		if deps.Dispatcher != nil {
			s := deps.Dispatcher.Stats()
			resp.DispatcherReceived = s.Received
			resp.DispatcherDeliver = s.Delivered
			resp.DispatcherDropped = s.Dropped
			resp.DropUnsupp = s.DropUnsupp
			resp.DropEnvelope = s.DropEnvelope
			resp.DropUnknownUser = s.DropUnknownUser
			resp.DropNoBroadcast = s.DropNoBroadcast
			resp.DispatcherSendFailed = s.SendFailed
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// PingHandler 는 GET /v1/ping — 인증 우회 헬스체크.
func PingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-push",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// helpers.

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": msg,
	})
}
