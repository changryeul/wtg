// Package handlers 는 mci-api 의 HTTP 핸들러.
//
// 각 핸들러는 Deps 를 주입받는 함수형 패턴이다 (struct 기반보다 wire-up 단순).
//
//	mux.HandleFunc("POST /v1/orders", handlers.CreateOrder(deps))
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
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/routing"
)

// Caller 는 mymq.Client 의 동기 RPC 인터페이스 (테스트 mock 가능).
//
// 일반 운영에서는 *mymq.Client 가 자동으로 만족시키므로 호출자는 신경 쓸
// 필요 없다. 단위 테스트에서는 fake 구현체를 주입해서 broker 응답을
// 시뮬레이션한다.
type Caller interface {
	Call(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error)
}

// Deps 는 모든 핸들러가 공유하는 의존성.
type Deps struct {
	// MQ 는 mymq broker 호출용 Caller (운영: *mymq.Client / 테스트: fakeCaller).
	MQ          Caller
	CallTimeout time.Duration
	Logger      *slog.Logger

	// Sessions 는 login 시 발급되어 logout/인증 미들웨어에서 조회되는 세션 저장소.
	// nil 이면 login/logout 핸들러는 503 — DevMode 운영에서만 허용.
	Sessions auth.Store

	// SessionTTL 은 신규 세션의 만료 시간. 0 이면 8h (auth.md §6 default).
	SessionTTL time.Duration

	// DefaultChannel 은 LoginRequest.Channel 이 비었을 때의 디폴트.
	// mci-api: "WEB", mci-admin: "ADMIN". 빈 값이면 "WEB".
	DefaultChannel string

	// Routes 는 transaction alias → exchange/routing_key 룰 저장소.
	// nil 이면 envelope.Alias 를 무시하고 envelope 의 raw 값 그대로 사용.
	Routes routing.Registry

	// Policy 는 운영 정책 엔진 (kill switch / 정비창 / 차단 심볼·routing-key).
	// nil 이면 정책 검사 비활성 (모두 허용).
	Policy *policy.Engine

	// JWTIssuer 가 채워지면 Login 이 access JWT 를 발급한다 (auth.md §6).
	// nil 이면 raw session_id 만 응답에 담음 (1차 호환).
	JWTIssuer *auth.Issuer

	// AccessTokenTTL 은 access JWT 만료. 0 이면 15분 (auth.md §6).
	AccessTokenTTL time.Duration

	// RefreshStore + RefreshTokenTTL 가 채워지면 Login 이 refresh token 도 발급.
	// /v1/refresh 핸들러도 사용 — 둘 다 nil 이면 refresh 흐름 비활성.
	RefreshStore    auth.RefreshStore
	RefreshTokenTTL time.Duration
}

// writeJSON 은 표준 JSON 응답 헬퍼. 에러 발생 시 access log 가 캡처하므로
// 별도 처리 안 함.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 는 표준 에러 응답 — 인증 미들웨어와 동일 포맷.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error":   code,
		"message": msg,
	})
}

// mapBrokerError 는 mymq 에러를 HTTP status + 에러 코드로 매핑한다.
//
// auth.md 의 권한 위임 원칙에 따라, 비즈니스 거부(권한/한도/시간 등) 사유는
// 매매 엔진의 errn 그대로 전달하고, 응답 본문에 errn 을 포함시킨다.
func mapBrokerError(err error) (status int, code, msg string) {
	switch {
	case errors.Is(err, mymq.ErrAuthErr):
		return http.StatusUnauthorized, "auth", err.Error()
	case errors.Is(err, mymq.ErrTimeoutErr), errors.Is(err, mymq.ErrSvcTimeoutErr):
		return http.StatusGatewayTimeout, "timeout", err.Error()
	case errors.Is(err, mymq.ErrNoSvcErr):
		return http.StatusBadRequest, "no_service", err.Error()
	case errors.Is(err, mymq.ErrTooBigErr):
		return http.StatusRequestEntityTooLarge, "too_big", err.Error()
	case errors.Is(err, mymq.ErrBadArgErr):
		return http.StatusBadRequest, "bad_argument", err.Error()
	case errors.Is(err, mymq.ErrReconnecting):
		return http.StatusServiceUnavailable, "reconnecting", "broker 재연결 중"
	case errors.Is(err, mymq.ErrClientClosed):
		return http.StatusServiceUnavailable, "broker_unavailable", "broker connection 종료됨"
	}
	// fallback: broker 비즈니스 에러는 422 + errn 동봉.
	var mqErr *mymq.Error
	if errors.As(err, &mqErr) {
		return http.StatusUnprocessableEntity, "broker_error", err.Error()
	}
	return http.StatusInternalServerError, "internal", err.Error()
}

// principalRequired 는 핸들러 진입 시 인증된 Principal 을 추출한다.
// 인증 미들웨어 통과 전이면 false 와 401 응답이 자동 처리된다.
func principalRequired(w http.ResponseWriter, r *http.Request) (*middleware.Principal, bool) {
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || p.Usid == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "인증되지 않은 요청")
		return nil, false
	}
	return p, true
}
