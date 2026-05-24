// QuoteValidationServer 는 매칭 엔진이 NewOrderSingle (FIX 'D') 의 QuoteID
// 를 검증하기 위해 호출하는 gRPC service.
//
// 설계 / 정책 분담은 docs/quoteid-validation-rfc.md 참조. WTG 는 존재 / 만료 /
// record echo 만 담당하고, side / slippage / last-look / 사용자 일치는 엔진
// 책임 (auth 위임 원칙).
package price

import (
	"context"
	"log/slog"
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

	// engineAllowlist — atomic.Pointer 로 hot swap 가능. nil 이면 RBAC 비활성.
	// 정적 설정 (SetEngineAllowlist) 과 동적 (etcd watcher) 모두 같은 필드에
	// store — 둘 중 하나만 활성 권장 (마지막 set 이 이긴다).
	engineAllowlist atomic.Pointer[map[string]struct{}]

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

// SetEngineAllowlist — 허용 engine_id 슬라이스 → 내부 set 으로. 빈 입력이면
// RBAC 비활성 (clear). atomic — 운영 중 hot swap 가능.
func (s *QuoteValidationServer) SetEngineAllowlist(engines []string) {
	if len(engines) == 0 {
		s.engineAllowlist.Store(nil)
		return
	}
	m := make(map[string]struct{}, len(engines))
	for _, e := range engines {
		if e != "" {
			m[e] = struct{}{}
		}
	}
	if len(m) == 0 {
		s.engineAllowlist.Store(nil)
		return
	}
	s.engineAllowlist.Store(&m)
}

// SetEngineAllowlistMap — etcd watcher 가 직접 호출. nil/빈 map 이면 RBAC 비활성.
func (s *QuoteValidationServer) SetEngineAllowlistMap(m map[string]struct{}) {
	if len(m) == 0 {
		s.engineAllowlist.Store(nil)
		return
	}
	// 호출자가 같은 map 을 mutate 하지 않도록 copy.
	cp := make(map[string]struct{}, len(m))
	for k := range m {
		cp[k] = struct{}{}
	}
	s.engineAllowlist.Store(&cp)
}

// checkEngine — allowlist 비활성이면 true. 활성이면 engineID 가 set 에
// 있을 때만 true. atomic.Pointer.Load 라 lock 없음.
func (s *QuoteValidationServer) checkEngine(engineID string) bool {
	mp := s.engineAllowlist.Load()
	if mp == nil {
		return true
	}
	_, ok := (*mp)[engineID]
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
//
// v1.11+ — Registry.Lookup 으로 record + consumed 를 atomic 1 RTT 조회.
// 이전 (v1.6–v1.10) 의 Get + Consumed 두 RTT 경로 제거.
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

	lr, err := s.registry.Lookup(ctx, quoteid.QuoteID(qid))
	if err != nil {
		s.callsInternal.Add(1)
		s.logger.Warn("quote validation internal error",
			slog.String("quote_id", qid),
			slog.String("engine_id", req.GetEngineId()),
			slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}
	return s.lookupToValidateResponse(lr, qid, req.GetEngineId()), nil
}

// lookupToValidateResponse — Lookup 결과를 Validate 응답 + 카운터 매핑.
// BatchValidate 가 동일 경로 재사용.
func (s *QuoteValidationServer) lookupToValidateResponse(lr quoteid.LookupResult, qid, engineID string) *wtgpb.ValidateResponse {
	if !lr.Found {
		s.callsNotFound.Add(1)
		s.logger.Info("quote validation",
			slog.String("quote_id", qid),
			slog.String("engine_id", engineID),
			slog.String("status", "NOT_FOUND"))
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "quote_id not found",
		}
	}
	if !lr.Record.ValidAt(s.now()) {
		s.callsExpired.Add(1)
		s.logger.Info("quote validation",
			slog.String("quote_id", qid),
			slog.String("engine_id", engineID),
			slog.String("status", "EXPIRED"))
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_EXPIRED,
			Record:       recordToProto(lr.Record),
			OrdRejReason: ordRejReasonExpired,
			RejectText:   "quote_id expired",
		}
	}
	if lr.Consumed {
		s.callsConsumed.Add(1)
		s.logger.Info("quote validation",
			slog.String("quote_id", qid),
			slog.String("engine_id", engineID),
			slog.String("status", "ALREADY_CONSUMED"))
		return &wtgpb.ValidateResponse{
			Status:       wtgpb.ValidationStatus_ALREADY_CONSUMED,
			Record:       recordToProto(lr.Record),
			OrdRejReason: ordRejReasonDuplicate,
			RejectText:   "quote_id already consumed",
		}
	}
	s.callsOK.Add(1)
	return &wtgpb.ValidateResponse{
		Status: wtgpb.ValidationStatus_OK,
		Record: recordToProto(lr.Record),
	}
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

// BatchValidate — Registry.LookupMany 로 pipeline 호출. 1 RTT (direct/sentinel)
// 또는 slot 별 1 RTT (cluster). 이전 (v1.7–v1.10) 의 goroutine fan-out 대비
// connection pool churn / goroutine overhead 제거.
//
// callsTotal 카운터는 per-item 누적 — 단일 Validate 호출과 정합.
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

	// 빈 quote_id (= 미지정) 도 NOT_FOUND 로 처리 — 단일 Validate 와 동일.
	// LookupMany 에 빈 문자열을 그대로 넘기면 Redis 에서 "{}:q" 키를 조회 →
	// 보통 미존재 → Found=false. lookupToValidateResponse 가 NOT_FOUND 매핑.
	regIDs := make([]quoteid.QuoteID, len(ids))
	for i, id := range ids {
		regIDs[i] = quoteid.QuoteID(id)
	}
	lookups, err := s.registry.LookupMany(ctx, regIDs)
	if err != nil {
		s.callsInternal.Add(1)
		s.logger.Warn("BatchValidate registry error",
			slog.String("engine_id", req.GetEngineId()),
			slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}
	results := make([]*wtgpb.ValidateResponse, len(ids))
	for i, lr := range lookups {
		s.callsTotal.Add(1) // single Validate 와 정합 — per-item.
		results[i] = s.lookupToValidateResponse(lr, ids[i], req.GetEngineId())
	}
	return &wtgpb.BatchValidateResponse{Results: results}, nil
}

// BatchMarkConsumed — Registry.MarkConsumedMany 로 1 connection grab + 1 RTT
// (direct/sentinel) 또는 slot 당 1 RTT (cluster). Per-item atomic 은 동일.
//
// 일부 OK 일부 ALREADY_CONSUMED 같은 혼재 결과 가능 (FIX NewOrderList 의 일부
// leg 만 다른 주문이 잡은 경우). 호출자가 per-item 분기로 일부 fill / 일부
// reject.
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

	// Registry.MarkConsumedMany 로 위임 — Memory 는 단일 mutex 안에서 직렬,
	// Redis 는 Pipeline 으로 묶음 송신.
	reqs := make([]quoteid.ConsumeRequest, len(items))
	for i, it := range items {
		reqs[i] = quoteid.ConsumeRequest{
			QuoteID:    quoteid.QuoteID(it.GetQuoteId()),
			ConsumerID: it.GetConsumerId(),
		}
	}
	regResults, err := s.registry.MarkConsumedMany(ctx, reqs)
	if err != nil {
		s.consumeInternal.Add(1)
		s.logger.Warn("BatchMarkConsumed registry error",
			slog.String("engine_id", req.GetEngineId()),
			slog.Any("error", err))
		// 전체 실패도 per-item STATUS_UNSPECIFIED 로 회피 가능하지만, 명백한
		// registry-wide error 는 grpc Internal 로 전파해 caller retry.
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}

	results := make([]*wtgpb.MarkConsumedResponse, len(items))
	for i, r := range regResults {
		results[i] = consumeResultToProto(r)
		// per-item 카운터 누적 — MarkConsumed 단일 핸들러와 동일.
		switch r.Status {
		case quoteid.ConsumeOK:
			s.consumeOK.Add(1)
		case quoteid.ConsumeAlreadyDone:
			s.consumeAlready.Add(1)
		case quoteid.ConsumeNotFound:
			s.consumeNotFound.Add(1)
		case quoteid.ConsumeExpired:
			s.consumeExpired.Add(1)
		}
		// consumeTotal 도 per-item — single MarkConsumed 와 정합.
		s.consumeTotal.Add(1)
	}
	return &wtgpb.BatchMarkConsumedResponse{Results: results}, nil
}

