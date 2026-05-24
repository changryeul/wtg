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

	// MarkConsumed 는 QuoteID 를 "사용 완료" 로 원자적으로 표시한다. 두 호출자가
	// 동시에 같은 QuoteID 에 대해 호출해도 정확히 한 명만 ConsumeOK 를 받는다
	// (FX Global Code Principle 17 "use only once" 보장).
	//
	// 반환:
	//   ConsumeOK             — 이 호출이 처음 표시 + record echo.
	//   ConsumeAlreadyDone    — 이미 다른 호출이 표시함 + 그 consumer 식별자.
	//   ConsumeNotFound       — record 없음.
	//   ConsumeExpired        — ValidUntil 도래 — 표시했더라도 거래 불가.
	MarkConsumed(ctx context.Context, id QuoteID, consumerID string) (ConsumeResult, error)

	// Consumed 는 QuoteID 가 이미 사용 완료되었는지 read-only 로 조회.
	// 반환: 첫 인수는 먼저 표시한 consumer (없으면 빈 문자열), 두 번째는 표시 여부.
	// Validate RPC 가 ALREADY_CONSUMED 분기를 위해 호출 — atomic write 없는 cheap path.
	Consumed(ctx context.Context, id QuoteID) (string, bool, error)

	// MarkConsumedMany — N 항목을 한 번에 표시. 각 항목은 개별 atomic — 같은
	// QuoteID 에 동시 호출 시 정확히 한 명만 ConsumeOK.
	//
	// 구현 :
	//   - Memory : 단일 mutex 안에서 직렬 처리 → batch 전체가 일관 snapshot.
	//   - Redis  : 파이프라인으로 EvalSha 묶음 송신 → 1 connection grab.
	//              Cluster 에서는 slot 별 자동 라우팅 (redis-go ClusterClient).
	//
	// 결과는 입력 순서와 동일.
	MarkConsumedMany(ctx context.Context, reqs []ConsumeRequest) ([]ConsumeResult, error)
}

// ConsumeRequest — MarkConsumedMany 단일 항목.
type ConsumeRequest struct {
	QuoteID    QuoteID
	ConsumerID string
}

// ConsumeResult — MarkConsumed 결과.
type ConsumeResult struct {
	Status     ConsumeStatus
	Record     Record // ConsumeOK / ConsumeAlreadyDone 에서 채워짐
	ConsumedBy string // ConsumeAlreadyDone 일 때 먼저 표시한 consumer
}

type ConsumeStatus int

const (
	ConsumeStatusUnknown ConsumeStatus = iota
	ConsumeOK
	ConsumeAlreadyDone
	ConsumeNotFound
	ConsumeExpired
)

func (s ConsumeStatus) String() string {
	switch s {
	case ConsumeOK:
		return "OK"
	case ConsumeAlreadyDone:
		return "ALREADY_CONSUMED"
	case ConsumeNotFound:
		return "NOT_FOUND"
	case ConsumeExpired:
		return "EXPIRED"
	default:
		return "UNKNOWN"
	}
}

var (
	// ErrNotFound — Registry 에 QuoteID 가 없거나 이미 만료.
	ErrNotFound = errors.New("quoteid: not found")

	// ErrInvalidRecord — Put 시점에 Record 가 비정상 (예: ValidUntil <= IssuedAt).
	ErrInvalidRecord = errors.New("quoteid: invalid record")
)
