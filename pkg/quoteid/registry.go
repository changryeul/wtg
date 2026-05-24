package quoteid

import (
	"context"
	"errors"
)

// Registry 는 QuoteID 의 발행 등록 / 검증 lookup 추상화.
//
//	mci-price.Publish 직후 → Registry.Put(record)
//	매칭 엔진 검증 시점   → Registry.Get(quoteID) → record (없으면 ErrNotFound)
//
// 만료 (ValidUntil 도래) 이후 호출자는 ErrNotFound 를 받는다 (Redis TTL /
// Memory lazy expiry).
//
// 운영 토폴로지:
//
//   - dev / 단위 테스트 : MemoryRegistry (in-process, sync.Map)
//   - 운영              : RedisRegistry (Redis Sentinel/Cluster, mci-price
//                         두 인스턴스 공유 — active-active 흐름의 single
//                         source of truth)
type Registry interface {
	// Put 은 발행된 quote 를 등록. ValidUntil 까지 (+ grace) 보존.
	// 동일 QuoteID 가 이미 있으면 overwrite — Generator 가 unique 발급이라
	// 정상 흐름에서는 발생하지 않지만 멱등성 보장.
	Put(ctx context.Context, rec Record) error

	// Get 은 QuoteID 의 Record 를 반환. 없으면 ErrNotFound.
	// 호출자 (매칭 엔진) 가 Record.ValidAt(now) 로 2차 정책 검증을 수행한다.
	Get(ctx context.Context, id QuoteID) (Record, error)
}

var (
	// ErrNotFound — Registry 에 QuoteID 가 없거나 이미 만료.
	ErrNotFound = errors.New("quoteid: not found")

	// ErrInvalidRecord — Put 시점에 Record 가 비정상 (예: ValidUntil <= IssuedAt).
	ErrInvalidRecord = errors.New("quoteid: invalid record")
)
