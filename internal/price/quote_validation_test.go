package price

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func qvQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mkValidationServer(t *testing.T) (*QuoteValidationServer, *quoteid.MemoryRegistry) {
	t.Helper()
	reg := quoteid.NewMemoryRegistry(0)
	srv := NewQuoteValidationServer(reg, qvQuietLogger())
	return srv, reg
}

func mkRegRecord(id string, ts time.Time, validity time.Duration) quoteid.Record {
	return quoteid.Record{
		QuoteID: quoteid.QuoteID(id),
		Pair:    session.Pair("USD/KRW"),
		Profile: session.Profile{
			Channel: session.ChannelWeb,
			Site:    session.SiteBranch,
			Tier:    session.TierVIP,
		},
		Tenor:      "SPOT",
		Bid:        1400.10,
		Ask:        1400.15,
		IssuedAt:   ts.UnixNano(),
		ValidUntil: ts.Add(validity).UnixNano(),
		Sequence:   42,
		Issuer:     "A",
	}
}

func TestQuoteValidation_OK(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	rec := mkRegRecord("A-mq-1", ts, 30*time.Second)
	if err := reg.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId:  "A-mq-1",
		EngineId: "test-engine",
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("status=%v, want OK", resp.GetStatus())
	}
	if resp.GetRecord() == nil {
		t.Fatal("record nil 인데 OK")
	}
	r := resp.GetRecord()
	if r.GetPair() != "USD/KRW" || r.GetBid() != 1400.10 || r.GetAsk() != 1400.15 {
		t.Errorf("record echo mismatch: pair=%s bid=%v ask=%v", r.GetPair(), r.GetBid(), r.GetAsk())
	}
	if r.GetChannel() != "WEB" || r.GetSite() != "BRANCH" || r.GetTier() != "VIP" {
		t.Errorf("Profile echo mismatch: %s/%s/%s", r.GetChannel(), r.GetSite(), r.GetTier())
	}
	if r.GetIssuer() != "A" || r.GetSequence() != 42 {
		t.Errorf("Issuer/Sequence: %s/%d", r.GetIssuer(), r.GetSequence())
	}
	if resp.GetOrdRejReason() != 0 || resp.GetRejectText() != "" {
		t.Errorf("OK 인데 reject 정보 설정됨: %d / %q", resp.GetOrdRejReason(), resp.GetRejectText())
	}
}

func TestQuoteValidation_NotFound(t *testing.T) {
	srv, _ := mkValidationServer(t)
	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-nope",
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("status=%v, want NOT_FOUND", resp.GetStatus())
	}
	if resp.GetOrdRejReason() != 5 {
		t.Errorf("OrdRejReason=%d, want 5 (Unknown order)", resp.GetOrdRejReason())
	}
	if resp.GetRecord() != nil {
		t.Errorf("NOT_FOUND 인데 record 채워짐: %+v", resp.GetRecord())
	}
}

func TestQuoteValidation_EmptyQuoteID(t *testing.T) {
	srv, _ := mkValidationServer(t)
	resp, _ := srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: ""})
	if resp.GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("status=%v, want NOT_FOUND", resp.GetStatus())
	}
	if resp.GetRejectText() != "quote_id required" {
		t.Errorf("rejectText=%q", resp.GetRejectText())
	}
}

func TestQuoteValidation_Expired(t *testing.T) {
	// Registry 가 grace 안에 있는 record 를 반환해도, Validate 가 wallclock 으로
	// EXPIRED 판정해야 함.
	reg := quoteid.NewMemoryRegistry(time.Hour) // grace 크게 — Registry GC 무효화.
	srv := NewQuoteValidationServer(reg, qvQuietLogger())

	t0 := time.Unix(1700000000, 0)
	reg.SetNow(func() time.Time { return t0 })

	rec := mkRegRecord("A-old", t0, 500*time.Millisecond)
	if err := reg.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Validate now = t0 + 1s — ValidUntil 도래 후 (grace 안이라 registry 는 보존).
	srv.SetNow(func() time.Time { return t0.Add(time.Second) })
	reg.SetNow(func() time.Time { return t0.Add(time.Second) })

	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-old"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_EXPIRED {
		t.Errorf("status=%v, want EXPIRED", resp.GetStatus())
	}
	if resp.GetOrdRejReason() != 13 {
		t.Errorf("OrdRejReason=%d, want 13 (Stale order)", resp.GetOrdRejReason())
	}
	// EXPIRED 도 record 는 echo (감사 추적).
	if resp.GetRecord() == nil || resp.GetRecord().GetQuoteId() != "A-old" {
		t.Errorf("EXPIRED 인데 record 누락: %+v", resp.GetRecord())
	}
}