// consumeResultToProto — Registry.ConsumeResult 를 proto MarkConsumedResponse 로.
func consumeResultToProto(r quoteid.ConsumeResult) *wtgpb.MarkConsumedResponse {
	resp := &wtgpb.MarkConsumedResponse{}
	switch r.Status {
	case quoteid.ConsumeOK:
		resp.Status = wtgpb.ValidationStatus_OK
		resp.Record = recordToProto(r.Record)
	case quoteid.ConsumeAlreadyDone:
		resp.Status = wtgpb.ValidationStatus_ALREADY_CONSUMED
		resp.Record = recordToProto(r.Record)
		resp.ConsumedBy = r.ConsumedBy
		resp.OrdRejReason = ordRejReasonDuplicate
		resp.RejectText = "quote_id already consumed"
	case quoteid.ConsumeNotFound:
		resp.Status = wtgpb.ValidationStatus_NOT_FOUND
		resp.OrdRejReason = ordRejReasonNotFound
		resp.RejectText = "quote_id not found"
	case quoteid.ConsumeExpired:
		resp.Status = wtgpb.ValidationStatus_EXPIRED
		resp.Record = recordToProto(r.Record)
		resp.OrdRejReason = ordRejReasonExpired
		resp.RejectText = "quote_id expired"
	default:
		resp.Status = wtgpb.ValidationStatus_STATUS_UNSPECIFIED
		resp.RejectText = "internal error"
	}
	return resp
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
