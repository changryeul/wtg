// Phase S3-c — ValidateSwap / ConsumeSwap RPC 핸들러.
//
// swap_id 1개로 두 leg quote_id 를 동시 검증/표시한다.
// 매매 AP (mymq) 가 FX swap 거래 시 본 RPC 를 호출 — 단일 NewOrderSingle 시
// Validate / MarkConsumed 와 동일한 흐름.
//
// 흐름 (자세히 docs/swap-trade-spec.md §6):
//
//	1) ValidateSwap(swap_id) → SwapIndex.GetSwap → LookupMany(near, far)
//	   AND 정책 — 둘 다 OK 여야 OK. 어느 한 쪽 실패면 그 reason 의 status 반환.
//
//	2) ConsumeSwap(swap_id, consumer_id) → 사전 ValidateSwap 통과 시
//	   MarkConsumedMany([near, far], consumer_id) 호출.
//	   race: 두 mci-price 가 동시에 같은 swap_id 받아도 Registry 의 SET NX 가
//	   per-leg atomic 보장 — 한 leg 라도 ALREADY 면 swap_status=ALREADY 로 회수.

package price

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/winwaysystems/wtg/pkg/quoteid"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// SetSwapIndex — QuoteValidationServer 에 swap_id 인덱스 store 주입.
// nil 이면 ValidateSwap / ConsumeSwap 이 Unimplemented (codes.Unimplemented)
// 반환 — 매매 AP 가 명확한 startup gate 로 인지 가능.
func (s *QuoteValidationServer) SetSwapIndex(idx quoteid.SwapIndex) {
	s.swapIdx.Store(&idx)
}

func (s *QuoteValidationServer) loadSwapIndex() quoteid.SwapIndex {
	p := s.swapIdx.Load()
	if p == nil {
		return nil
	}
	return *p
}

// ValidateSwap — read-only. swap_id → near/far → LookupMany.
func (s *QuoteValidationServer) ValidateSwap(ctx context.Context, req *wtgpb.ValidateSwapRequest) (*wtgpb.ValidateSwapResponse, error) {
	start := time.Now()
	s.swapValidateTotal.Add(1)

	if ok, reason := s.checkEngine(req.GetEngineId(), quoteid.PermValidate); !ok {
		s.observeOp("validate_swap", "denied", time.Since(start))
		return nil, s.permissionDenied(req.GetSwapId(), req.GetEngineId(), "ValidateSwap", reason)
	}
	idx := s.loadSwapIndex()
	if idx == nil {
		return nil, status.Error(codes.Unimplemented, "SwapIndex 미주입 — mci-price 가 swap 미활성")
	}
	swapID := req.GetSwapId()
	if swapID == "" {
		s.swapValidateNotFound.Add(1)
		s.observeOp("validate_swap", "not_found", time.Since(start))
		return &wtgpb.ValidateSwapResponse{
			SwapStatus:   wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "swap_id required",
		}, nil
	}

	sw, err := idx.GetSwap(ctx, swapID)
	if err == quoteid.ErrSwapNotFound {
		s.swapValidateNotFound.Add(1)
		s.observeOp("validate_swap", "not_found", time.Since(start))
		s.logger.Info("swap validation",
			slog.String("swap_id", swapID),
			slog.String("engine_id", req.GetEngineId()),
			slog.String("status", "NOT_FOUND"))
		return &wtgpb.ValidateSwapResponse{
			SwapStatus:   wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "swap_id not found",
		}, nil
	}
	if err != nil {
		s.swapValidateInternal.Add(1)
		s.observeOp("validate_swap", "internal", time.Since(start))
		return nil, status.Errorf(codes.Internal, "swap index: %v", err)
	}

	// 두 leg 동시 lookup — Memory 는 단일 RLock, Redis 는 pipeline.
	results, err := s.registry.LookupMany(ctx, []quoteid.QuoteID{sw.NearID, sw.FarID})
	if err != nil {
		s.swapValidateInternal.Add(1)
		s.observeOp("validate_swap", "internal", time.Since(start))
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}
	if len(results) != 2 {
		s.swapValidateInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "lookup len=%d (기대 2)", len(results))
	}

	nearResp := s.lookupToValidateResponse(results[0], string(sw.NearID), req.GetEngineId())
	farResp := s.lookupToValidateResponse(results[1], string(sw.FarID), req.GetEngineId())
	swapStatus := worstStatus(nearResp.GetStatus(), farResp.GetStatus())

	resp := &wtgpb.ValidateSwapResponse{
		SwapStatus: swapStatus,
		Near:       nearResp,
		Far:        farResp,
	}
	switch swapStatus {
	case wtgpb.ValidationStatus_OK:
		s.swapValidateOK.Add(1)
	case wtgpb.ValidationStatus_EXPIRED:
		s.swapValidateExpired.Add(1)
		resp.OrdRejReason = ordRejReasonExpired
		resp.RejectText = "swap leg expired"
	case wtgpb.ValidationStatus_ALREADY_CONSUMED:
		s.swapValidateConsumed.Add(1)
		resp.OrdRejReason = ordRejReasonDuplicate
		resp.RejectText = "swap leg already consumed"
	default:
		s.swapValidateNotFound.Add(1)
		resp.OrdRejReason = ordRejReasonNotFound
		resp.RejectText = "swap leg not found"
	}
	s.observeOp("validate_swap", statusLabelForValidation(swapStatus), time.Since(start))
	return resp, nil
}

