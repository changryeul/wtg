package price

import (
	"testing"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func mkQ(sym string, seq int64) wtgpb.AlgoQuote {
	return wtgpb.AlgoQuote{Sym: sym, Seq: seq, Bid: 1.0, Ask: 1.1}
}

// ring 이 비어있을 때 — ticks nil, gap=false.
func TestAlgoRing_Empty(t *testing.T) {
	r := newAlgoRing(5)
	ticks, oldest, gap := r.snapshot(0)
	if len(ticks) != 0 || oldest != 0 || gap {
		t.Fatalf("empty ring: ticks=%d oldest=%d gap=%v", len(ticks), oldest, gap)
	}
}

// ring 미 가득 상태 — 3 push, snapshot(0) → 3 개 리턴.
func TestAlgoRing_PartialFill(t *testing.T) {
	r := newAlgoRing(5)
	for i := int64(1); i <= 3; i++ {
		r.push(mkQ("USDKRW", i))
	}
	ticks, oldest, gap := r.snapshot(0)
	if gap {
		t.Fatalf("unexpected gap")
	}
	if oldest != 1 || len(ticks) != 3 {
		t.Fatalf("ticks=%d oldest=%d", len(ticks), oldest)
	}
	for i, tk := range ticks {
		if tk.Seq != int64(i+1) {
			t.Errorf("ticks[%d].Seq=%d want %d", i, tk.Seq, i+1)
		}
	}
}

// ring wrap 후 fromSeq=0 요청 — client 는 처음부터 원하지만 ring 은 밀려나가
// oldest=4 라 seq 1~3 을 못 줌 → gap.
func TestAlgoRing_WrapAroundFromZero(t *testing.T) {
	r := newAlgoRing(5)
	for i := int64(1); i <= 8; i++ {
		r.push(mkQ("USDKRW", i))
	}
	_, oldest, gap := r.snapshot(0)
	if !gap {
		t.Fatalf("gap 예상 — fromSeq+1=1 < oldest=%d 이므로 client 는 seq 1~3 을 잃어버림", oldest)
	}
	if oldest != 4 {
		t.Errorf("oldest=%d want 4", oldest)
	}
}

// ring wrap 후 fromSeq 가 oldest 이내면 replay 정상.
func TestAlgoRing_SnapshotWithinWindow(t *testing.T) {
	r := newAlgoRing(5)
	for i := int64(1); i <= 8; i++ {
		r.push(mkQ("USDKRW", i))
	}
	// ring 안 4~8. fromSeq=5 → 6,7,8 리턴.
	ticks, oldest, gap := r.snapshot(5)
	if gap {
		t.Fatalf("gap 예상 안 함 (fromSeq+1=6 >= oldest=4)")
	}
	if oldest != 4 {
		t.Errorf("oldest=%d want 4", oldest)
	}
	if len(ticks) != 3 {
		t.Fatalf("ticks=%d want 3", len(ticks))
	}
	for i, tk := range ticks {
		if tk.Seq != int64(i+6) {
			t.Errorf("ticks[%d].Seq=%d want %d", i, tk.Seq, i+6)
		}
	}
}

// gap 발생 — client 가 seq=1 요청, ring 은 4~8. 1+1 < 4 → gap.
func TestAlgoRing_SnapshotGap(t *testing.T) {
	r := newAlgoRing(5)
	for i := int64(1); i <= 8; i++ {
		r.push(mkQ("USDKRW", i))
	}
	ticks, oldest, gap := r.snapshot(1)
	if !gap {
		t.Fatalf("gap 예상. ticks=%d oldest=%d", len(ticks), oldest)
	}
	if oldest != 4 {
		t.Errorf("oldest=%d want 4", oldest)
	}
}

// fromSeq >= newest → ticks 비어있고 gap=false (live 로 이어감).
func TestAlgoRing_SnapshotAhead(t *testing.T) {
	r := newAlgoRing(5)
	for i := int64(1); i <= 5; i++ {
		r.push(mkQ("USDKRW", i))
	}
	// fromSeq=5 (마지막 것). 다음 6 요청 → ring 에 6 없음 → 비어있고 gap=false.
	ticks, _, gap := r.snapshot(5)
	if gap {
		t.Fatalf("gap 예상 안 함 — client 가 서버보다 앞선 경우가 아니라 딱 맞음")
	}
	if len(ticks) != 0 {
		t.Fatalf("ticks=%d want 0", len(ticks))
	}
}
