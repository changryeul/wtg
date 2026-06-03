package ratelimit

import (
	"context"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisLimiter — Lua script 기반 distributed token bucket. 다중 인스턴스
// (mci-edge-* N개) 가 같은 Redis 를 공유해 인스턴스 무관 단일 카운터.
//
// 의도:
//   - in-memory Limiter 는 인스턴스 별 독립 bucket 이라 한도 × N 발생
//   - Redis 단일 카운터로 정확한 분산 rate limit
//   - 알고리즘은 in-memory 와 동일 token bucket (정밀한 burst 유지)
//
// Lua script 가 atomic — race 없이 single Redis call 로 fetch + refill + claim.
type RedisLimiter struct {
	client *redis.Client
	rate   float64
	burst  int
	prefix string
	ttl    time.Duration
	logger *slog.Logger

	// fail-open 카운터 — Redis 호출 실패 시 (network 등) 호출자에게 통과
	// 시키되 운영자가 알람 잡을 수 있도록 누적. operations.md 의 alert 권장.
	failCount atomic.Uint64
}

// RedisLimiterOptions — RedisLimiter 생성 옵션.
type RedisLimiterOptions struct {
	// Client — 호출자가 dial 한 go-redis client. 필수.
	Client *redis.Client
	// RatePerSec — sustained TPS 한도.
	RatePerSec float64
	// Burst — 버킷 최대 깊이.
	Burst int
	// Prefix — Redis key 의 prefix (default "wtg:ratelimit:").
	// 운영 시 service / rule 별 별도 prefix 권장 (예: "wtg:rl:edge-api:POST /v1/tx:").
	Prefix string
	// KeyTTL — Redis 키 TTL — idle 키 자동 정리 (default 60s).
	KeyTTL time.Duration
	// Logger — Redis call 실패 시 warn. nil 가능.
	Logger *slog.Logger
}

// luaTokenBucket — atomic token bucket script.
//
// KEYS[1]: 키
// ARGV[1]: 현재 시각 (sec, 소수점 포함)
// ARGV[2]: rate (tokens/sec)
// ARGV[3]: burst (max tokens)
// ARGV[4]: cost (보통 1)
// ARGV[5]: TTL (sec)
//
// 반환: 1 = 허용, 0 = 거부.
//
// 상태 저장: HSET key tokens <float> ts <float>
// Redis 가 string 으로 저장하므로 tonumber 로 parsing.
const luaTokenBucket = `
local key   = KEYS[1]
local now   = tonumber(ARGV[1])
local rate  = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local cost  = tonumber(ARGV[4])
local ttl   = tonumber(ARGV[5])

local data = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts     = tonumber(data[2])
if tokens == nil then
	tokens = burst
	ts     = now
end

local elapsed = now - ts
if elapsed > 0 then
	tokens = math.min(burst, tokens + elapsed * rate)
end

local allowed = 0
if tokens >= cost then
	tokens = tokens - cost
	allowed = 1
end

redis.call('HMSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, ttl)
return allowed
`

// NewRedisLimiter — RedisLimiter 생성. Lua script 는 매 Allow 마다 EVAL.
// 빈번한 SCRIPT LOAD 를 피하려고 go-redis 의 자체 cache 사용.
func NewRedisLimiter(opt RedisLimiterOptions) *RedisLimiter {
	prefix := opt.Prefix
	if prefix == "" {
		prefix = "wtg:ratelimit:"
	}
	ttl := opt.KeyTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisLimiter{
		client: opt.Client,
		rate:   opt.RatePerSec,
		burst:  opt.Burst,
		prefix: prefix,
		ttl:    ttl,
		logger: logger,
	}
}

// Allow — Redis Lua 호출 + 결과 bool.
//
// Redis 호출 실패 시 fail-open (true 반환) — 운영 안전성 우선. failCount 증가로
// 운영자가 모니터링.
func (r *RedisLimiter) Allow(key string) bool {
	return r.allowAt(key, time.Now())
}

// allowAt — internal — Allow 와 동일하되 호출자 시각 주입. 테스트에서
// fast-forward 시뮬레이션 가능.
func (r *RedisLimiter) allowAt(key string, t time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	now := float64(t.UnixNano()) / 1e9
	res, err := r.client.Eval(ctx, luaTokenBucket,
		[]string{r.prefix + key},
		strconv.FormatFloat(now, 'f', 6, 64),
		strconv.FormatFloat(r.rate, 'f', 6, 64),
		strconv.Itoa(r.burst),
		"1",
		strconv.Itoa(int(r.ttl.Seconds())),
	).Result()
	if err != nil {
		r.failCount.Add(1)
		r.logger.Warn("Redis rate limit Eval 실패 — fail-open",
			slog.String("key", key), slog.Any("error", err))
		return true
	}
	allowed, ok := res.(int64)
	if !ok {
		r.failCount.Add(1)
		return true
	}
	return allowed == 1
}

// Stop — RedisLimiter 자체는 background goroutine 없음 — no-op.
// Redis client 는 호출자 관리.
func (r *RedisLimiter) Stop() {}

// FailCount — Redis 호출 실패 누적 (operations 모니터링).
func (r *RedisLimiter) FailCount() uint64 {
	return r.failCount.Load()
}

// MakeRedisFactories — service prefix 기반 Redis-backed factory 쌍 (rule + fallback).
//
// 키 구조: "wtg:rl:<service>:<rule.Pattern>:<key>" 또는 "wtg:rl:<service>:default:<key>"
// → service / rule 별 격리, 같은 키라도 룰이 다르면 별도 버킷.
//
// 사용:
//
//	rs, err := ratelimit.NewRuleSetWithFactory(rules, fallback,
//	    ratelimit.MakeRedisFactories(rdb, "edge-api", logger))
func MakeRedisFactories(client *redis.Client, service string, logger *slog.Logger) (LimiterFactory, func(*Config) AllowLimiter) {
	base := "wtg:rl:" + service + ":"
	return func(r Rule) AllowLimiter {
			return NewRedisLimiter(RedisLimiterOptions{
				Client: client, RatePerSec: r.Rate, Burst: r.Burst,
				Prefix: base + r.Pattern + ":",
				Logger: logger,
			})
		}, func(c *Config) AllowLimiter {
			return NewRedisLimiter(RedisLimiterOptions{
				Client: client, RatePerSec: c.RatePerSec, Burst: c.Burst,
				Prefix: base + "default:",
				Logger: logger,
			})
		}
}
