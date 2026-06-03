package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
)

// RefreshRequest 는 POST /v1/refresh 본문.
//
// auth.md §6 — refresh token 으로 새 access JWT 발급. Single-use rotation:
// 사용된 refresh 는 즉시 무효화되고 새 refresh 가 함께 발급된다 (replay 방어).
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// RefreshResponse 는 새 access + refresh 페어.
type RefreshResponse struct {
	AccessToken  string    `json:"access_token"`
	AccessExpAt  time.Time `json:"access_expires_at"`
	RefreshToken string    `json:"refresh_token"`
	RefreshExpAt time.Time `json:"refresh_expires_at"`
	SID          string    `json:"sid,omitempty"`
}

// Refresh 는 POST /v1/refresh 핸들러.
//
// 흐름:
//
//  1. body 의 refresh_token 추출
//  2. RefreshStore.Consume — 단일-사용 (즉시 삭제)
//  3. SessionStore.Get(sid) — 세션이 살아있는지 확인 (logout / 만료 후 재사용 차단)
//  4. 새 refresh + access JWT 발급
//
// 인증 미들웨어를 통과할 필요가 없다 (자체 토큰을 들고 옴) — server.go 에서
// /v1/refresh 를 isPublicPath 우회하거나, 미들웨어 자체가 이 path 를 우회.
func Refresh(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.RefreshStore == nil || deps.JWTIssuer == nil || deps.Sessions == nil {
			writeError(w, http.StatusServiceUnavailable, "refresh_unavailable",
				"refresh 흐름 미구성")
			return
		}

		var req RefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if req.RefreshToken == "" {
			writeError(w, http.StatusBadRequest, "validation", "refresh_token 필요")
			return
		}

		consumeCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		old, err := deps.RefreshStore.Consume(consumeCtx, req.RefreshToken)
		cancel()
		if err != nil {
			if errors.Is(err, auth.ErrRefreshNotFound) || errors.Is(err, auth.ErrRefreshExpired) {
				// audit log — refresh 거부는 보안 도메인. 알 수 없거나 이미 사용된
				// 토큰 (replay 시도 포함). 동일 SIEM 룰에서 빈도 추적 가능.
				deps.Logger.WarnContext(r.Context(), "refresh 거부",
					slog.String("evt", "auth.refresh_denied"),
					slog.String("remote", r.RemoteAddr),
					slog.String("ua", r.UserAgent()),
					slog.String("rid", middleware.RequestIDFromContext(r.Context())),
					slog.Any("error", err),
				)
				writeError(w, http.StatusUnauthorized, "refresh_invalid", "refresh 만료/미존재 — 재로그인 필요")
				return
			}
			writeError(w, http.StatusInternalServerError, "refresh_store", err.Error())
			return
		}

		// 세션이 (logout / 만료 등으로) 사라진 경우 cookie_t 가 없으므로 재발급 불가.
		// auth.md §5 — logout 시 RefreshStore.DeleteBySID 도 같이 처리되어야 함.
		sess, err := deps.Sessions.Get(r.Context(), old.SID)
		if err != nil {
			deps.Logger.WarnContext(r.Context(), "refresh — 세션 미존재",
				slog.String("sid", old.SID),
				slog.Any("error", err),
			)
			writeError(w, http.StatusUnauthorized, "session_gone", "세션이 만료되었거나 logout 됨")
			return
		}

		now := time.Now()
		accessTTL := deps.AccessTokenTTL
		if accessTTL <= 0 {
			accessTTL = defaultAccessTTL
		}
		accessExp := now.Add(accessTTL)
		access, err := deps.JWTIssuer.Sign(auth.Claims{
			SID:  sess.ID,
			Usid: sess.Usid,
			Chan: sess.Channel,
			EXP:  accessExp.Unix(),
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "jwt_sign", err.Error())
			return
		}

		// 새 refresh 발급 — single-use rotation.
		refreshTTL := deps.RefreshTokenTTL
		if refreshTTL <= 0 {
			refreshTTL = defaultRefreshTTL
		}
		refreshExp := now.Add(refreshTTL)
		newRT, err := auth.NewRefreshTokenString()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "rng", err.Error())
			return
		}
		if err := deps.RefreshStore.Put(r.Context(), &auth.RefreshToken{
			Token:     newRT,
			SID:       sess.ID,
			Usid:      sess.Usid,
			Channel:   sess.Channel,
			IssuedAt:  now,
			ExpiresAt: refreshExp,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "refresh_store", err.Error())
			return
		}

		// audit log — refresh 성공. usid / sid / 새 만료시각 + client info.
		deps.Logger.InfoContext(r.Context(), "refresh 성공",
			slog.String("evt", "auth.refresh"),
			slog.String("usid", sess.Usid),
			slog.String("sid", sess.ID),
			slog.String("remote", r.RemoteAddr),
			slog.String("ua", r.UserAgent()),
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			slog.Time("access_exp", accessExp),
			slog.Time("refresh_exp", refreshExp),
		)
		writeJSON(w, http.StatusOK, RefreshResponse{
			AccessToken:  access,
			AccessExpAt:  accessExp,
			RefreshToken: newRT,
			RefreshExpAt: refreshExp,
			SID:          sess.ID,
		})
	}
}
