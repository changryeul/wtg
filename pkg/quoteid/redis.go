package quoteid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRegistry 는 Redis 기반 Registry — 운영 active-active mci-price 가
// 공유. 두 인스턴스가 발급한 QuoteID 가 동일 Redis 에 누적되어 매칭 엔진의
// 단일 검증 source 가 된다.
//
// 키 설계:
//
//   - "<prefix>:q:<quoteid>"  — Record JSON, TTL = (ValidUntil - now) + grace
//
// TTL 이 지나면 Redis 가 자동 삭제 → Get → redis.Nil → ErrNotFound. Memory
// 구현과 동일한 contract.
type RedisRegistry struct {
	rdb    redis.UniversalClient
	prefix string
	grace  time.Duration
	now    func() time.Time
}

// RedisRegistryOptions 는 RedisRegistry 생성 옵션.
type RedisRegistryOptions struct {
	// Prefix — 키 namespace (default "wtg:quoteid").
	Prefix string
	// Grace — ValidUntil 이후에도 record 를 유지하는 시간. last-look 시간 +
	// 네트워크 지연 + clock skew 여유. 0 이면 ValidUntil 도래 즉시 만료.
	Grace time.Duration
	// Now — 테스트용 시간 주입. nil 이면 time.Now.
	Now func() time.Time
}

// NewRedisRegistry — 호출자가 만든 UniversalClient 를 그대로 받는다.
// Close 는 호출자가 관리 (RedisStore 와 동일 컨벤션).
func NewRedisRegistry(rdb redis.UniversalClient, opt RedisRegistryOptions) *RedisRegistry {
	if opt.Prefix == "" {
		opt.Prefix = "wtg:quoteid"
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &RedisRegistry{
		rdb:    rdb,
		prefix: opt.Prefix,
		grace:  opt.Grace,
		now:    opt.Now,
	}
}

func (r *RedisRegistry) key(id QuoteID) string {
	return r.prefix + ":q:" + string(id)
}

// consumedKey — MarkConsumed 표시 키. SET NX 로 원자적 first-writer-wins.
func (r *RedisRegistry) consumedKey(id QuoteID) string {
	return r.prefix + ":c:" + string(id)
}

func (r *RedisRegistry) Put(ctx context.Context, rec Record) error {
	if rec.ValidUntil <= rec.IssuedAt {
		return ErrInvalidRecord
	}
	if rec.QuoteID == "" {
		return ErrInvalidRecord
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("quoteid: marshal: %w", err)
	}
	// TTL 계산: (ValidUntil - now) + grace. 음수가 될 수 있는 경계 케이스 방어.
	remaining := time.Unix(0, rec.ValidUntil).Sub(r.now()) + r.grace
	if remaining <= 0 {
		// 이미 만료된 record — 등록하지 않음 (Put 후 즉시 Get → Nil 이 normal).
		return nil
	}
	return r.rdb.Set(ctx, r.key(rec.QuoteID), body, remaining).Err()
}

func (r *RedisRegistry) Get(ctx context.Context, id QuoteID) (Record, error) {
	body, err := r.rdb.Get(ctx, r.key(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("quoteid: redis get: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return Record{}, fmt.Errorf("quoteid: unmarshal: %w", err)
	}
	return rec, nil
}

// Consumed — read-only 조회. consumedKey 가 존재하면 그 value (consumer) 반환.
func (r *RedisRegistry) Consumed(ctx context.Context, id QuoteID) (string, bool, error) {
	v, err := r.rdb.Get(ctx, r.consumedKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("quoteid: redis get consumed: %w", err)
	}
	return v, true, nil
}

// MarkConsumed — Redis SET NX 로 first-writer-wins.
//
// 흐름:
//
//	1. GET q:<id> → record (없으면 NOT_FOUND).
//	2. ValidUntil 도래 검사 → EXPIRED.
//	3. SET NX c:<id> consumer_id EX (ValidUntil-now + grace) — 성공이면 OK.
//	4. SET NX 실패면 GET c:<id> 로 먼저 잡은 consumer 회수 → ALREADY_CONSUMED.
//
// 두 mci-price 가 동시에 같은 QuoteID 를 처리해도 SET NX 의 atomicity 가
// 정확히 한 호출만 OK 보장 (Redis 단일 instance / master 기준).
func (r *RedisRegistry) MarkConsumed(ctx context.Context, id QuoteID, consumerID string) (ConsumeResult, error) {
	rec, err := r.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return ConsumeResult{Status: ConsumeNotFound}, nil
	}
	if err != nil {
		return ConsumeResult{}, err
	}
	if !rec.ValidAt(r.now()) {
		return ConsumeResult{Status: ConsumeExpired, Record: rec}, nil
	}
	ttl := time.Unix(0, rec.ValidUntil).Sub(r.now()) + r.grace
	if ttl <= 0 {
		ttl = r.grace
	}
	ok, err := r.rdb.SetNX(ctx, r.consumedKey(id), consumerID, ttl).Result()
	if err != nil {
		return ConsumeResult{}, fmt.Errorf("quoteid: setnx: %w", err)
	}
	if ok {
		return ConsumeResult{Status: ConsumeOK, Record: rec}, nil
	}
	// 누가 먼저 잡았는지 회수 — race 후 약간 늦게 도착해도 의미 있음.
	prev, gerr := r.rdb.Get(ctx, r.consumedKey(id)).Result()
	if gerr != nil && !errors.Is(gerr, redis.Nil) {
		return ConsumeResult{Status: ConsumeAlreadyDone, Record: rec}, nil
	}
	return ConsumeResult{Status: ConsumeAlreadyDone, Record: rec, ConsumedBy: prev}, nil
}
