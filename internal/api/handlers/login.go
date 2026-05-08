package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

// LoginRequest 는 POST /v1/login 입력.
//
// auth.md §3 — id/pw/TOTP 는 매매 엔진이 검증. WTG 는 raw 그대로 LOGON
// 트랜잭션 페이로드에 실어 보낸다 (passthrough 패턴 일관성).
//
// exchange/routing_key 가 비어 있으면 운영 디폴트 ("ADMIN"/"LOGON") 사용.
// 매매 엔진의 LOGON 트랜잭션 코드가 다르면 클라이언트가 명시.
type LoginRequest struct {
	Exchange   string          `json:"exchange,omitempty"`
	RoutingKey string          `json:"routing_key,omitempty"`
	Channel    string          `json:"channel,omitempty"` // 세션 메타. 빈 값이면 "WEB".
	Data       json.RawMessage `json:"data,omitempty"`    // 엔진이 정의한 LOGON 페이로드 (id/pw/totp 등)
}

// LoginResponse 는 POST /v1/login 출력.
//
// JWTIssuer 가 구성되었으면 access_token (auth.md §6 access JWT) 이 채워지고,
// RefreshStore 도 있으면 refresh_token 도 함께. 두 가지가 모두 nil 이면
// 1차 호환 모드 — session_id 가 다음 요청의 Bearer 토큰.
type LoginResponse struct {
	SessionID    string          `json:"session_id"`
	AccessToken  string          `json:"access_token,omitempty"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	AccessExpAt  *time.Time      `json:"access_expires_at,omitempty"`
	RefreshExpAt *time.Time      `json:"refresh_expires_at,omitempty"`
	ExpiresAt    time.Time       `json:"expires_at"` // 세션(=cookie) 만료
	Channel      string          `json:"channel,omitempty"`
	Data         json.RawMessage `json:"data,omitempty"`
}

const (
	defaultLoginExchange   = "ADMIN"
	defaultLoginRoutingKey = "LOGON"
	defaultSessionTTL      = 8 * time.Hour
	defaultAccessTTL       = 15 * time.Minute
	defaultRefreshTTL      = 8 * time.Hour
)

// Login 은 POST /v1/login 핸들러.
//
// 흐름 (auth.md §3):
//
//  1. 페이로드 디코딩 (이 핸들러는 인증 미들웨어를 우회한다 — 아직 세션이 없음)
//  2. broker LOGON 트랜잭션 호출 (cookie 첨부 없이)
//  3. reply.Cookie 추출 → Store 에 신규 세션 저장
//  4. session_id + reply body 응답
//
// reply.Cookie 가 nil 이면 매매 엔진이 LOGON 응답에 cookie_t 를 동봉하지
// 않은 것 — 401 로 거부.
func Login(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Sessions == nil {
			writeError(w, http.StatusServiceUnavailable, "no_session_store",
				"세션 저장소가 구성되지 않음 — 운영 인증 비활성")
			return
		}

		var req LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}

		exchange := req.Exchange
		if exchange == "" {
			exchange = defaultLoginExchange
		}
		routingKey := req.RoutingKey
		if routingKey == "" {
			routingKey = defaultLoginRoutingKey
		}
		channel := req.Channel
		if channel == "" {
			channel = deps.DefaultChannel
		}
		if channel == "" {
			channel = "WEB"
		}

		frame := &mymq.FrameInput{
			Func: mymq.FCTran,
			Subc: mymq.SubTranMsg,
			Dirf: mymq.DirForward,
			Keyc: mymq.KeySend,
			Xchg: exchange,
			Rkey: routingKey,
			Body: []byte(req.Data),
			// Cookie 미첨부 — LOGON 은 cookie 발급 트랜잭션.
		}

		callCtx, cancel := context.WithTimeout(r.Context(), deps.CallTimeout)
		defer cancel()
		reply, err := deps.MQ.Call(callCtx, frame)
		if err != nil {
			deps.Logger.WarnContext(r.Context(), "LOGON Call 실패",
				slog.String("rid", middleware.RequestIDFromContext(r.Context())),
				slog.Any("error", err),
			)
			status, code, msg := mapBrokerError(err)
			writeError(w, status, code, msg)
			return
		}
		if mqErr := reply.AsError(); mqErr != nil {
			// 매매 엔진이 LOGON 거부 (잘못된 비밀번호 등) — errn 그대로 노출.
			status, _, _ := mapBrokerError(mqErr)
			writeJSON(w, status, map[string]any{
				"error":   "login_failed",
				"errn":    reply.Errn,
				"errm":    reply.ErrMsg,
				"message": mqErr.Error(),
			})
			return
		}
		if reply.Cookie == nil {
			deps.Logger.WarnContext(r.Context(), "LOGON 응답에 cookie 없음 — 매매 엔진 설정 확인 필요",
				slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			)
			writeError(w, http.StatusBadGateway, "no_cookie",
				"엔진이 cookie 를 발급하지 않음")
			return
		}

		sid, err := auth.NewSessionID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "rng", err.Error())
			return
		}
		ttl := deps.SessionTTL
		if ttl <= 0 {
			ttl = defaultSessionTTL
		}
		now := time.Now()
		sess := &auth.Session{
			ID:        sid,
			Usid:      cookieUsid(reply.Cookie),
			Channel:   channel,
			Cookie:    reply.Cookie,
			IssuedAt:  now,
			ExpiresAt: now.Add(ttl),
		}
		if err := deps.Sessions.Put(r.Context(), sess); err != nil {
			deps.Logger.ErrorContext(r.Context(), "세션 저장 실패",
				slog.Any("error", err),
			)
			writeError(w, http.StatusInternalServerError, "session_store", err.Error())
			return
		}

		// auth.md §10 audit — 추후 audit emitter 통합 시 LOGIN_SUCCESS 기록.
		deps.Logger.InfoContext(r.Context(), "로그인 성공",
			slog.String("usid", sess.Usid),
			slog.String("sid", sid),
			slog.String("chan", channel),
		)

		var dataOut json.RawMessage
		if len(reply.Body) > 0 {
			if json.Valid(reply.Body) {
				dataOut = json.RawMessage(reply.Body)
			} else {
				if b, e := json.Marshal(string(reply.Body)); e == nil {
					dataOut = b
				}
			}
		}

		resp := LoginResponse{
			SessionID: sid,
			ExpiresAt: sess.ExpiresAt,
			Channel:   channel,
			Data:      dataOut,
		}

		// access JWT 발급 — Issuer 가 구성된 경우.
		if deps.JWTIssuer != nil {
			accessTTL := deps.AccessTokenTTL
			if accessTTL <= 0 {
				accessTTL = defaultAccessTTL
			}
			accessExp := now.Add(accessTTL)
			tok, err := deps.JWTIssuer.Sign(auth.Claims{
				SID:  sid,
				Usid: sess.Usid,
				Chan: channel,
				EXP:  accessExp.Unix(),
			})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "jwt_sign", err.Error())
				return
			}
			resp.AccessToken = tok
			resp.AccessExpAt = &accessExp
		}

		// refresh token 발급 — RefreshStore 가 구성된 경우.
		if deps.RefreshStore != nil {
			refreshTTL := deps.RefreshTokenTTL
			if refreshTTL <= 0 {
				refreshTTL = defaultRefreshTTL
			}
			refreshExp := now.Add(refreshTTL)
			rt, err := auth.NewRefreshTokenString()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "rng", err.Error())
				return
			}
			if err := deps.RefreshStore.Put(r.Context(), &auth.RefreshToken{
				Token:     rt,
				SID:       sid,
				Usid:      sess.Usid,
				Channel:   channel,
				IssuedAt:  now,
				ExpiresAt: refreshExp,
			}); err != nil {
				writeError(w, http.StatusInternalServerError, "refresh_store", err.Error())
				return
			}
			resp.RefreshToken = rt
			resp.RefreshExpAt = &refreshExp
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// cookieUsid 는 cookie.Usid (NUL 패딩된 [16]byte) 를 string 으로 trim.
func cookieUsid(c *mymq.Cookie) string {
	if c == nil {
		return ""
	}
	for i, b := range c.Usid {
		if b == 0 {
			return string(c.Usid[:i])
		}
	}
	return string(c.Usid[:])
}
