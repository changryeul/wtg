package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/session"
)

// RedisStore 는 Session 의 Redis-backed Store 구현.
//
// 운영 시나리오:
//
//   - 다중 mci-api / mci-push 인스턴스 간 세션 공유 (sticky 라우팅 불필요)
//   - 인스턴스 재시작 후 즉시 세션 복구 (재로그인 강요 X)
//
// 영속 대상 (Session 의 인증 컨텍스트):
//
//   - ID, Usid, Channel, Cookie (mymq.Cookie wire bytes)
//   - IssuedAt, ExpiresAt, LastSeenAt
//   - Profile (Channel, Site, Tier), LogonID
//
// 영속 안 함 (ws-local, sticky 가정):
//
//   - Subscribed (구독 중인 통화쌍 집합) — ws 재연결 시 재구축
//
// 키 설계:
//
//   - "<prefix>:session:<id>"      — Session JSON 직렬화
//   - TTL = Session.ExpiresAt - now (Put 시점). 만료 후 Redis 가 자동 삭제.
//   - 슬라이딩 TTL 은 Get 호출 시 LastSeenAt 만 갱신 (만료시각 자체는 변경 안 함 —
//     MemoryStore 와 동일한 의미).
type RedisStore struct {
	rdb    redis.UniversalClient
	prefix string
	now    func() time.Time
}

// RedisStoreOptions 는 RedisStore 생성 옵션.
type RedisStoreOptions struct {
	// Prefix 는 모든 키 앞에 붙는 namespace (default "wtg:auth").
	// 여러 환경(dev/staging/prod) 또는 여러 서비스가 같은 Redis 를 쓸 때 충돌 회피.
	Prefix string
	// Now 는 현재 시각 함수 (테스트 시간 조작용). nil 이면 time.Now.
	Now func() time.Time
}