func TestQuoteValidation_ValidateAlreadyConsumed(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))

	// 먼저 MarkConsumed.
	_, err := srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId:    "A-1",
		ConsumerId: "order-1",
	})
	if err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}

	// 이제 Validate 가 ALREADY_CONSUMED.
	resp, _ := srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})
	if resp.GetStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Errorf("status=%v, want ALREADY_CONSUMED", resp.GetStatus())
	}
	if resp.GetOrdRejReason() != 6 {
		t.Errorf("OrdRejReason=%d, want 6 (Duplicate)", resp.GetOrdRejReason())
	}
	if resp.GetRecord() == nil {
		t.Error("ALREADY_CONSUMED 인데 record 비어있음")
	}
}

func TestQuoteValidation_MarkConsumed_FirstWins(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))

	r1, err := srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId:    "A-1",
		ConsumerId: "order-X",
	})
	if err != nil {
		t.Fatalf("MarkConsumed 1: %v", err)
	}
	if r1.GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("first: %v, want OK", r1.GetStatus())
	}

	r2, _ := srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId:    "A-1",
		ConsumerId: "order-Y",
	})
	if r2.GetStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Errorf("second: %v, want ALREADY_CONSUMED", r2.GetStatus())
	}
	if r2.GetConsumedBy() != "order-X" {
		t.Errorf("ConsumedBy=%q, want order-X", r2.GetConsumedBy())
	}
	if r2.GetOrdRejReason() != 6 {
		t.Errorf("OrdRejReason=%d, want 6", r2.GetOrdRejReason())
	}
}

func TestQuoteValidation_MarkConsumed_NotFound(t *testing.T) {
	srv, _ := mkValidationServer(t)
	r, _ := srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId:    "A-nope",
		ConsumerId: "order-1",
	})
	if r.GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("status=%v, want NOT_FOUND", r.GetStatus())
	}
	if r.GetOrdRejReason() != 5 {
		t.Errorf("OrdRejReason=%d, want 5", r.GetOrdRejReason())
	}
}

func TestQuoteValidation_BatchValidate(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))
	_ = reg.Put(context.Background(), mkRegRecord("A-2", ts, time.Hour))
	// A-3 는 MarkConsumed 로 ALREADY_CONSUMED 만들기.
	_ = reg.Put(context.Background(), mkRegRecord("A-3", ts, time.Hour))
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-3", ConsumerId: "order-X",
	})

	resp, err := srv.BatchValidate(context.Background(), &wtgpb.BatchValidateRequest{
		QuoteIds: []string{"A-1", "A-2", "A-3", "A-nope", ""},
		EngineId: "test-engine",
	})
	if err != nil {
		t.Fatalf("BatchValidate: %v", err)
	}
	results := resp.GetResults()
	if len(results) != 5 {
		t.Fatalf("results len=%d, want 5", len(results))
	}
	// 입력 순서 그대로.
	if results[0].GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("results[0] (A-1) status=%v, want OK", results[0].GetStatus())
	}
	if results[1].GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("results[1] (A-2) status=%v, want OK", results[1].GetStatus())
	}
	if results[2].GetStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Errorf("results[2] (A-3) status=%v, want ALREADY_CONSUMED", results[2].GetStatus())
	}
	if results[3].GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("results[3] (A-nope) status=%v, want NOT_FOUND", results[3].GetStatus())
	}
	if results[4].GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("results[4] (빈 QuoteID) status=%v, want NOT_FOUND", results[4].GetStatus())
	}

	s := srv.Stats()
	if s.BatchTotal != 1 {
		t.Errorf("BatchTotal=%d, want 1", s.BatchTotal)
	}
	if s.BatchItems != 5 {
		t.Errorf("BatchItems=%d, want 5", s.BatchItems)
	}
}

