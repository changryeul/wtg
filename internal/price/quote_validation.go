// QuoteValidationServer 는 매칭 엔진이 NewOrderSingle (FIX 'D') 의 QuoteID
// 를 검증하기 위해 호출하는 gRPC service.
//
// 설계 / 정책 분담은 docs/quoteid-validation-rfc.md 참조. WTG 는 존재 / 만료 /
// record echo 만 담당하고, side / slippage / last-look / 사용자 일치는 엔진
// 책임 (auth 위임 원칙).
package price

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/winwaysystems/wtg/pkg/quoteid"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// MaxBatchValidateSize — BatchValidate RPC 단일 호출 상한. 운영 abuse / 과도한
// goroutine 폭발 방어. 1000 = FIX NewOrderList 의 일반적 cap.
const MaxBatchValidateSize = 1000

// MaxBatchConsumeSize — BatchMarkConsumed 상한. 같은 이유로 1000.
const MaxBatchConsumeSize = 1000

// FIX 4.4 OrdRejReason (tag 103) 매핑 — RFC §4.3.
const (
	ordRejReasonNotFound int32 = 5  // Unknown order
	ordRejReasonExpired  int32 = 13 // Stale order
	ordRejReasonDuplicate int32 = 6 // Duplicate order — ALREADY_CONSUMED 매핑
)

// QuoteValidationServer 는 wtgpb.QuoteValidationServiceServer 구현.
// pkg/quoteid.Registry 의 thin wrapper — 별도 비즈니스 로직 없음.
type QuoteValidationServer struct {
	wtgpb.UnimplementedQuoteValidationServiceServer

	registry quoteid.Registry
	logger   *slog.Logger
	now      func() time.Time

	// engineAllowlist — 비어있으면 RBAC 비활성. 채워져 있으면 모든 RPC 가
	// engine_id 가 set 안에 있어야 통과 (그렇지 않으면 PermissionDenied).
	engineAllowlist map[string]struct{}

	// 누적 카운터 — `/v1/stats` 노출 또는 grpc interceptor 메트릭 대안.
	callsTotal        atomic.Uint64
	callsOK           atomic.Uint64
	callsNotFound     atomic.Uint64
	callsExpired      atomic.Uint64
	callsConsumed     atomic.Uint64 // Validate 가 ALREADY_CONSUMED 반환
	callsInternal     atomic.Uint64
	consumeTotal      atomic.Uint64
	consumeOK         atomic.Uint64
	consumeAlready    atomic.Uint64
	consumeNotFound   atomic.Uint64
	consumeExpired    atomic.Uint64
	consumeInternal   atomic.Uint64
	batchTotal        atomic.Uint64 // BatchValidate RPC 누적
	batchItems        atomic.Uint64 // 처리된 quote_id 총합
	batchConsumeTotal atomic.Uint64 // BatchMarkConsumed RPC 누적
	batchConsumeItems atomic.Uint64 // 처리된 표시 항목 총합
	deniedEngine      atomic.Uint64 // engine_id allowlist 거절
}

