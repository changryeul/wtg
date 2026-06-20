package quoteid

import (
	"context"
	"errors"
)

// Phase S3-b — swap (near+far 2-leg) 거래용 보조 인덱스.
//
// 매매 AP 가 swap_id 한 개로 두 leg 의 quote_id 를 한 번에 lookup 할 수 있도록
// Registry 옆에 별도 인덱스를 유지한다. 본 인터페이스는 core Registry 와 분리 —
// 미지원 Registry 구현 (예: Redis 1차) 에서도 spot/forward 는 동작 보장.
//
// 호출 패턴:
//
//	mci-price.SwapLockHandler:
//	    Reg.Put(near_record)
//	    Reg.Put(far_record)
//	    SwapIdx.PutSwap({swap_id, near_id, far_id, ...})
//	    부분 실패 → SwapIdx.Delete(near_id), Delete(far_id), DeleteSwap(swap_id)
//
//	매매 AP (Validate RPC):
//	    SwapIdx.GetSwap(swap_id) → {near_id, far_id}
//	    Reg.LookupMany([near_id, far_id]) → 두 Record 동시 검증

// SwapRecord — swap_id 인덱스 entry. leg 의 가격/마진은 leg quote_id 의
// Record 에 그대로 들어있으므로 본 record 는 묶음 정보만.
type SwapRecord struct {
	SwapID     string  `json:"swap_id"`
	NearID     QuoteID `json:"near_id"`
	FarID      QuoteID `json:"far_id"`
	IssuedAt   int64   `json:"issued_unix_nano"`
	ValidUntil int64   `json:"valid_until_unix_nano"`
	Issuer     string  `json:"issuer"`
}

// SwapIndex — Registry 의 optional 확장. swap_lock handler 는 type assertion
// 으로 가용성 확인 후 사용. 미구현이면 swap endpoint 자체를 등록하지 않는다
// (운영 시 명확한 startup gate).
type SwapIndex interface {
	// PutSwap — swap_id 인덱스 등록. 동일 SwapID 재호출은 overwrite.
	PutSwap(ctx context.Context, rec SwapRecord) error

	// GetSwap — swap_id 로 인덱스 조회. 미존재 시 ErrSwapNotFound.
	GetSwap(ctx context.Context, swapID string) (SwapRecord, error)

	// Delete — leg quote_id 의 Record revoke. partial-failure 복구용. 미존재는
	// nil — 호출자가 best-effort 로 안전하게 N번 호출 가능 (idempotent).
	Delete(ctx context.Context, id QuoteID) error

	// DeleteSwap — swap_id 인덱스 entry 삭제. 미존재는 nil.
	DeleteSwap(ctx context.Context, swapID string) error
}

// ErrSwapNotFound — SwapIndex.GetSwap 가 swap_id 를 못 찾았을 때.
var ErrSwapNotFound = errors.New("quoteid: swap not found")