func TestQuoteValidation_BatchValidate_Empty(t *testing.T) {
	srv, _ := mkValidationServer(t)
	resp, err := srv.BatchValidate(context.Background(), &wtgpb.BatchValidateRequest{})
	if err != nil {
		t.Fatalf("BatchValidate empty: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("빈 batch results len=%d", len(resp.GetResults()))
	}
}

func TestQuoteValidation_BatchValidate_ExceedsMax(t *testing.T) {
	srv, _ := mkValidationServer(t)
	ids := make([]string, MaxBatchValidateSize+1)
	for i := range ids {
		ids[i] = "A-x"
	}
	_, err := srv.BatchValidate(context.Background(), &wtgpb.BatchValidateRequest{QuoteIds: ids})
	if err == nil {
		t.Fatal("상한 초과인데 error nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code=%v, want InvalidArgument", st.Code())
	}
}

func TestQuoteValidation_BatchMarkConsumed(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))
	_ = reg.Put(context.Background(), mkRegRecord("A-2", ts, time.Hour))
	_ = reg.Put(context.Background(), mkRegRecord("A-3", ts, time.Hour))
	// A-3 는 이미 다른 consumer 가 잡음 — race 시뮬레이션.
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-3", ConsumerId: "order-pre",
	})

	resp, err := srv.BatchMarkConsumed(context.Background(), &wtgpb.BatchMarkConsumedRequest{
		Items: []*wtgpb.ConsumeItem{
			{QuoteId: "A-1", ConsumerId: "order-1"},
			{QuoteId: "A-2", ConsumerId: "order-2"},
			{QuoteId: "A-3", ConsumerId: "order-3"},
			{QuoteId: "A-nope", ConsumerId: "order-4"},
		},
		EngineId: "test-engine",
	})
	if err != nil {
		t.Fatalf("BatchMarkConsumed: %v", err)
	}
	results := resp.GetResults()
	if len(results) != 4 {
		t.Fatalf("results len=%d, want 4", len(results))
	}
	if results[0].GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("results[0] (A-1) status=%v, want OK", results[0].GetStatus())
	}
	if results[1].GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("results[1] (A-2) status=%v, want OK", results[1].GetStatus())
	}
	if results[2].GetStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Errorf("results[2] (A-3) status=%v, want ALREADY_CONSUMED", results[2].GetStatus())
	}
	if results[2].GetConsumedBy() != "order-pre" {
		t.Errorf("results[2].consumedBy=%q, want order-pre", results[2].GetConsumedBy())
	}
	if results[3].GetStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("results[3] (A-nope) status=%v, want NOT_FOUND", results[3].GetStatus())
	}

	s := srv.Stats()
	if s.BatchConsumeTotal != 1 {
		t.Errorf("BatchConsumeTotal=%d, want 1", s.BatchConsumeTotal)
	}
	if s.BatchConsumeItems != 4 {
		t.Errorf("BatchConsumeItems=%d, want 4", s.BatchConsumeItems)
	}
}

func TestQuoteValidation_BatchMarkConsumed_Empty(t *testing.T) {
	srv, _ := mkValidationServer(t)
	resp, err := srv.BatchMarkConsumed(context.Background(), &wtgpb.BatchMarkConsumedRequest{})
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("빈 batch results len=%d", len(resp.GetResults()))
	}
}