// ConsumeSwap — 두 leg MarkConsumedMany. 사전검사 통과 시도. 한 leg 라도
// 표시 실패면 다른 leg 의 표시는 시도 안 함 (atomic-skip).
func (s *QuoteValidationServer) ConsumeSwap(ctx context.Context, req *wtgpb.ConsumeSwapRequest) (*wtgpb.ConsumeSwapResponse, error) {
	start := time.Now()
	s.swapConsumeTotal.Add(1)

	if ok, reason := s.checkEngine(req.GetEngineId(), quoteid.PermMarkConsumed); !ok {
		s.observeOp("consume_swap", "denied", time.Since(start))
		return nil, s.permissionDenied(req.GetSwapId(), req.GetEngineId(), "ConsumeSwap", reason)
	}
	idx := s.loadSwapIndex()
	if idx == nil {
		return nil, status.Error(codes.Unimplemented, "SwapIndex 미주입 — mci-price 가 swap 미활성")
	}
	swapID := req.GetSwapId()
	if swapID == "" {
		s.swapConsumeNotFound.Add(1)
		return &wtgpb.ConsumeSwapResponse{
			SwapStatus:   wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "swap_id required",
		}, nil
	}

	sw, err := idx.GetSwap(ctx, swapID)
	if err == quoteid.ErrSwapNotFound {
		s.swapConsumeNotFound.Add(1)
		return &wtgpb.ConsumeSwapResponse{
			SwapStatus:   wtgpb.ValidationStatus_NOT_FOUND,
			OrdRejReason: ordRejReasonNotFound,
			RejectText:   "swap_id not found",
		}, nil
	}
	if err != nil {
		s.swapConsumeInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "swap index: %v", err)
	}

	// 1단계 — 사전 LookupMany. 둘 다 OK 가 아니면 표시 시도 안 함.
	pre, err := s.registry.LookupMany(ctx, []quoteid.QuoteID{sw.NearID, sw.FarID})
	if err != nil {
		s.swapConsumeInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "registry: %v", err)
	}
	if len(pre) != 2 {
		s.swapConsumeInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "lookup len=%d (기대 2)", len(pre))
	}
	preNear := s.lookupToValidateResponse(pre[0], string(sw.NearID), req.GetEngineId())
	preFar := s.lookupToValidateResponse(pre[1], string(sw.FarID), req.GetEngineId())
	preStatus := worstStatus(preNear.GetStatus(), preFar.GetStatus())
	if preStatus != wtgpb.ValidationStatus_OK {
		resp := &wtgpb.ConsumeSwapResponse{
			SwapStatus: preStatus,
			// Near/Far 는 MarkConsumed 모양으로 wrap — 사전검사 reason 그대로.
			Near: preValidateToConsumeShape(preNear),
			Far:  preValidateToConsumeShape(preFar),
		}
		switch preStatus {
		case wtgpb.ValidationStatus_EXPIRED:
			s.swapConsumeExpired.Add(1)
			resp.OrdRejReason = ordRejReasonExpired
			resp.RejectText = "swap leg expired"
		case wtgpb.ValidationStatus_ALREADY_CONSUMED:
			s.swapConsumeAlready.Add(1)
			resp.OrdRejReason = ordRejReasonDuplicate
			resp.RejectText = "swap leg already consumed"
		default:
			s.swapConsumeNotFound.Add(1)
			resp.OrdRejReason = ordRejReasonNotFound
			resp.RejectText = "swap leg not found"
		}
		s.observeOp("consume_swap", statusLabelForValidation(preStatus), time.Since(start))
		return resp, nil
	}

	// 2단계 — MarkConsumedMany. race 시 일부 ALREADY 가능.
	cmResults, err := s.registry.MarkConsumedMany(ctx, []quoteid.ConsumeRequest{
		{QuoteID: sw.NearID, ConsumerID: req.GetConsumerId()},
		{QuoteID: sw.FarID, ConsumerID: req.GetConsumerId()},
	})
	if err != nil {
		s.swapConsumeInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "mark consumed: %v", err)
	}
	if len(cmResults) != 2 {
		s.swapConsumeInternal.Add(1)
		return nil, status.Errorf(codes.Internal, "mark consumed len=%d", len(cmResults))
	}
	nearMC := swapConsumeResultToProto(cmResults[0])
	farMC := swapConsumeResultToProto(cmResults[1])
	swapStatus := worstStatus(nearMC.GetStatus(), farMC.GetStatus())

	resp := &wtgpb.ConsumeSwapResponse{
		SwapStatus: swapStatus,
		Near:       nearMC,
		Far:        farMC,
	}
	switch swapStatus {
	case wtgpb.ValidationStatus_OK:
		s.swapConsumeOK.Add(1)
	case wtgpb.ValidationStatus_EXPIRED:
		s.swapConsumeExpired.Add(1)
		resp.OrdRejReason = ordRejReasonExpired
		resp.RejectText = "swap leg expired"
	case wtgpb.ValidationStatus_ALREADY_CONSUMED:
		s.swapConsumeAlready.Add(1)
		s.swapConsumePartialRace.Add(1)
		resp.OrdRejReason = ordRejReasonDuplicate
		resp.RejectText = "swap leg already consumed (partial — race)"
		s.logger.Warn("swap consume partial race",
			slog.String("swap_id", swapID),
			slog.String("consumer_id", req.GetConsumerId()),
			slog.String("near_status", nearMC.GetStatus().String()),
			slog.String("far_status", farMC.GetStatus().String()))
	default:
		s.swapConsumeNotFound.Add(1)
		resp.OrdRejReason = ordRejReasonNotFound
		resp.RejectText = "swap leg not found"
	}
	s.observeOp("consume_swap", statusLabelForValidation(swapStatus), time.Since(start))
	return resp, nil
}

