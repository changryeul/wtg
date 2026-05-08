package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

// 인증 컨텍스트 — 핸들러가 사용자 식별 정보를 꺼낼 수 있게 한다.
// auth.md 의 위임 모델에 따라, 여기서는 사용자가 누구인지(Usid)만 확정하고
// 그 외 권한은 매매 엔진에 위임한다.

type authCtxKey int

const principalKey authCtxKey = 1

// 신뢰된 edge → mci-api 사이의 claim 전달 헤더.
// 외부 listener 에서는 반드시 stripIngressHeaders 로 제거된다.
const (
	HeaderEdgeSID     = "X-WTG-SID"
	HeaderEdgeUser    = "X-WTG-User"
	HeaderEdgeChannel = "X-WTG-Channel"
)

// Principal 은 인증된 사용자 식별.
// auth.md §6 의 JWT claim 에서 추출되거나, DevMode 에서는 X-WTG-User 헤더에서.
type Principal struct {
	Usid      string       // 사용자 ID (cookie_t.usid 로 매핑됨)
	Channel   string       // 채널 ("WEB" 등). 보통 ChannelWeb 고정.
	SessionID string       // SessionMode 에서만 채워짐. DevMode 는 빈 문자열.
	Cookie    *mymq.Cookie // SessionMode 시 broker 첨부용 cookie_t. DevMode 는 nil.
}

// PrincipalFromContext 는 context 에서 Principal 을 추출한다.
// 인증 미들웨어를 통과하지 않은 경로에서는 nil.
func PrincipalFromContext(ctx context.Context) *Principal {
	if v, ok := ctx.Value(principalKey).(*Principal); ok {
		return v
	}
	return nil
}

// ContextWithPrincipal 은 context 에 Principal 을 주입한다.
// 일반 요청 흐름에서는 Auth 미들웨어가 자동 호출하므로 호출자는 보통
// PrincipalFromContext 만 사용한다. 테스트나 외부 인증 통합 (gRPC stream 등)
// 에서 직접 인증 결과를 주입할 때 사용.
func ContextWithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// AuthConfig 는 Auth 미들웨어의 동작을 제어한다.
//
// 인증 모드 우선순위 (위에서부터):
//
//  1. DevMode=true        — X-WTG-User 헤더만 신뢰. 운영 금지.
//  2. TrustEdgeHeaders    — X-WTG-SID 헤더(Internal 망에서 mci-edge-api 가
//     주입) 만 보고 SessionStore 에서 cookie_t 복원. auth.md §4 흐름의 Internal
//     단. 외부 노출 endpoint 에서는 절대 활성화 금지 — 헤더 위조 가능.
//  3. JWTVerifier!=nil    — Authorization: Bearer <JWT(RS256)> 검증.
//     SessionStore 가 있으면 claim.SID 로 cookie_t 까지 복원 (Internal),
//     없으면 claim 만으로 Principal 생성 (DMZ edge — cookie 첨부 불필요).
//  4. SessionStore!=nil   — Authorization: Bearer <session_id> raw 토큰. 1차
//     호환 모드 (JWT 미배포 환경).
//  5. (위 어느 것도 아니면) — 401.
type AuthConfig struct {
	DevMode bool

	// SessionStore 는 cookie_t 보관소.
	// Internal 서비스(mci-api / mci-admin) 에서는 운영상 필수.
	// DMZ edge 에서는 nil — secret 을 DMZ 에 두지 않는다 (auth.md §1).
	SessionStore auth.Store

	// JWTVerifier 는 access JWT 검증기. DMZ edge 는 public key 만 가지므로
	// SessionStore=nil 이어도 JWT 검증 자체는 가능하다.
	JWTVerifier *auth.Verifier

	// TrustEdgeHeaders 가 true 면 mci-edge-api 가 검증/주입한 X-WTG-SID 헤더를
	// 신뢰한다. mTLS 로 보호된 Internal 망에서만 활성화. 외부 listener 에서는
	// 반드시 false.
	TrustEdgeHeaders bool

	Logger *slog.Logger
}