func TestQuoteValidation_BatchMarkConsumed_ExceedsMax(t *testing.T) {
	srv, _ := mkValidationServer(t)
	items := make([]*wtgpb.ConsumeItem, MaxBatchConsumeSize+1)
	for i := range items {
		items[i] = &wtgpb.ConsumeItem{QuoteId: "A-x", ConsumerId: "c"}
	}
	_, err := srv.BatchMarkConsumed(context.Background(), &wtgpb.BatchMarkConsumedRequest{Items: items})
	if err == nil {
		t.Fatal("상한 초과인데 err nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code=%v, want InvalidArgument", st.Code())
	}
}

func TestQuoteValidation_EngineAllowlist_AllowsListed(t *testing.T) {
	srv, reg := mkValidationServer(t)
	srv.SetEngineAllowlist([]string{"engine-A", "engine-B"})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// 허용된 engine_id — 통과.
	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "engine-A",
	})
	if err != nil {
		t.Fatalf("허용된 engine: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("status=%v, want OK", resp.GetStatus())
	}
}

func TestQuoteValidation_EngineAllowlist_DeniesUnknown(t *testing.T) {
	srv, reg := mkValidationServer(t)
	srv.SetEngineAllowlist([]string{"engine-A"})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// 미허용 engine_id — Validate 거부.
	_, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "engine-EVIL",
	})
	if err == nil {
		t.Fatal("허용 안 된 engine 통과 — RBAC 회귀")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.PermissionDenied {
		t.Errorf("err code=%v, want PermissionDenied", st.Code())
	}

	// 빈 engine_id (= 미지정) 도 거부.
	_, err = srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "",
	})
	if err == nil {
		t.Fatal("빈 engine_id 통과 — RBAC 회귀")
	}

	// 모든 RPC 가 동일하게 차단.
	_, err = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-1", ConsumerId: "order-1", EngineId: "evil",
	})
	if err == nil {
		t.Fatal("MarkConsumed: evil 통과")
	}
	_, err = srv.BatchValidate(context.Background(), &wtgpb.BatchValidateRequest{
		QuoteIds: []string{"A-1"}, EngineId: "evil",
	})
	if err == nil {
		t.Fatal("BatchValidate: evil 통과")
	}
	_, err = srv.BatchMarkConsumed(context.Background(), &wtgpb.BatchMarkConsumedRequest{
		Items:    []*wtgpb.ConsumeItem{{QuoteId: "A-1", ConsumerId: "order-1"}},
		EngineId: "evil",
	})
	if err == nil {
		t.Fatal("BatchMarkConsumed: evil 통과")
	}

	// 카운터.
	if got := srv.Stats().DeniedEngine; got < 5 {
		t.Errorf("DeniedEngine=%d, want ≥5 (Validate empty + 4 RPCs)", got)
	}
}

func TestQuoteValidation_EngineMeta_ReadOnlyEngine(t *testing.T) {
	srv, reg := mkValidationServer(t)
	// audit-cli — Validate 만 허용 (read-only).
	srv.SetEngineAllowlistMap(map[string]quoteid.EngineMeta{
		"audit-cli": {Permissions: []string{quoteid.PermValidate}},
		"matching":  {}, // 풀 권한 (default)
	})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// audit-cli + Validate → 허용.
	_, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "audit-cli",
	})
	if err != nil {
		t.Errorf("audit-cli Validate 거부: %v", err)
	}

	// audit-cli + MarkConsumed → 거부 (권한 없음).
	_, err = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-1", ConsumerId: "order-1", EngineId: "audit-cli",
	})
	if err == nil {
		t.Fatal("audit-cli MarkConsumed 통과 — 권한 분리 회귀")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.PermissionDenied {
		t.Errorf("code=%v, want PermissionDenied", st.Code())
	}
	if !strings.Contains(st.Message(), "mark_consumed") {
		t.Errorf("reject_text 에 권한 누락 메시지 없음: %s", st.Message())
	}

	// matching + MarkConsumed → 허용.
	if _, err := srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{
		QuoteId: "A-1", ConsumerId: "order-1", EngineId: "matching",
	}); err != nil {
		t.Errorf("matching MarkConsumed 거부: %v", err)
	}
}

