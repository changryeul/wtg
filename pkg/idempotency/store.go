// Package idempotency 는 매매 transaction 의 중복 제출 방지용 (Idempotency-Key
// 헤더 기반).
//
// 흐름:
//
//  1. handler 가 Idempotency-Key 헤더 추출 → key = usid + "|" + headerValue
//  2. Reserve(key, sha256(body)) 호출:
//     - Miss     : 우리가 leader — broker 호출 진행, 결과를 Commit
//     - Cached   : 이전 응답 반환 (broker call skip)
//     - InFlight : 동시 중복 (같은 body, 다른 호출 진행 중) — 409
//     - Conflict : 같은 key 에 다른 body — 409
//  3. leader path 에서 broker call 실패 시 Rollback (캐시 X, 재시도 가능)
//
// store backend:
//   - Memory : 단일 인스턴스 / dev. lazy expire.
//   - Redis  : 다중 인스턴스 공유 (후속).
package idempotency

import (
	"context"
	"errors"
	"time"
)

// Status — Reserve 반환 상태.
type Status int

const (
	StatusMiss     Status = iota // 신규 reservation 성공 (leader)
	StatusCached                 // 이전 응답 캐시 hit
	StatusInFlight               // 같은 key + 같은 body 의 in-flight reservation
	StatusConflict               // 같은 key + 다른 body
)

// CachedReply — 성공 reply 의 캐시 본문.
type CachedReply struct {
	StatusCode int    `json:"status_code"`
	Body       []byte `json:"body"`
}

// 표준 에러.
var (
	// ErrConflict — 같은 Idempotency-Key 에 다른 request body. 409 매핑.
	ErrConflict = errors.New("idempotency: body hash conflict")

	// ErrInFlight — 같은 key + 같은 body 가 다른 goroutine/요청에서 처리 중.
	// 호출자가 잠시 후 재시도하면 Cached 로 응답 가능.
	ErrInFlight = errors.New("idempotency: another request in-flight")
)

// Store — Idempotency reservation + reply 캐시.
//
// 단일 호출자 흐름:
//
//	st, cached, err := store.Reserve(ctx, key, hash)
//	switch st {
//	case StatusCached:
//	    return cached
//	case StatusMiss:
//	    reply, broker_err := broker.Call(...)
//	    if broker_err != nil {
//	        store.Rollback(ctx, key)
//	        return broker_err
//	    }
//	    store.Commit(ctx, key, replyToCache(reply))
//	    return reply
//	case StatusInFlight, StatusConflict:
//	    return 409
//	}
type Store interface {
	// Reserve — key 가 없으면 in-flight reservation 만들고 StatusMiss.
	// 있으면 상태 + (Cached 인 경우) reply.
	Reserve(ctx context.Context, key string, bodyHash [32]byte) (Status, *CachedReply, error)

	// Commit — reservation 을 캐시된 reply 로 확정. TTL 후 자동 만료.
	Commit(ctx context.Context, key string, reply *CachedReply) error

	// Rollback — reservation 해제 (broker call 실패 시). 재시도 가능.
	Rollback(ctx context.Context, key string) error

	// Close — backend 리소스 정리.
	Close() error
}

// MakeKey — handler 가 (usid, header) 로 store key 를 합성하는 헬퍼.
// 사용자 격리 — 다른 user 의 같은 header 가 충돌 안 함.
func MakeKey(usid, header string) string {
	return usid + "|" + header
}

// Options — store 공통 옵션.
type Options struct {
	// TTL — reservation / cached reply 만료. 0 이면 5분 default.
	TTL time.Duration
}

func (o Options) effectiveTTL() time.Duration {
	if o.TTL <= 0 {
		return 5 * time.Minute
	}
	return o.TTL
}
