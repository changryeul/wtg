package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRefreshStore 는 refresh token 의 Redis-backed Store 구현.
//
// 운영 다중 인스턴스 mci-api 에서 한 인스턴스가 발급한 refresh 가 다른
// 인스턴스의 /v1/refresh 요청에서도 보이도록 공유 저장소 필수.
//
// 키 구조:
//
//	<prefix>:rt:<token>            → JSON RefreshToken (TTL = ExpiresAt-now)
//	<prefix>:sid:<sid>             → SET of tokens (DeleteBySID 의 fan-out 용)
//
// Consume 은 GET + DEL atomic 으로 — Lua script 또는 pipeline 사용.
// 본 구현은 단일 GETDEL command (Redis 6.2+) 로 single-use rotation 보장.
type RedisRefreshStore struct {
	rdb    redis.UniversalClient
	prefix string
	now    func() time.Time
}

// RedisRefreshStoreOptions — 옵션.
type RedisRefreshStoreOptions struct {
	// Prefix — 모든 키 앞에 붙음 (default "wtg:auth").
	Prefix string
	// Now — 시각 함수 (테스트 시간 조작용). nil 이면 time.Now.
	Now func() time.Time
}

// NewRedisRefreshStore — 호출자가 만든 redis.UniversalClient 그대로 주입.
// Close 는 호출자가 관리 (이 Store 의 Close 는 no-op).
func NewRedisRefreshStore(rdb redis.UniversalClient, opt RedisRefreshStoreOptions) *RedisRefreshStore {
	if opt.Prefix == "" {
		opt.Prefix = "wtg:auth"
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &RedisRefreshStore{
		rdb:    rdb,
		prefix: opt.Prefix,
		now:    opt.Now,
	}
}

func (s *RedisRefreshStore) keyToken(token string) string {
	return fmt.Sprintf("%s:rt:%s", s.prefix, token)
}

func (s *RedisRefreshStore) keySID(sid string) string {
	return fmt.Sprintf("%s:sid:%s", s.prefix, sid)
}

// refreshDTO — JSON 직렬화 모양.
type refreshDTO struct {
	Token     string    `json:"token"`
	SID       string    `json:"sid"`
	Usid      string    `json:"usid"`
	Channel   string    `json:"channel"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Put — 토큰 + SID→token set 두 키 동시 SET. TTL 은 ExpiresAt 까지.
func (s *RedisRefreshStore) Put(ctx context.Context, t *RefreshToken) error {
	if t == nil || t.Token == "" {
		return errors.New("auth: RefreshToken.Token 필수")
	}
	now := s.now()
	if t.IssuedAt.IsZero() {
		t.IssuedAt = now
	}
	ttl := time.Until(t.ExpiresAt)
	if ttl <= 0 {
		ttl = 1 * time.Second // 최소 TTL — 즉시 만료보단 1s 가 안전 (race)
	}
	data, err := json.Marshal(refreshDTO{
		Token:     t.Token,
		SID:       t.SID,
		Usid:      t.Usid,
		Channel:   t.Channel,
		IssuedAt:  t.IssuedAt,
		ExpiresAt: t.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("auth: refresh marshal: %w", err)
	}

	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.keyToken(t.Token), data, ttl)
	pipe.SAdd(ctx, s.keySID(t.SID), t.Token)
	// SID set 의 TTL 은 max(현재 TTL, 새 토큰 TTL) — 단순화 위해 새 토큰 TTL 로
	// EXPIRE. 같은 SID 에 더 긴 토큰이 이미 있어도 그 토큰의 TTL 은 자체 키에
	// 살아있어 작동에 영향 없음.
	pipe.Expire(ctx, s.keySID(t.SID), ttl)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("auth: redis Put: %w", err)
	}
	return nil
}

// Consume — single-use rotation. GETDEL (Redis 6.2+) 으로 atomic GET+DELETE.
// 만료 시 redis.Nil 반환되며 ErrRefreshNotFound 로 매핑.
func (s *RedisRefreshStore) Consume(ctx context.Context, token string) (*RefreshToken, error) {
	if token == "" {
		return nil, ErrRefreshNotFound
	}
	raw, err := s.rdb.GetDel(ctx, s.keyToken(token)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrRefreshNotFound
		}
		return nil, fmt.Errorf("auth: redis Consume: %w", err)
	}
	var d refreshDTO
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("auth: refresh unmarshal: %w", err)
	}
	rt := &RefreshToken{
		Token:     d.Token,
		SID:       d.SID,
		Usid:      d.Usid,
		Channel:   d.Channel,
		IssuedAt:  d.IssuedAt,
		ExpiresAt: d.ExpiresAt,
	}
	if rt.Expired(s.now()) {
		// TTL 이 잘 작동하면 도달 안 함 — race 방어.
		return nil, ErrRefreshExpired
	}
	// SID set 에서도 정리 (best-effort, 실패해도 무시 — TTL 로 자연 만료).
	_ = s.rdb.SRem(ctx, s.keySID(rt.SID), token).Err()
	return rt, nil
}

// DeleteBySID — logout 시 호출. SID 에 묶인 모든 token 일괄 제거.
func (s *RedisRefreshStore) DeleteBySID(ctx context.Context, sid string) (int, error) {
	tokens, err := s.rdb.SMembers(ctx, s.keySID(sid)).Result()
	if err != nil {
		return 0, fmt.Errorf("auth: SMembers: %w", err)
	}
	if len(tokens) == 0 {
		return 0, nil
	}
	pipe := s.rdb.TxPipeline()
	for _, t := range tokens {
		pipe.Del(ctx, s.keyToken(t))
	}
	pipe.Del(ctx, s.keySID(sid))
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("auth: pipe Exec: %w", err)
	}
	return len(tokens), nil
}

// Close — no-op (rdb 의 lifecycle 은 호출자 책임).
func (s *RedisRefreshStore) Close() error { return nil }
