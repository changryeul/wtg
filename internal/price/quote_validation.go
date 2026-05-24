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
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/winwaysystems/wtg/pkg/quoteid"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// FIX 4.4 OrdRejReason (tag 103) 매핑 — RFC §4.3.
const (
	ordRejReasonNotFound int32 = 5  // Unknown order
	ordRejReasonExpired  int32 = 13 // Stale order
)

// QuoteValidationServer 는 wtgpb.QuoteValidationServiceServer 구현.
// pkg/quoteid.Registry 의 thin wrapper — 별도 비즈니스 로직 없음.
type QuoteValidationServer struct {
	wtgpb.UnimplementedQuoteValidationServiceServer

	registry quoteid.Registry
	logger   *slog.Logger
	now      func() time.Time

	// 누적 카운터 — `/v1/stats` 노출 또는 grpc interceptor 메트릭 대안.
	callsTotal      atomic.Uint64
	callsOK         atomic.Uint64
	callsNotFound   atomic.Uint64
	callsExpired    atomic.Uint64
	callsInternal   atomic.Uint64
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

// SetNow — 테스트용 시간 주입.
func (s *QuoteValidationServer) SetNow(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

// Validate 는 RFC §4.1 의 ValidateRequest 를 처리.
func (s *QuoteValidationServer) Validate(ctx context.Context, req *wtgpb.ValidateRequest) (*wtgpb.ValidateResponse, error) {
	s.callsTotal.Add(1)

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

	s.callsOK.Add(1)
	return &wtgpb.ValidateResponse{
		Status: wtgpb.ValidationStatus_OK,
		Record: recordToProto(rec),
	}, nil
}

// QuoteValidationStats — 누적 카운터 snapshot. 운영 모니터링용.
type QuoteValidationStats struct {
	Total    uint64 `json:"total"`
	OK       uint64 `json:"ok"`
	NotFound uint64 `json:"not_found"`
	Expired  uint64 `json:"expired"`
	Internal uint64 `json:"internal"`
}

func (s *QuoteValidationServer) Stats() QuoteValidationStats {
	return QuoteValidationStats{
		Total:    s.callsTotal.Load(),
		OK:       s.callsOK.Load(),
		NotFound: s.callsNotFound.Load(),
		Expired:  s.callsExpired.Load(),
		Internal: s.callsInternal.Load(),
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
