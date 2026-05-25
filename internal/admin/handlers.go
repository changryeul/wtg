package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

// Caller 는 mymq.Client 의 동기 RPC 추상화 (테스트 mock 가능).
type Caller interface {
	Call(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error)
}

// HandlerDeps 는 admin 핸들러 공유 의존성.
type HandlerDeps struct {
	MQ          Caller
	CallTimeout time.Duration
	Logger      *slog.Logger
}

// AdminCmdRequest 는 generic /v1/admin/cmd 본문.
//
// subc 는 mymq.SubGet* (150~160) 또는 SubCtl* 등 admin 명령 코드.
// data 는 명령별 페이로드 (대부분 비어있거나 짧은 키).
type AdminCmdRequest struct {
	Subc uint8           `json:"subc"`
	Data json.RawMessage `json:"data,omitempty"`
}

// AdminCmdResponse — broker 응답 envelope.
type AdminCmdResponse struct {
	Errn uint32          `json:"errn,omitempty"`
	Errm string          `json:"errm,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// AdminCmd 는 generic admin command 핸들러.
//
// 매매 엔진의 transaction 과 달리 admin 명령은 broker 자체에 도달 (Dirf=DirIoctl,
// Func=FCAdmin). 따라서 navigation 자동 채움 X — applyDefaults 가 origin 만 채우게
// 빈 Xchg/Rkey 로 호출하면 broker 가 admin 으로 처리.
func AdminCmd(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AdminCmdRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.Subc == 0 {
			writeJSONError(w, http.StatusBadRequest, "validation", "subc 필요")
			return
		}
		body := []byte(req.Data)
		reply, err := callAdmin(r.Context(), r, deps, mymq.Subc(req.Subc), body)
		if err != nil {
			writeBrokerError(w, deps.Logger, r, err)
			return
		}
		writeJSON(w, http.StatusOK, replyToEnvelope(reply))
	}
}

// shortcut endpoint들은 모두 admin_*.go 파일로 이전됨 (placeholder body +
// binary 응답 디코드 패턴 적용).
//   - GetStatus    → admin_status.go
//   - GetExchanges → admin_exchanges.go
//   - GetClients   → admin_clients.go
//   - GetUsers     → admin_users.go
//   - GetWhois     → admin_whois.go (broker 시멘틱 — xchg/rkey/qnam 검색)

// callAdmin 은 admin 명령 RPC 의 공통 코드.
//
// FrameInput.Xchg/Rkey 비워두면 Client.applyDefaults 의 자동 navi 채움이
// 활성화되지 않아 broker 가 admin 명령으로 처리한다 (Dirf=DirIoctl).
//
// SessionMode 인증을 통과한 요청이면 Principal.Cookie 가 들어와 있으므로
// admin 명령에도 자동 첨부 — broker/엔진 측 admin 권한 확인용.
func callAdmin(ctx context.Context, r *http.Request, deps *HandlerDeps, subc mymq.Subc, body []byte) (*mymq.Reply, error) {
	callCtx, cancel := context.WithTimeout(ctx, deps.CallTimeout)
	defer cancel()

	in := &mymq.FrameInput{
		Func: mymq.FCAdmin,
		Subc: subc,
		Dirf: mymq.DirIoctl,
		Keyc: mymq.KeySend,
		Body: body,
	}
	if p := middleware.PrincipalFromContext(r.Context()); p != nil && p.Cookie != nil {
		in.Cookie = p.Cookie
	}
	return deps.MQ.Call(callCtx, in)
}

// replyToEnvelope 는 mymq.Reply → AdminCmdResponse.
func replyToEnvelope(r *mymq.Reply) *AdminCmdResponse {
	if r == nil {
		return &AdminCmdResponse{}
	}
	env := &AdminCmdResponse{Errn: r.Errn, Errm: r.ErrMsg}
	if len(r.Body) > 0 {
		if json.Valid(r.Body) {
			env.Data = json.RawMessage(r.Body)
		} else {
			b, _ := json.Marshal(string(r.Body))
			env.Data = b
		}
	}
	return env
}

// writeBrokerError 는 broker call 실패 → HTTP status 매핑.
func writeBrokerError(w http.ResponseWriter, logger *slog.Logger, r *http.Request, err error) {
	if logger != nil {
		logger.WarnContext(r.Context(), "broker admin Call 실패",
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
	}
	switch {
	case errors.Is(err, mymq.ErrTimeoutErr), errors.Is(err, context.DeadlineExceeded):
		writeJSONError(w, http.StatusGatewayTimeout, "timeout", err.Error())
	case errors.Is(err, mymq.ErrReconnecting):
		writeJSONError(w, http.StatusServiceUnavailable, "reconnecting", "broker 재연결 중")
	case errors.Is(err, mymq.ErrClientClosed):
		writeJSONError(w, http.StatusServiceUnavailable, "broker_unavailable", "broker 연결 종료")
	default:
		var mqErr *mymq.Error
		if errors.As(err, &mqErr) {
			writeJSON(w, http.StatusUnprocessableEntity, &AdminCmdResponse{
				Errn: mqErr.Errn,
				Errm: mqErr.Msg,
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// 응답 헬퍼.

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

// PingHandler — 헬스체크.
func PingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"service": "mci-admin",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}
