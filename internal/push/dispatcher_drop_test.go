package push

import (
	"context"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// TestDispatcherDropReason_Unsupp — FCTran 같은 비-push func 은 dropUnsupp 으로 분류.
// (기존 TestDispatcherIgnoresNonPushFuncs 는 Delivered=0 만 검증 — 본 테스트는
// 사유별 카운터까지 확인.)
func TestDispatcherDropReason_Unsupp(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	prefix := mkPrefix("ORDER", "WEB", "trader01", uint8(mymq.FCTran), 0)
	sub.ch <- mkUnsol(mymq.FCTran, prefix, []byte(`{}`))
	time.Sleep(50 * time.Millisecond)

	s := d.Stats()
	if s.Received != 1 || s.Dropped != 1 || s.DropUnsupp != 1 {
		t.Errorf("Received=%d Dropped=%d DropUnsupp=%d, want 1/1/1", s.Received, s.Dropped, s.DropUnsupp)
	}
	if s.DropUnknownUser != 0 || s.DropNoBroadcast != 0 || s.DropEnvelope != 0 {
		t.Errorf("다른 drop 사유 오염: %+v", s)
	}
}

// TestDispatcherDropReason_UnknownUser — LogonID 명시인데 user 등록 안 됨.
func TestDispatcherDropReason_UnknownUser(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	prefix := mkPrefix("EXEC", "", "ghost_user", uint8(mymq.FCPush), uint8(mymq.SubPush))
	sub.ch <- mkUnsol(mymq.FCPush, prefix, []byte(`{}`))
	time.Sleep(50 * time.Millisecond)

	s := d.Stats()
	if s.DropUnknownUser != 1 || s.Dropped != 1 {
		t.Errorf("DropUnknownUser=%d Dropped=%d, want 1/1", s.DropUnknownUser, s.Dropped)
	}
	if s.DropUnsupp != 0 || s.DropNoBroadcast != 0 {
		t.Errorf("다른 drop 사유 오염: %+v", s)
	}
}

// TestDispatcherDropReason_NoBroadcast — LogonID 빈값 + 등록 conn 0.
func TestDispatcherDropReason_NoBroadcast(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	prefix := mkPrefix("PRICE", "", "", uint8(mymq.FCCast), uint8(mymq.SubBroadcast))
	sub.ch <- mkUnsol(mymq.FCCast, prefix, []byte(`{}`))
	time.Sleep(50 * time.Millisecond)

	s := d.Stats()
	if s.DropNoBroadcast != 1 || s.Dropped != 1 {
		t.Errorf("DropNoBroadcast=%d Dropped=%d, want 1/1", s.DropNoBroadcast, s.Dropped)
	}
}

// TestDispatcherSendFailed — slow consumer (queue 가득) 인 conn 의 send 실패는
// sendFailed 로 카운트되고 delivered 는 영향 없음.
func TestDispatcherSendFailed(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})

	// 빠른 consumer + 느린 consumer 동일 user
	fast := mkTestConn("trader01", 4)
	slow := mkTestConn("trader01", 1)
	// slow 의 send 큐 미리 가득 채워서 다음 Send 가 ErrSendQueueFull 반환하게.
	slow.send <- []byte(`already-queued`)
	r.Add(fast)
	r.Add(slow)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	prefix := mkPrefix("EXEC", "", "trader01", uint8(mymq.FCPush), uint8(mymq.SubPush))
	sub.ch <- mkUnsol(mymq.FCPush, prefix, []byte(`{"event":"test"}`))
	time.Sleep(100 * time.Millisecond)

	s := d.Stats()
	if s.Delivered < 1 {
		t.Errorf("Delivered=%d, want >= 1 (fast conn 에 전달)", s.Delivered)
	}
	if s.SendFailed < 1 {
		t.Errorf("SendFailed=%d, want >= 1 (slow conn 의 queue 가득)", s.SendFailed)
	}
	if s.Dropped != 0 {
		t.Errorf("일부라도 전달됐으면 Dropped=0, got %d", s.Dropped)
	}
}

// TestDispatcherStatsSumConsistency — Dropped == sum of all drop reasons.
// 누적 합산 일관성 보장 (Prometheus dashboard 가 두 표현 다 노출).
func TestDispatcherStatsSumConsistency(t *testing.T) {
	sub := newFakeSub()
	r := NewRegistry(discardLogger())
	d := NewDispatcher(DispatcherOptions{Sub: sub, Registry: r, Logger: discardLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// 사유별 1건씩 발사
	sub.ch <- mkUnsol(mymq.FCTran, mkPrefix("", "", "u", uint8(mymq.FCTran), 0), nil)                       // unsupp
	sub.ch <- mkUnsol(mymq.FCPush, mkPrefix("", "", "ghost", uint8(mymq.FCPush), uint8(mymq.SubPush)), nil) // unknown user
	sub.ch <- mkUnsol(mymq.FCCast, mkPrefix("", "", "", uint8(mymq.FCCast), uint8(mymq.SubBroadcast)), nil) // no broadcast

	time.Sleep(150 * time.Millisecond)
	s := d.Stats()
	sum := s.DropUnsupp + s.DropEnvelope + s.DropUnknownUser + s.DropNoBroadcast
	if s.Dropped != sum {
		t.Errorf("Dropped=%d != 사유별 합 %d (Unsupp=%d Envelope=%d UnknownUser=%d NoBroadcast=%d)",
			s.Dropped, sum, s.DropUnsupp, s.DropEnvelope, s.DropUnknownUser, s.DropNoBroadcast)
	}
	if s.Dropped != 3 {
		t.Errorf("3 사유 1건씩 → Dropped=3 기대, got %d", s.Dropped)
	}
}
