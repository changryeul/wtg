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