func TestQuoteValidation_EngineMeta_Expired(t *testing.T) {
	srv, reg := mkValidationServer(t)
	// engine-A 가 이미 만료 (과거 timestamp).
	srv.SetEngineAllowlistMap(map[string]quoteid.EngineMeta{
		"engine-A": {ExpiresAt: "2020-01-01T00:00:00Z"},
	})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	_, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "engine-A",
	})
	if err == nil {
		t.Fatal("만료된 engine 통과 — auto-revoke 회귀")
	}
	st, _ := status.FromError(err)
	if !strings.Contains(st.Message(), "expired") {
		t.Errorf("reject_text 에 expired 누락: %s", st.Message())
	}
}

func TestQuoteValidation_EngineMeta_FutureExpiry(t *testing.T) {
	srv, reg := mkValidationServer(t)
	srv.SetEngineAllowlistMap(map[string]quoteid.EngineMeta{
		"engine-A": {ExpiresAt: "2099-01-01T00:00:00Z"},
	})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))
	if _, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "engine-A",
	}); err != nil {
		t.Errorf("미래 만료 engine 거부: %v", err)
	}
}

func TestQuoteValidation_EngineMeta_InvalidExpiryFailsOpen(t *testing.T) {
	srv, reg := mkValidationServer(t)
	// 잘못된 RFC3339 — 파싱 실패 시 운영 안전성 위해 통과 (운영자가 발견하여 수정).
	srv.SetEngineAllowlistMap(map[string]quoteid.EngineMeta{
		"engine-A": {ExpiresAt: "not-a-date"},
	})
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))
	if _, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "engine-A",
	}); err != nil {
		t.Errorf("잘못된 expires_at 으로 거부 — fail-open 회귀: %v", err)
	}
}

func TestQuoteValidation_EngineAllowlist_DisabledByEmpty(t *testing.T) {
	srv, reg := mkValidationServer(t)
	srv.SetEngineAllowlist([]string{}) // 빈 → RBAC 비활성
	_ = reg.Put(context.Background(), mkRegRecord("A-1", time.Now(), time.Hour))

	// 어떤 engine_id 든 통과 — back-compat.
	resp, err := srv.Validate(context.Background(), &wtgpb.ValidateRequest{
		QuoteId: "A-1", EngineId: "anything",
	})
	if err != nil {
		t.Fatalf("RBAC 비활성인데 거부: %v", err)
	}
	if resp.GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("status=%v, want OK", resp.GetStatus())
	}
}

func TestQuoteValidation_Stats(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))
	_ = reg.Put(context.Background(), mkRegRecord("A-2", ts, time.Hour))

	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})   // OK
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-xxx"}) // NOT_FOUND
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: ""})      // NOT_FOUND (empty)

	// A-2 표시 후 다시 Validate → ALREADY_CONSUMED.
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{QuoteId: "A-2", ConsumerId: "order-1"}) // ConsumeOK
	_, _ = srv.MarkConsumed(context.Background(), &wtgpb.MarkConsumedRequest{QuoteId: "A-2", ConsumerId: "order-2"}) // AlreadyDone
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-2"})                                // ALREADY_CONSUMED

	s := srv.Stats()
	if s.Total != 4 {
		t.Errorf("Total=%d, want 4", s.Total)
	}
	if s.OK != 1 {
		t.Errorf("OK=%d, want 1", s.OK)
	}
	if s.NotFound != 2 {
		t.Errorf("NotFound=%d, want 2", s.NotFound)
	}
	if s.Consumed != 1 {
		t.Errorf("Consumed=%d, want 1", s.Consumed)
	}
	if s.ConsumeTotal != 2 {
		t.Errorf("ConsumeTotal=%d, want 2", s.ConsumeTotal)
	}
	if s.ConsumeOK != 1 {
		t.Errorf("ConsumeOK=%d, want 1", s.ConsumeOK)
	}
	if s.ConsumeAlready != 1 {
		t.Errorf("ConsumeAlready=%d, want 1", s.ConsumeAlready)
	}
}