// NewRedisStore 는 RedisStore 를 생성한다.
// 호출자가 만든 redis.UniversalClient (Client / ClusterClient / FailoverClient)
// 를 그대로 주입 — Close 도 호출자가 관리한다.
func NewRedisStore(rdb redis.UniversalClient, opt RedisStoreOptions) *RedisStore {
	if opt.Prefix == "" {
		opt.Prefix = "wtg:auth"
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &RedisStore{
		rdb:    rdb,
		prefix: opt.Prefix,
		now:    opt.Now,
	}
}

// sessionDTO 는 Session 의 직렬화 모양 (인증 컨텍스트만).
// Subscribed / subMu 는 의도적으로 제외.
type sessionDTO struct {
	ID         string          `json:"id"`
	Usid       string          `json:"usid"`
	Channel    string          `json:"channel"`
	CookieB64  string          `json:"cookie_b64,omitempty"`
	IssuedAt   time.Time       `json:"issued_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	LastSeenAt time.Time       `json:"last_seen_at"`
	Profile    session.Profile `json:"profile"`
	LogonID    session.LogonID `json:"logon_id"`
}

func toDTO(s *Session) *sessionDTO {
	d := &sessionDTO{
		ID:         s.ID,
		Usid:       s.Usid,
		Channel:    s.Channel,
		IssuedAt:   s.IssuedAt,
		ExpiresAt:  s.ExpiresAt,
		LastSeenAt: s.LastSeenAt,
		Profile:    s.Profile,
		LogonID:    s.LogonID,
	}
	if s.Cookie != nil {
		buf := make([]byte, mymq.CookieWire)
		mymq.EncodeCookie(buf, s.Cookie)
		d.CookieB64 = base64.StdEncoding.EncodeToString(buf)
	}
	return d
}

func fromDTO(d *sessionDTO) (*Session, error) {
	s := &Session{
		ID:         d.ID,
		Usid:       d.Usid,
		Channel:    d.Channel,
		IssuedAt:   d.IssuedAt,
		ExpiresAt:  d.ExpiresAt,
		LastSeenAt: d.LastSeenAt,
		Profile:    d.Profile,
		LogonID:    d.LogonID,
	}
	if d.CookieB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(d.CookieB64)
		if err != nil {
			return nil, fmt.Errorf("redis store: cookie base64 decode: %w", err)
		}
		if len(raw) != mymq.CookieWire {
			return nil, fmt.Errorf("redis store: cookie wire len %d, want %d", len(raw), mymq.CookieWire)
		}
		c := mymq.DecodeCookie(raw)
		s.Cookie = &c
	}
	return s, nil
}

func (s *RedisStore) key(id string) string {
	return s.prefix + ":session:" + id
}

// Put 은 세션을 Redis 에 저장한다.
// IssuedAt/LastSeenAt 이 0 이면 자동 채우고, TTL 은 (ExpiresAt - now) 로 설정.
// ExpiresAt 이 0 이면 TTL 없이 영속 저장 (auth.md 정책상 권장 안 함).
func (s *RedisStore) Put(ctx context.Context, sess *Session) error {
	if sess == nil || sess.ID == "" {
		return errors.New("redis store: 세션 또는 ID 가 비어있음")
	}
	now := s.now()
	if sess.IssuedAt.IsZero() {
		sess.IssuedAt = now
	}
	sess.LastSeenAt = now

	dto := toDTO(sess)
	body, err := json.Marshal(dto)
	if err != nil {
		return fmt.Errorf("redis store: marshal: %w", err)
	}

	ttl := time.Duration(0)
	if !sess.ExpiresAt.IsZero() {
		ttl = sess.ExpiresAt.Sub(now)
		if ttl <= 0 {
			// 이미 만료된 세션은 저장하지 않음 — Get 즉시 not-found.
			return ErrSessionExpired
		}
	}
	return s.rdb.Set(ctx, s.key(sess.ID), body, ttl).Err()
}

// Get 은 세션을 조회한다.
//   - 키 없음     → ErrSessionNotFound
//   - 직렬화 손상 → 에러 (Redis 데이터 무결성 문제)
//   - 만료(now 기준) → ErrSessionExpired + 삭제
//   - 정상 → LastSeenAt 갱신(슬라이딩 TTL 갱신은 하지 않음 — auth.md §6 의 의미)
func (s *RedisStore) Get(ctx context.Context, id string) (*Session, error) {
	body, err := s.rdb.Get(ctx, s.key(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("redis store: get: %w", err)
	}
	var dto sessionDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		return nil, fmt.Errorf("redis store: unmarshal: %w", err)
	}
	sess, err := fromDTO(&dto)
	if err != nil {
		return nil, err
	}

	now := s.now()
	if sess.Expired(now) {
		// 만료 — 즉시 삭제 + 사용자에게 알림. (Redis TTL 이 아직 청소 안 했을 수도 있음.)
		_ = s.rdb.Del(ctx, s.key(id)).Err()
		return nil, ErrSessionExpired
	}

	// 슬라이딩 — LastSeenAt 만 갱신. 만료시각 자체는 변경 안 함.
	sess.LastSeenAt = now
	// 갱신본을 다시 write (best-effort; 실패해도 Get 자체는 성공으로 처리).
	if dto2 := toDTO(sess); dto2 != nil {
		if body2, err := json.Marshal(dto2); err == nil {
			ttl := sess.ExpiresAt.Sub(now)
			if ttl > 0 {
				_ = s.rdb.Set(ctx, s.key(id), body2, ttl).Err()
			}
		}
	}
	return sess, nil
}

// Delete 는 세션을 즉시 제거. 미존재 무시.
func (s *RedisStore) Delete(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := s.rdb.Del(ctx, s.key(id)).Err(); err != nil {
		return fmt.Errorf("redis store: del: %w", err)
	}
	return nil
}

// Close 는 no-op — redis.Client 의 lifecycle 은 호출자가 관리.
func (s *RedisStore) Close() error {
	return nil
}
