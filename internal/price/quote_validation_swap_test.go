package price

import (
	"context"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quoteid"
	"github.com/winwaysystems/wtg/pkg/session"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// makeSwapValidator — Registry + SwapIndex 가 같은 MemoryRegistry 인스턴스인
// 일반 dev 셋업. now=고정시간 t0 → ValidAt 분기를 결정적으로 제어.
func makeSwapValidator(t *testing.T) (*QuoteValidationServer, *quoteid.MemoryRegistry, time.Time) {
	t.Helper()
	reg := quoteid.NewMemoryRegistry(time.Second)
	t0 := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	reg.SetNow(func() time.Time { return t0 })
	srv := NewQuoteValidationServer(reg, nil)
	srv.now = func() time.Time { return t0 }
	srv.SetSwapIndex(reg)
	return srv, reg, t0
}

// putSwap — near/far record + swap_id 인덱스 셋업 helper.
func putSwap(t *testing.T, reg *quoteid.MemoryRegistry, t0 time.Time, validity time.Duration,
	swapID, near, far string,
) {
	t.Helper()
	ctx := context.Background()
	profile, _ := session.ParseProfileKey("WEB.BRANCH.VIP")
	mkRec := func(id string) quoteid.Record {
		return quoteid.Record{
			QuoteID: quoteid.QuoteID(id),
			Pair:    "USD/KRW",
			Profile: profile,
			Tenor:   "SPOT",
			Bid:     1373.12, Ask: 1373.22,
			IssuedAt:   t0.UnixNano(),
			ValidUntil: t0.Add(validity).UnixNano(),
			Sequence:   1, Issuer: "T",
		}
	}
	if err := reg.Put(ctx, mkRec(near)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ctx, mkRec(far)); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutSwap(ctx, quoteid.SwapRecord{
		SwapID:     swapID,
		NearID:     quoteid.QuoteID(near),
		FarID:      quoteid.QuoteID(far),
		IssuedAt:   t0.UnixNano(),
		ValidUntil: t0.Add(validity).UnixNano(),
		Issuer:     "T",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSwap_OK(t *testing.T) {
	srv, reg, t0 := makeSwapValidator(t)
	putSwap(t, reg, t0, 500*time.Millisecond, "SW-1", "QN-1", "QF-1")

	resp, err := srv.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{
		SwapId: "SW-1", EngineId: "matching-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSwapStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("swap_status=%v 기대 OK, near=%v far=%v", resp.GetSwapStatus(),
			resp.GetNear().GetStatus(), resp.GetFar().GetStatus())
	}
	if resp.GetNear().GetRecord().GetQuoteId() != "QN-1" {
		t.Errorf("near record echo 실패: %+v", resp.GetNear())
	}
}

func TestValidateSwap_SwapNotFound(t *testing.T) {
	srv, _, _ := makeSwapValidator(t)
	resp, err := srv.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{
		SwapId: "SW-NONE", EngineId: "matching-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSwapStatus() != wtgpb.ValidationStatus_NOT_FOUND {
		t.Errorf("status=%v 기대 NOT_FOUND", resp.GetSwapStatus())
	}
}

func TestValidateSwap_OneLegExpired(t *testing.T) {
	srv, reg, t0 := makeSwapValidator(t)
	// near 는 정상, far 는 이미 만료된 ValidUntil 로 직접 Put.
	profile, _ := session.ParseProfileKey("WEB.BRANCH.VIP")
	if err := reg.Put(context.Background(), quoteid.Record{
		QuoteID: "QN-2", Pair: "USD/KRW", Profile: profile, Tenor: "SPOT",
		Bid: 1.0, Ask: 1.0,
		IssuedAt: t0.UnixNano(), ValidUntil: t0.Add(500 * time.Millisecond).UnixNano(),
		Sequence: 1, Issuer: "T",
	}); err != nil {
		t.Fatal(err)
	}
	// far — IssuedAt 보다 ValidUntil 이 ahead 면 Put OK 지만 ValidAt(t0) 시 expired
	// 가 되도록 ValidUntil 을 t0 직전에 둠. grace=1s 라 Get 은 가능.
	if err := reg.Put(context.Background(), quoteid.Record{
		QuoteID: "QF-2", Pair: "USD/KRW", Profile: profile, Tenor: "1M",
		Bid: 1.0, Ask: 1.0,
		IssuedAt:   t0.Add(-2 * time.Second).UnixNano(),
		ValidUntil: t0.Add(-1 * time.Millisecond).UnixNano(),
		Sequence:   2, Issuer: "T",
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutSwap(context.Background(), quoteid.SwapRecord{
		SwapID: "SW-2", NearID: "QN-2", FarID: "QF-2",
		IssuedAt: t0.UnixNano(), ValidUntil: t0.Add(500 * time.Millisecond).UnixNano(),
		Issuer: "T",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{
		SwapId: "SW-2", EngineId: "matching-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSwapStatus() != wtgpb.ValidationStatus_EXPIRED {
		t.Errorf("status=%v 기대 EXPIRED (far leg)", resp.GetSwapStatus())
	}
	if resp.GetNear().GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("near 는 OK 여야: %v", resp.GetNear().GetStatus())
	}
}

func TestConsumeSwap_HappyPath_BothMarked(t *testing.T) {
	srv, reg, t0 := makeSwapValidator(t)
	putSwap(t, reg, t0, 500*time.Millisecond, "SW-3", "QN-3", "QF-3")

	resp, err := srv.ConsumeSwap(context.Background(), &wtgpb.ConsumeSwapRequest{
		SwapId: "SW-3", ConsumerId: "order-42", EngineId: "matching-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSwapStatus() != wtgpb.ValidationStatus_OK {
		t.Fatalf("status=%v 기대 OK", resp.GetSwapStatus())
	}
	if resp.GetNear().GetStatus() != wtgpb.ValidationStatus_OK ||
		resp.GetFar().GetStatus() != wtgpb.ValidationStatus_OK {
		t.Errorf("두 leg 모두 OK 여야: near=%v far=%v",
			resp.GetNear().GetStatus(), resp.GetFar().GetStatus())
	}
	// 두 번째 호출 → ALREADY_CONSUMED (둘 다).
	resp2, _ := srv.ConsumeSwap(context.Background(), &wtgpb.ConsumeSwapRequest{
		SwapId: "SW-3", ConsumerId: "order-43", EngineId: "matching-A",
	})
	if resp2.GetSwapStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Errorf("재호출 status=%v 기대 ALREADY_CONSUMED", resp2.GetSwapStatus())
	}
}

func TestConsumeSwap_AtomicSkip_OneLegAlreadyConsumed(t *testing.T) {
	srv, reg, t0 := makeSwapValidator(t)
	putSwap(t, reg, t0, 500*time.Millisecond, "SW-4", "QN-4", "QF-4")
	// near 는 이미 다른 주문이 소비.
	if _, err := reg.MarkConsumed(context.Background(), "QN-4", "other-order"); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.ConsumeSwap(context.Background(), &wtgpb.ConsumeSwapRequest{
		SwapId: "SW-4", ConsumerId: "order-99", EngineId: "matching-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSwapStatus() != wtgpb.ValidationStatus_ALREADY_CONSUMED {
		t.Fatalf("status=%v 기대 ALREADY_CONSUMED", resp.GetSwapStatus())
	}
	// far 는 표시되지 않아야 — atomic-skip.
	c, ok, _ := reg.Consumed(context.Background(), "QF-4")
	if ok {
		t.Errorf("far 가 표시됐음 (consumer=%q) — atomic-skip 위반", c)
	}
}

func TestValidateSwap_NoSwapIndex_Unimplemented(t *testing.T) {
	reg := quoteid.NewMemoryRegistry(time.Second)
	srv := NewQuoteValidationServer(reg, nil)
	// SetSwapIndex 호출 안 함.
	_, err := srv.ValidateSwap(context.Background(), &wtgpb.ValidateSwapRequest{
		SwapId: "SW-X",
	})
	if err == nil {
		t.Fatal("SwapIndex 미주입인데 err=nil")
	}
}
