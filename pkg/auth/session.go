// Package auth 는 WTG 의 web-layer 인증 빌딩 블록을 제공한다.
//
// auth.md §1 의 위임 모델을 따른다 — 사용자 본인 확인(Authentication) 만
// WTG 가 처리하고, 비즈니스 권한(Authorization) 은 매매 엔진에 위임.
//
// 이 패키지의 책임:
//
//   - Session 저장소 (login → cookie_t 보관, request → cookie_t 복원)
//   - 향후: JWT 발급/검증 (RS256), TOTP, audit emitter (auth.md §11)
//
// 1차 prototype 은 in-memory Store 만 제공하며, 운영 환경에서는 Redis 구현으로
// 차환한다 (auth.md §7). 인터페이스가 일치하므로 호출자 변경은 필요 없다.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// Session 은 로그인 한 번에 대응하는 web 세션 단위.
//
// auth.md §3 흐름의 6단계 — mci-api 가 LOGON 응답에서 cookie_t 를 받아
// 이 구조체로 감싸 Store 에 저장한다. 이후 모든 요청은 SessionID 만 들고
// 와서 cookie_t 를 복원한다.
type Session struct {
	ID         string       // 외부 노출용 불투명 식별자 (auth.md §6 의 sid)
	Usid       string       // 사용자 ID (cookie.Usid 와 동일, 디버깅/감사용)
	Channel    string       // 채널 코드 ("WEB" / "ADMIN" / "FIX" 등)
	Cookie     *mymq.Cookie // 매매 엔진에 첨부할 cookie_t
	IssuedAt   time.Time    // 발급 시각
	ExpiresAt  time.Time    // 만료 시각 (auth.md §6 refresh 만료, default 8h)
	LastSeenAt time.Time    // 마지막 사용 시각 (슬라이딩 TTL 용)
}

// Expired 는 now 기준으로 세션이 만료되었는지 반환한다.
func (s *Session) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// 표준 에러 sentinel — 호출자가 errors.Is 로 분기 가능.
var (
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionExpired  = errors.New("auth: session expired")
)

// Store 는 Session 영속화 인터페이스.
//
// 구현체:
//
//   - MemoryStore — 단일 인스턴스용 (테스트, dev). TTL 만료 + 슬라이딩.
//   - RedisStore  — 운영용 (auth.md §7). 별도 PR 에서 구현 예정.
//
// 모든 메서드는 goroutine-safe.
type Store interface {
	// Put 은 세션을 저장한다. 동일 ID 가 있으면 덮어쓴다.
	Put(ctx context.Context, s *Session) error

	// Get 은 세션을 조회한다.
	//   - 존재하지 않으면 ErrSessionNotFound
	//   - 만료된 세션은 ErrSessionExpired (자동 삭제)
	//   - 존재하면 LastSeenAt 갱신 (슬라이딩 TTL)
	Get(ctx context.Context, id string) (*Session, error)

	// Delete 는 세션을 삭제한다 (LOGOUT 시).
	// 존재하지 않아도 에러 아님.
	Delete(ctx context.Context, id string) error

	// Close 는 백그라운드 goroutine 등 리소스를 정리한다.
	Close() error
}

// NewSessionID 는 충돌 확률이 무시 가능한 무작위 ID 를 만든다.
//
// 길이 24 byte → base32 (RFC 4648, no-pad) 39 자.
// auth.md §6 의 sid 형식. 외부에 노출되므로 추측 불가능해야 한다.
func NewSessionID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}
