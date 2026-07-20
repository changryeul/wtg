package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

// LogoutRequest 는 옵셔널 입력. 비어 있어도 됨.
//
// exchange/routing_key 가 비면 ADMIN/LOGOFF 디폴트.
type LogoutRequest struct {
	Exchange   string          `json:"exchange,omitempty"`
	RoutingKey string          `json:"routing_key,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

const (
	defaultLogoutExchange   = "ADMIN"
	defaultLogoutRoutingKey = "LOGOFF"
)

// Logout 은 POST /v1/logout 핸들러.
//
// 흐름 (auth.md §5):
//
//  1. 인증 미들웨어 통과 — Principal.SessionID + Cookie 사용 가능 가정
//  2. broker LOGOFF 트랜잭션 호출 (cookie 첨부)
//  3. SessionStore 에서 세션 즉시 삭제
//  4. 200 OK
//
// LOGOFF 호출 실패해도 세션은 항상 삭제 — 클라이언트 입장에서 로그아웃은
// 멱등이고, 엔진 측 cookie 정리가 늦어져도 web 보안에는 영향 없다 (KILL 통보는
// 별도). errn 자체는 응답에 포함시켜 클라이언트가 알 수 있게 한다.
func Logout(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Sessions == nil {
			writeError(w, http.StatusServiceUnavailable, "no_session_store",
				"세션 저장소가 구성되지 않음")
			return
		}
		p, ok := principalRequired(w, r)
		if !ok {
			return
		}
		if p.SessionID == "" {
			// SessionMode 가 아닌 채로 logout 호출 — DevMode 에서는 무의미.
			writeError(w, http.StatusBadRequest, "no_session",
				"DevMode 또는 비-세션 인증으로는 logout 할 수 없음")
			return
		}

		var req LogoutRequest
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_json", err.Error())
				return
			}
		}
		exchange := req.Exchange
		if exchange == "" {
			exchange = defaultLogoutExchange
		}
		routingKey := req.RoutingKey
		if routingKey == "" {
			routingKey = defaultLogoutRoutingKey
		}

		// LOGOFF 트랜잭션 — cookie 첨부.
		var brokerErrn uint32
		var brokerErrm string

		// chain 모드 — 세션의 lgnIdntCon 을 W1130A03 으로 반납.
		// 실패해도 세션 삭제는 진행 (멱등 — 기존 LOGOFF semantics 와 동일).
		if deps.LoginChain != nil && p.SessionID != "" {
			if sess, err := deps.Sessions.Get(r.Context(), p.SessionID); err == nil && sess.LgnIdntCon != "" {
				_, err := callChainStep(r.Context(), deps, "logout",
					deps.LoginChain.logoutAlias(), sess.Usid,
					map[string]interface{}{"loip": clientIPOf(r)},
					map[string]interface{}{
						"prGb":       "1",
						"fxUserNo":   sess.Usid,
						"lgnIdntCon": sess.LgnIdntCon,
					})
				if err != nil {
					var stepErr *chainStepError
					if errors.As(err, &stepErr) {
						brokerErrn, brokerErrm = stepErr.Errn, stepErr.Errm
					}
					deps.Logger.WarnContext(r.Context(), "W1130A03 반납 실패 — 세션은 삭제 진행",
						slog.String("sid", p.SessionID),
						slog.Any("error", err),
					)
				}
			}
		}

		if p.Cookie != nil {
			frame := &mymq.FrameInput{
				Func:   mymq.FCTran,
				Subc:   mymq.SubTranMsg,
				Dirf:   mymq.DirForward,
				Keyc:   mymq.KeySend,
				Xchg:   exchange,
				Rkey:   routingKey,
				Body:   []byte(req.Data),
				Cookie: p.Cookie,
			}
			callCtx, cancel := context.WithTimeout(r.Context(), deps.CallTimeout)
			reply, err := deps.MQ.Call(callCtx, frame)
			cancel()
			if err != nil {
				// broker 호출 실패 — 로깅만 하고 세션 삭제는 계속.
				deps.Logger.WarnContext(r.Context(), "LOGOFF Call 실패 — 세션은 삭제 진행",
					slog.String("sid", p.SessionID),
					slog.Any("error", err),
				)
			} else if mqErr := reply.AsError(); mqErr != nil {
				brokerErrn = reply.Errn
				brokerErrm = reply.ErrMsg
				deps.Logger.WarnContext(r.Context(), "LOGOFF errn — 세션은 삭제 진행",
					slog.String("sid", p.SessionID),
					slog.Uint64("errn", uint64(reply.Errn)),
				)
			}
		}

		// 세션 삭제는 항상 시도.
		if err := deps.Sessions.Delete(r.Context(), p.SessionID); err != nil {
			deps.Logger.ErrorContext(r.Context(), "세션 삭제 실패",
				slog.String("sid", p.SessionID),
				slog.Any("error", err),
			)
			writeError(w, http.StatusInternalServerError, "session_store", err.Error())
			return
		}
		// refresh 토큰도 동시 무효화 — 동일 SID 의 모든 refresh 제거.
		if deps.RefreshStore != nil {
			if _, err := deps.RefreshStore.DeleteBySID(r.Context(), p.SessionID); err != nil {
				deps.Logger.WarnContext(r.Context(), "refresh 토큰 정리 실패",
					slog.String("sid", p.SessionID),
					slog.Any("error", err),
				)
			}
		}

		// audit log — 인증 행위는 access log 와 별도로 명시 (security 도메인).
		// 운영 SIEM 이 evt=auth.logout 로 필터 가능.
		deps.Logger.InfoContext(r.Context(), "로그아웃",
			slog.String("evt", "auth.logout"),
			slog.String("usid", p.Usid),
			slog.String("sid", p.SessionID),
			slog.String("remote", r.RemoteAddr),
			slog.String("ua", r.UserAgent()),
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			slog.Uint64("broker_errn", uint64(brokerErrn)),
		)

		resp := map[string]any{"ok": true}
		if brokerErrn != 0 {
			resp["broker_errn"] = brokerErrn
			resp["broker_errm"] = brokerErrm
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
