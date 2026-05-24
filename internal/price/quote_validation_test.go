package price

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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

func TestQuoteValidation_Stats(t *testing.T) {
	srv, reg := mkValidationServer(t)
	ts := time.Now()
	_ = reg.Put(context.Background(), mkRegRecord("A-1", ts, time.Hour))

	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})    // OK
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-xxx"})  // NOT_FOUND
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: ""})       // NOT_FOUND (empty)
	_, _ = srv.Validate(context.Background(), &wtgpb.ValidateRequest{QuoteId: "A-1"})    // OK

	s := srv.Stats()
	if s.Total != 4 {
		t.Errorf("Total=%d, want 4", s.Total)
	}
	if s.OK != 2 {
		t.Errorf("OK=%d, want 2", s.OK)
	}
	if s.NotFound != 2 {
		t.Errorf("NotFound=%d, want 2", s.NotFound)
	}
}