// Auth 는 JWT 또는 (DevMode 시) 헤더 기반 인증 미들웨어.
//
// Phase 2 단계: 운영팀과 인증 합의 (auth.md) 가 끝나기 전이라 RS256/Redis 통합은
// 미구현. DevMode 또는 stub 검증만 동작한다. Phase 2~3 사이에 본격 통합 예정.
//
// 인증 실패 시 401 Unauthorized + JSON 에러 응답.
func Auth(cfg AuthConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 인증 우회 경로 — 헬스체크 등.
			if isPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			p, err := authenticate(r, cfg)
			if err != nil {
				if cfg.Logger != nil {
					cfg.Logger.WarnContext(r.Context(), "인증 실패",
						slog.String("path", r.URL.Path),
						slog.String("rid", RequestIDFromContext(r.Context())),
						slog.Any("error", err),
					)
				}
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", err.Error())
				return
			}
			ctx := context.WithValue(r.Context(), principalKey, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isPublicPath 는 인증 없이 통과시킬 path 패턴을 정의한다.
//
//   - /v1/ping, /healthz, /readyz : 헬스체크
//   - /metrics                    : Prometheus scrape (사내망 노출 정책)
//   - /v1/login                   : 로그인 자체는 세션이 없으므로 인증 우회
//   - /v1/refresh                 : refresh token 본인이 인증 — 미들웨어 우회
//
// /metrics 는 사내 모니터링 시스템(Prometheus) 이 수집하므로 외부 노출되지
// 않는 사내망 IP 또는 K8s ServiceMonitor 만 도달 가능해야 한다 (운영 정책).
func isPublicPath(path string) bool {
	switch path {
	case "/v1/ping", "/healthz", "/readyz", "/metrics", "/v1/login", "/v1/refresh":
		return true
	}
	return false
}

// authenticate 는 요청에서 Principal 을 추출한다.
func authenticate(r *http.Request, cfg AuthConfig) (*Principal, error) {
	if cfg.DevMode {
		usid := r.Header.Get(HeaderEdgeUser)
		if usid == "" {
			return nil, errMissingUser
		}
		// DevMode 에서 X-WTG-Channel 헤더로 채널 spoof 가능 — 채널별 정책
		// (kill switch scope 등) 검증 도구에서 사용. 빈 헤더면 WEB 디폴트.
		ch := strings.ToUpper(strings.TrimSpace(r.Header.Get(HeaderEdgeChannel)))
		if ch == "" {
			ch = "WEB"
		}
		return &Principal{Usid: usid, Channel: ch}, nil
	}
	if cfg.TrustEdgeHeaders {
		return authenticateEdgeHeaders(r, cfg.SessionStore)
	}
	if cfg.JWTVerifier != nil {
		return authenticateJWT(r, cfg.JWTVerifier, cfg.SessionStore)
	}
	if cfg.SessionStore != nil {
		return authenticateSession(r, cfg.SessionStore)
	}
	return nil, errAuthNotImplemented
}

// authenticateEdgeHeaders — mci-edge-api 가 주입한 X-WTG-SID 헤더 신뢰.
//
// SessionStore 에서 cookie_t 복원. Edge 와 mci-api 사이는 mTLS 가 강제되어야
// 하며, 외부 노출 listener 에서는 이 모드를 절대 활성화하면 안 된다.
func authenticateEdgeHeaders(r *http.Request, store auth.Store) (*Principal, error) {
	sid := r.Header.Get(HeaderEdgeSID)
	if sid == "" {
		return nil, errMissingEdgeHeader
	}
	if store == nil {
		return nil, errAuthNotImplemented
	}
	sess, err := store.Get(r.Context(), sid)
	if err != nil {
		return nil, errInvalidSession
	}
	return &Principal{
		Usid:      sess.Usid,
		Channel:   sess.Channel,
		SessionID: sess.ID,
		Cookie:    sess.Cookie,
	}, nil
}

// authenticateJWT 는 access JWT 를 검증한다.
//
// store 가 채워져 있으면 (Internal) claim.SID 로 SessionStore 조회 → cookie_t
// 까지 복원. store 가 nil 이면 (DMZ edge) claim 정보만으로 Principal 생성하고
// cookie 는 nil — edge 는 broker 호출을 안 하므로 cookie 불필요. 실제 broker
// 호출은 Internal mci-api 에서 헤더 신뢰 모드로 수행.
func authenticateJWT(r *http.Request, ver *auth.Verifier, store auth.Store) (*Principal, error) {
	tok, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	claims, err := ver.Verify(tok)
	if err != nil {
		// 만료/서명실패/형식오류 모두 클라이언트 입장에서 동일 — refresh 또는 재로그인.
		return nil, errInvalidJWT
	}
	if store == nil {
		// Edge 모드 — cookie 미복원, claim 그대로 전달.
		return &Principal{
			Usid:      claims.Usid,
			Channel:   claims.Chan,
			SessionID: claims.SID,
		}, nil
	}
	sess, err := store.Get(r.Context(), claims.SID)
	if err != nil {
		return nil, errInvalidSession
	}
	return &Principal{
		Usid:      sess.Usid,
		Channel:   sess.Channel,
		SessionID: sess.ID,
		Cookie:    sess.Cookie,
	}, nil
}

// authenticateSession 은 Bearer 토큰(session_id) → SessionStore 조회.
//
// auth.md §4 흐름의 4단계. JWT 통합 전 단계라 Bearer 값이 raw session_id.
// JWT 도입 후에는 sid claim 을 꺼내서 동일 Store.Get 을 호출한다.
func authenticateSession(r *http.Request, store auth.Store) (*Principal, error) {
	token, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	sess, err := store.Get(r.Context(), token)
	if err != nil {
		// 만료/미존재 모두 클라이언트 입장에서는 재로그인 — 동일 401.
		if errors.Is(err, auth.ErrSessionNotFound) || errors.Is(err, auth.ErrSessionExpired) {
			return nil, errInvalidSession
		}
		return nil, err
	}
	return &Principal{
		Usid:      sess.Usid,
		Channel:   sess.Channel,
		SessionID: sess.ID,
		Cookie:    sess.Cookie,
	}, nil
}

// bearerToken 은 "Authorization: Bearer <token>" 에서 token 을 추출.
func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", errMissingAuthHeader
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", errBadAuthScheme
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", errMissingAuthHeader
	}
	return tok, nil
}

// 인증 에러 sentinel.
var (
	errMissingUser        = stringError("X-WTG-User 헤더 필요 (DevMode)")
	errAuthNotImplemented = stringError("운영 인증 미구현 — DevMode 사용 또는 SessionStore 주입 필요")
	errMissingAuthHeader  = stringError("Authorization 헤더 필요")
	errBadAuthScheme      = stringError("Authorization 스킴은 Bearer 이어야 함")
	errInvalidSession     = stringError("세션 만료 또는 미존재 — 재로그인 필요")
	errInvalidJWT         = stringError("JWT 만료/검증실패 — refresh 또는 재로그인 필요")
	errMissingEdgeHeader  = stringError("X-WTG-SID 헤더 필요 (edge trust 모드)")
)

type stringError string

func (e stringError) Error() string { return string(e) }

// writeJSONError 는 표준 에러 응답 포맷.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":   code,
		"message": msg,
	})
}