// worstStatus — 두 leg 중 더 나쁜 status 를 swap 단위로 채택. AND 정책.
// 우선순위: NOT_FOUND > ALREADY_CONSUMED > EXPIRED > OK > UNSPECIFIED.
func worstStatus(a, b wtgpb.ValidationStatus) wtgpb.ValidationStatus {
	rank := func(s wtgpb.ValidationStatus) int {
		switch s {
		case wtgpb.ValidationStatus_NOT_FOUND:
			return 4
		case wtgpb.ValidationStatus_ALREADY_CONSUMED:
			return 3
		case wtgpb.ValidationStatus_EXPIRED:
			return 2
		case wtgpb.ValidationStatus_OK:
			return 1
		default:
			return 0
		}
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// preValidateToConsumeShape — 사전검사 결과 (ValidateResponse) 를 응답의
// ConsumeSwapResponse.Near/Far 모양 (MarkConsumedResponse) 으로 변환.
// MarkConsumed 가 실제로 호출되지 않은 경우의 응답 채움.
func preValidateToConsumeShape(v *wtgpb.ValidateResponse) *wtgpb.MarkConsumedResponse {
	return &wtgpb.MarkConsumedResponse{
		Status:       v.GetStatus(),
		Record:       v.GetRecord(),
		OrdRejReason: v.GetOrdRejReason(),
		RejectText:   v.GetRejectText(),
	}
}

// swapConsumeResultToProto — quoteid.ConsumeResult → wtgpb.MarkConsumedResponse.
// MarkConsumed 핸들러와 동일 매핑. 본 swap 흐름은 카운터 갱신을 swap 카운터
// 에 통합하므로 별도 매핑 함수.
func swapConsumeResultToProto(r quoteid.ConsumeResult) *wtgpb.MarkConsumedResponse {
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
		resp.RejectText = "unknown ConsumeStatus"
	}
	return resp
}
