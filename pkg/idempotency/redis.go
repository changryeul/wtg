package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore — 다중 인스턴스 운영용 Redis backend.
//
// key 구조:
//
//	<prefix><key>      : body hash (hex string, 64 char). 존재 자체가 reservation.
//	<prefix><key>:r    : reply JSON (committed 시에만 존재).
//
// 두 key 모두 동일 TTL. Lua atomic 처리로 race condition 안전.
type RedisStore struct {
	client redisCmdable
	prefix string
	ttl    time.Duration
}

// redisCmdable — go-redis 의 Eval 만 사용. *redis.Client / *redis.ClusterClient
// 등이 모두 만족.
type redisCmdable interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

// RedisOptions — RedisStore 생성 옵션.
type RedisOptions struct {
	// Client — go-redis client. 호출자가 lifecycle 관리.
	Client redisCmdable

	// Prefix — Redis key prefix. 빈값이면 "wtg:idem:".
	Prefix string

	// TTL — reservation / cached reply 만료. 0 이면 5분.
	TTL time.Duration
}

// NewRedisStore — 옵션 검증 후 store 생성.
func NewRedisStore(opt RedisOptions) (*RedisStore, error) {
	if opt.Client == nil {
		return nil, errors.New("idempotency: Redis Client 필수")
	}
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg:idem:"
	}
	ttl := opt.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &RedisStore{client: opt.Client, prefix: prefix, ttl: ttl}, nil
}

// luaReserve — atomic 상태 점검 + 신규 reservation.
//
// 반환 status 값은 Go 의 Status enum (Miss=0, Cached=1, InFlight=2, Conflict=3)
// 과 정확히 일치해야 함.
//
//	{0, ''}        Miss     — 새 reservation
//	{1, replyJSON} Cached   — reply 있음, payload 에 동봉
//	{2, ''}        InFlight — 같은 hash 의 reservation, reply 미생성
//	{3, ''}        Conflict — 다른 hash 의 기존 reservation
const luaReserve = `
local key = KEYS[1]
local replyKey = key .. ':r'
local hash = ARGV[1]
local ttl = tonumber(ARGV[2])

local cur = redis.call('GET', key)
if not cur then
    redis.call('SET', key, hash, 'PX', ttl)
    return {0, ''}
end
if cur ~= hash then
    return {3, ''}
end
local reply = redis.call('GET', replyKey)
if not reply then
    return {2, ''}
end
return {1, reply}
`

// luaCommit — reply 저장 + 두 key TTL 갱신.
const luaCommit = `
local key = KEYS[1]
local replyKey = key .. ':r'
local replyJSON = ARGV[1]
local ttl = tonumber(ARGV[2])

redis.call('SET', replyKey, replyJSON, 'PX', ttl)
redis.call('PEXPIRE', key, ttl)
return 1
`

// luaRollback — reservation 해제. 이미 committed 면 보존 (no-op).
const luaRollback = `
local key = KEYS[1]
local replyKey = key .. ':r'

local reply = redis.call('GET', replyKey)
if reply then
    return 0
end
redis.call('DEL', key)
return 1
`

// Reserve — Lua atomic 으로 상태 점검.
func (s *RedisStore) Reserve(ctx context.Context, key string, bodyHash [32]byte) (Status, *CachedReply, error) {
	hash := hashHex(bodyHash)
	ttlMs := strconv.FormatInt(s.ttl.Milliseconds(), 10)
	res, err := s.client.Eval(ctx, luaReserve, []string{s.prefix + key}, hash, ttlMs).Result()
	if err != nil {
		return 0, nil, fmt.Errorf("idempotency: redis reserve: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return 0, nil, fmt.Errorf("idempotency: redis reserve 응답 형식: %T", res)
	}
	statusN, _ := arr[0].(int64)
	switch Status(statusN) {
	case StatusMiss:
		return StatusMiss, nil, nil
	case StatusConflict:
		return StatusConflict, nil, nil
	case StatusInFlight:
		return StatusInFlight, nil, nil
	case StatusCached:
		payload, _ := arr[1].(string)
		var reply CachedReply
		if err := json.Unmarshal([]byte(payload), &reply); err != nil {
			return 0, nil, fmt.Errorf("idempotency: cached reply unmarshal: %w", err)
		}
		return StatusCached, &reply, nil
	default:
		return 0, nil, fmt.Errorf("idempotency: 알 수 없는 status %d", statusN)
	}
}

// Commit — reservation 의 reply 저장 + TTL 갱신.
func (s *RedisStore) Commit(ctx context.Context, key string, reply *CachedReply) error {
	if reply == nil {
		return errors.New("idempotency: Commit reply nil")
	}
	payload, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("idempotency: reply marshal: %w", err)
	}
	ttlMs := strconv.FormatInt(s.ttl.Milliseconds(), 10)
	if _, err := s.client.Eval(ctx, luaCommit, []string{s.prefix + key}, string(payload), ttlMs).Result(); err != nil {
		return fmt.Errorf("idempotency: redis commit: %w", err)
	}
	return nil
}

// Rollback — reservation 해제. committed 상태면 no-op.
func (s *RedisStore) Rollback(ctx context.Context, key string) error {
	if _, err := s.client.Eval(ctx, luaRollback, []string{s.prefix + key}).Result(); err != nil {
		return fmt.Errorf("idempotency: redis rollback: %w", err)
	}
	return nil
}

// Close — client lifecycle 은 호출자 관리. no-op.
func (s *RedisStore) Close() error { return nil }

// hashHex — sha256 32 byte → 64 char hex (Redis 저장용).
func hashHex(h [32]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range h {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0xf]
	}
	return string(out)
}