// NewQuoteValidationServer — Registry 가 nil 이면 panic. logger 가 nil 이면
// slog.Default.
func NewQuoteValidationServer(registry quoteid.Registry, logger *slog.Logger) *QuoteValidationServer {
	if registry == nil {
		panic("price: QuoteValidationServer 에 Registry 필수")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &QuoteValidationServer{
		registry: registry,
		logger:   logger,
		now:      time.Now,
	}
}

// SetEngineAllowlist — 허용 engine_id 목록 등록. 빈 슬라이스면 RBAC 비활성.
// 호출은 Start 전에 1회 (atomic 갱신은 v2 후속).
func (s *QuoteValidationServer) SetEngineAllowlist(engines []string) {
	if len(engines) == 0 {
		s.engineAllowlist = nil
		return
	}
	m := make(map[string]struct{}, len(engines))
	for _, e := range engines {
		if e != "" {
			m[e] = struct{}{}
		}
	}
	if len(m) == 0 {
		s.engineAllowlist = nil
		return
	}
	s.engineAllowlist = m
}

// checkEngine — allowlist 비활성이면 true. 활성이면 engineID 가 set 에
// 있을 때만 true.
func (s *QuoteValidationServer) checkEngine(engineID string) bool {
	if s.engineAllowlist == nil {
		return true
	}
	_, ok := s.engineAllowlist[engineID]
	return ok
}

// permissionDenied — 모든 핸들러가 RBAC 거절 시 공통으로 호출.
func (s *QuoteValidationServer) permissionDenied(qid, engineID, op string) error {
	s.deniedEngine.Add(1)
	s.logger.Warn("quote validation: engine_id 거부",
		slog.String("op", op),
		slog.String("engine_id", engineID),
		slog.String("quote_id", qid))
	return status.Errorf(codes.PermissionDenied,
		"engine_id %q not in allowlist", engineID)
}

// SetNow — 테스트용 시간 주입.
func (s *QuoteValidationServer) SetNow(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

// Validate 는 RFC §4.1 의 ValidateRequest 를 처리.
func (s *QuoteValidationServer) Validate(ctx context.Context, req *wtgpb.ValidateRequest) (*wtgpb.ValidateResponse, error) {
	s.callsTotal.Add(1)

	if !s.checkEngine(req.GetEngineId()) {
		return nil, s.permissionDenied(req.GetQuoteId(), req.GetEngineId(), "Validate")
	}

	qid := req.GetQuoteId()
	if qid == "" {
		s.callsNotFound.Add(1)
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "quote_id required",
		}, nil
	}

	rec, err := s.registry.Get(ctx, quoteid.QuoteID(qid))
	if err != nil {
		if errors.Is(err, quoteid.ErrNotFound) {
			s.callsNotFound.Add(1)
			s.logger.Info("quote validation",
				slog.String("quote_id", qid),
				slog.String("engine_id", req.GetEngineId()),
				slog.String("status", "NOT_FOUND"))
			return &wtgpb.ValidateResponse{
				Status:       wtgpb.ValidationStatus_NOT_FOUND,
				OrdRejReason: ordRejReasonNotFound,
				RejectText:   "quote_id not found",
			}, nil
		}
		s.callsInternal.Add(1)
		s.logger.Warn("quote validation internal error",
			slog.String("quote_id", qid),
			slog.String("engine_id", req.GetEngineId()),
			slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}

	// Registry 가 grace 적용 후 GC 했더라도, 호출 시점 wallclock 으로 한 번 더
	// ValidUntil 도래 여부 검사 (grace 안에 있지만 ValidUntil 후인 경우 EXPIRED).
	if !rec.ValidAt(s.now()) {
		s.callsExpired.Add(1)
		s.logger.Info("quote validation",
			slog.String("quote_id", qid),
			slog.String("engine_id", req.GetEngineId()),
			slog.String("status", "EXPIRED"))
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_EXPIRED,
			Record:       recordToProto(rec),
			OrdRejReason: ordRejReasonExpired,
			RejectText:   "quote_id expired",
		}, nil
	}

	// ALREADY_CONSUMED — 이미 다른 주문이 사용. (FX Global Code Principle 17
	// "use only once".) Consumed 는 read-only — atomic write 는 MarkConsumed.
	if _, consumed, err := s.registry.Consumed(ctx, quoteid.QuoteID(qid)); err == nil && consumed {
		s.callsConsumed.Add(1)
		s.logger.Info("quote validation",
			slog.String("quote_id", qid),
			slog.String("engine_id", req.GetEngineId()),
			slog.String("status", "ALREADY_CONSUMED"))
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_ALREADY_CONSUMED,
			Record:       recordToProto(rec),
			OrdRejReason: ordRejReasonDuplicate,
			RejectText:   "quote_id already consumed",
		}, nil
	}

	s.callsOK.Add(1)
	return &wtgpb.ValidateResponse{
		Status: wtgpb.ValidationStatus_OK,
		Record: recordToProto(rec),
	}, nil
}

// MarkConsumed 는 RFC §4.1 의 두 번째 RPC. 동시 호출 atomic 보장은
// Registry 의 책임 (Memory mutex / Redis SET NX).
func (s *QuoteValidationServer) MarkConsumed(ctx context.Context, req *wtgpb.MarkConsumedRequest) (*wtgpb.MarkConsumedResponse, error) {
	s.consumeTotal.Add(1)

	if !s.checkEngine(req.GetEngineId()) {
		return nil, s.permissionDenied(req.GetQuoteId(), req.GetEngineId(), "MarkConsumed")
	}

	qid := req.GetQuoteId()
	if qid == "" {
		s.consumeNotFound.Add(1)
		return &wtgpb.MarkConsumedResponse{
			Status:       wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "quote_id required",
		}, nil
	}

	result, err := s.registry.MarkConsumed(ctx, quoteid.QuoteID(qid), req.GetConsumerId())
	if err != nil {
		s.consumeInternal.Add(1)
		s.logger.Warn("mark consumed internal error",
			slog.String("quote_id", qid),
			slog.String("engine_id", req.GetEngineId()),
			slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}

	resp := &wtgpb.MarkConsumedResponse{}
	switch result.Status {
	case quoteid.ConsumeOK:
		s.consumeOK.Add(1)
		resp.Status = wtgpb.ValidationStatus_OK
		resp.Record = recordToProto(result.Record)
	case quoteid.ConsumeAlreadyDone:
		s.consumeAlready.Add(1)
		resp.Status = wtgpb.ValidationStatus_ALREADY_CONSUMED
		resp.Record = recordToProto(result.Record)
		resp.ConsumedBy = result.ConsumedBy
		resp.OrdRejReason = ordRejReasonDuplicate
		resp.RejectText = "quote_id already consumed"
		s.logger.Info("mark consumed conflict",
			slog.String("quote_id", qid),
			slog.String("requested_by", req.GetConsumerId()),
			slog.String("consumed_by", result.ConsumedBy))
	case quoteid.ConsumeNotFound:
		s.consumeNotFound.Add(1)
		resp.Status = wtgpb.ValidationStatus_NOT_FOUND
		resp.OrdRejReason = ordRejReasonNotFound
		resp.RejectText = "quote_id not found"
	case quoteid.ConsumeExpired:
		s.consumeExpired.Add(1)
		resp.Status = wtgpb.ValidationStatus_EXPIRED
		resp.Record = recordToProto(result.Record)
		resp.OrdRejReason = ordRejReasonExpired
		resp.RejectText = "quote_id expired"
	default:
		s.consumeInternal.Add(1)
		resp.Status = wtgpb.ValidationStatus_STATUS_UNSPECIFIED
		resp.RejectText = "unknown ConsumeStatus"
	}
	return resp, nil
}

// BatchValidate — 다건 QuoteID 를 병렬로 검증. 결과는 입력과 같은 순서의
// 배열로 반환. 빈 배열이면 빈 결과. 상한 초과면 InvalidArgument.
//
// 내부적으로 N goroutine fan-out — 각 항목이 자체 Registry 호출. 100건 batch
// 가 단일 Validate 와 비슷한 wallclock (Redis round-trip 병렬화).
//
// 한 항목이 Registry internal error 를 만나도 batch 전체는 실패 안 함 — 해당
// index 의 ValidateResponse 만 STATUS_UNSPECIFIED + reject_text 로 표시.
// 호출자가 per-item 분기.
func (s *QuoteValidationServer) BatchValidate(ctx context.Context, req *wtgpb.BatchValidateRequest) (*wtgpb.BatchValidateResponse, error) {
	s.batchTotal.Add(1)
	if !s.checkEngine(req.GetEngineId()) {
		return nil, s.permissionDenied("", req.GetEngineId(), "BatchValidate")
	}
	ids := req.GetQuoteIds()
	if len(ids) == 0 {
		return &wtgpb.BatchValidateResponse{}, nil
	}
	if len(ids) > MaxBatchValidateSize {
		return nil, status.Errorf(codes.InvalidArgument,
			"batch size %d exceeds max %d", len(ids), MaxBatchValidateSize)
	}
	s.batchItems.Add(uint64(len(ids)))

	results := make([]*wtgpb.ValidateResponse, len(ids))
	var wg sync.WaitGroup
	wg.Add(len(ids))
	for i := range ids {
		i := i
		go func() {
			defer wg.Done()
			resp, err := s.Validate(ctx, &wtgpb.ValidateRequest{
				QuoteId:    ids[i],
				EngineId:   req.GetEngineId(),
				TsUnixNano: req.GetTsUnixNano(),
			})
			if err != nil || resp == nil {
				results[i] = &wtgpb.ValidateResponse{
					Status:     wtgpb.ValidationStatus_STATUS_UNSPECIFIED,
					RejectText: "internal error",
				}
				return
			}
			results[i] = resp
		}()
	}
	wg.Wait()
	return &wtgpb.BatchValidateResponse{Results: results}, nil
}

// BatchMarkConsumed — 다건 (quote_id, consumer_id) 표시. 각 항목은 독립
// atomic — Memory mutex / Redis SET NX 가 per-key 보장.
//
// 일부 OK 일부 ALREADY_CONSUMED 같은 혼재 결과 가능 (FIX NewOrderList 의 일부
// leg 만 다른 주문이 잡은 경우). 호출자가 per-item 분기로 일부 fill / 일부
// reject.
//
// goroutine fan-out 으로 Redis round-trip 병렬화 — 100건 ≈ 단일 MarkConsumed.
func (s *QuoteValidationServer) BatchMarkConsumed(ctx context.Context, req *wtgpb.BatchMarkConsumedRequest) (*wtgpb.BatchMarkConsumedResponse, error) {
	s.batchConsumeTotal.Add(1)
	if !s.checkEngine(req.GetEngineId()) {
		return nil, s.permissionDenied("", req.GetEngineId(), "BatchMarkConsumed")
	}
	items := req.GetItems()
	if len(items) == 0 {
		return &wtgpb.BatchMarkConsumedResponse{}, nil
	}
	if len(items) > MaxBatchConsumeSize {
		return nil, status.Errorf(codes.InvalidArgument,
			"batch size %d exceeds max %d", len(items), MaxBatchConsumeSize)
	}
	s.batchConsumeItems.Add(uint64(len(items)))

	results := make([]*wtgpb.MarkConsumedResponse, len(items))
	var wg sync.WaitGroup
	wg.Add(len(items))
	for i := range items {
		i := i
		go func() {
			defer wg.Done()
			resp, err := s.MarkConsumed(ctx, &wtgpb.MarkConsumedRequest{
				QuoteId:    items[i].GetQuoteId(),
				ConsumerId: items[i].GetConsumerId(),
				EngineId:   req.GetEngineId(),
				TsUnixNano: req.GetTsUnixNano(),
			})
			if err != nil || resp == nil {
				results[i] = &wtgpb.MarkConsumedResponse{
					Status:     wtgpb.ValidationStatus_STATUS_UNSPECIFIED,
					RejectText: "internal error",
				}
				return
			}
			results[i] = resp
		}()
	}
	wg.Wait()
	return &wtgpb.BatchMarkConsumedResponse{Results: results}, nil
}

// QuoteValidationStats — 누적 카운터 snapshot. 운영 모니터링용.
type QuoteValidationStats struct {
	// Validate RPC 카운터.
	Total    uint64 `json:"total"`
	OK       uint64 `json:"ok"`
	NotFound uint64 `json:"not_found"`
	Expired  uint64 `json:"expired"`
	Consumed uint64 `json:"already_consumed"`
	Internal uint64 `json:"internal"`
	// MarkConsumed RPC 카운터.
	ConsumeTotal    uint64 `json:"consume_total"`
	ConsumeOK       uint64 `json:"consume_ok"`
	ConsumeAlready  uint64 `json:"consume_already"`
	ConsumeNotFound uint64 `json:"consume_not_found"`
	ConsumeExpired  uint64 `json:"consume_expired"`
	ConsumeInternal uint64 `json:"consume_internal"`
	// BatchValidate RPC 카운터.
	BatchTotal uint64 `json:"batch_total"`
	BatchItems uint64 `json:"batch_items"`
	// BatchMarkConsumed RPC 카운터.
	BatchConsumeTotal uint64 `json:"batch_consume_total"`
	BatchConsumeItems uint64 `json:"batch_consume_items"`
	// RBAC 카운터.
	DeniedEngine uint64 `json:"denied_engine"`
}

func (s *QuoteValidationServer) Stats() QuoteValidationStats {
	return QuoteValidationStats{
		Total:           s.callsTotal.Load(),
		OK:              s.callsOK.Load(),
		NotFound:        s.callsNotFound.Load(),
		Expired:         s.callsExpired.Load(),
		Consumed:        s.callsConsumed.Load(),
		Internal:        s.callsInternal.Load(),
		ConsumeTotal:    s.consumeTotal.Load(),
		ConsumeOK:       s.consumeOK.Load(),
		ConsumeAlready:  s.consumeAlready.Load(),
		ConsumeNotFound: s.consumeNotFound.Load(),
		ConsumeExpired:  s.consumeExpired.Load(),
		ConsumeInternal: s.consumeInternal.Load(),
		BatchTotal:        s.batchTotal.Load(),
		BatchItems:        s.batchItems.Load(),
		BatchConsumeTotal: s.batchConsumeTotal.Load(),
		BatchConsumeItems: s.batchConsumeItems.Load(),
		DeniedEngine:      s.deniedEngine.Load(),
	}
}

// recordToProto — pkg/quoteid.Record → proto.
func recordToProto(r quoteid.Record) *wtgpb.QuoteRecord {
	return &wtgpb.QuoteRecord{
		QuoteId:            string(r.QuoteID),
		Pair:               string(r.Pair),
		Channel:            string(r.Profile.Channel),
		Site:               string(r.Profile.Site),
		Tier:               string(r.Profile.Tier),
		Tenor:              r.Tenor,
		Bid:                r.Bid,
		Ask:                r.Ask,
		IssuedUnixNano:     r.IssuedAt,
		ValidUntilUnixNano: r.ValidUntil,
		Sequence:           r.Sequence,
		Issuer:             r.Issuer,
	}
}
