package price

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/quote"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func mkQ(sym string, seq int64) *wtgpb.AlgoQuote {
	return &wtgpb.AlgoQuote{Sym: sym, Seq: seq, Bid: 1.0, Ask: 1.1}
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

// Phase C — slow client 감지: firstDropAt 이 timeout 지나면 evictSlowSubs 가
// sub.done 을 close 하고 카운터 증가.
// OnTick 이 cross tick(SourceCross, 예: CNH/KRW 재정환율)도 받아 emit 한다
// (mds refprctype=4 cross-mid 대응 — CNHKRW=USDKRW/USDCNH 는 CrossRateConsumer
// 가 SourceCross 로 emit).
func TestAlgoServer_OnTickAcceptsCross(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{RingSize: 8})
	defer s.Stop()

	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: "CNHKRW", Bid: 190.10, Ask: 190.30, Src: SourceCross, TS: time.Now().UTC(),
	})
	s.OnTick(&Tick{Symbol: "CNHKRW", Source: SourceCross, Body: body, Received: time.Now()})

	ring := s.ringFor("CNHKRW")
	if ring == nil {
		t.Fatal("cross tick 이 AlgoStream 에 안 들어옴 (SourceCross drop)")
	}
	ticks, _, _ := ring.snapshot(0)
	if len(ticks) != 1 {
		t.Fatalf("ring %d개, want 1", len(ticks))
	}
	if ticks[0].GetBid() != 190.10 || ticks[0].GetMid() != 190.20 {
		t.Errorf("cross AlgoQuote bid=%v mid=%v, want 190.10/190.20",
			ticks[0].GetBid(), ticks[0].GetMid())
	}
}

// OnTick 이 mid = (bid+ask)/2 를 계산해 넣는다 (mds mdquot_calc_mid 대응,
// refprctype=2). 반올림 없음.
func TestAlgoServer_OnTickComputesMid(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{RingSize: 8})
	defer s.Stop()

	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: "USDKRW", Bid: 1380.00, Ask: 1380.10, Src: SourceBest, TS: time.Now().UTC(),
	})
	s.OnTick(&Tick{Symbol: "USDKRW", Source: SourceBest, Body: body, Received: time.Now()})

	ticks, _, _ := s.ringFor("USDKRW").snapshot(0)
	if len(ticks) != 1 {
		t.Fatalf("ring %d개, want 1", len(ticks))
	}
	if got := ticks[0].GetMid(); got != 1380.05 {
		t.Errorf("mid=%v, want 1380.05 ((bid+ask)/2)", got)
	}
}

// OnTick 이 BEST tick 의 체결가(last)를 AlgoQuote 로 전달한다 (mds fillprc 대응).
func TestAlgoServer_OnTickCarriesLast(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{RingSize: 8})
	defer s.Stop()

	body, _ := json.Marshal(quote.JSONEnvelope{
		Sym: "USDKRW", Bid: 1380.00, Ask: 1380.10, Src: SourceBest,
		TS: time.Now().UTC(), Last: 1380.05, LastQty: 500000,
	})
	s.OnTick(&Tick{
		Symbol: "USDKRW", Source: SourceBest, Body: body, Received: time.Now(),
	})

	ring := s.ringFor("USDKRW")
	if ring == nil {
		t.Fatal("ring 없음")
	}
	ticks, _, _ := ring.snapshot(0)
	if len(ticks) != 1 {
		t.Fatalf("ring %d개, want 1", len(ticks))
	}
	if ticks[0].GetLast() != 1380.05 || ticks[0].GetLastQty() != 500000 {
		t.Errorf("AlgoQuote last=%v qty=%v, want 1380.05/500000",
			ticks[0].GetLast(), ticks[0].GetLastQty())
	}
}

func TestAlgoServer_EvictSlowClient(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{
		RingSize:          10,
		ClientBufferSize:  1,
		SlowClientTimeout: 100 * time.Millisecond,
	})
	defer s.Stop()

	sub := &algoSub{
		clientID:  "slow-test",
		symbolSet: map[string]struct{}{},
		ch:        make(chan *wtgpb.AlgoQuote, 1),
		done:      make(chan struct{}),
	}
	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// 인위적으로 200ms 전 firstDropAt — timeout(100ms) 초과 상태.
	sub.firstDropAt.Store(time.Now().Add(-200 * time.Millisecond).UnixNano())

	s.evictSlowSubs()

	select {
	case <-sub.done:
		// OK — close 됨
	case <-time.After(10 * time.Millisecond):
		t.Fatal("done 이 close 안 됨 (evict 실패)")
	}
	if got := s.disconnectedSlow.Load(); got != 1 {
		t.Errorf("disconnectedSlow=%d want 1", got)
	}
	if !sub.slowFired.Load() {
		t.Errorf("slowFired=false want true")
	}
}

// Phase C — firstDropAt 이 있지만 timeout 미만이면 evict 안 함.
func TestAlgoServer_EvictSkippedIfWithinTimeout(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{
		SlowClientTimeout: 500 * time.Millisecond,
	})
	defer s.Stop()

	sub := &algoSub{
		clientID:  "borderline",
		symbolSet: map[string]struct{}{},
		ch:        make(chan *wtgpb.AlgoQuote, 1),
		done:      make(chan struct{}),
	}
	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// 50ms 전 firstDropAt — timeout(500ms) 미만 → evict 안 해야.
	sub.firstDropAt.Store(time.Now().Add(-50 * time.Millisecond).UnixNano())

	s.evictSlowSubs()

	select {
	case <-sub.done:
		t.Fatal("done 이 잘못 close 됨 (아직 timeout 안 됨)")
	default:
	}
	if got := s.disconnectedSlow.Load(); got != 0 {
		t.Errorf("disconnectedSlow=%d want 0", got)
	}
}

// Phase C — firstDropAt=0 (정상 상태) 이면 skip.
func TestAlgoServer_EvictSkippedIfHealthy(t *testing.T) {
	s := NewAlgoStreamServer(nil, AlgoStreamOptions{
		SlowClientTimeout: 100 * time.Millisecond,
	})
	defer s.Stop()

	sub := &algoSub{
		clientID:  "healthy",
		symbolSet: map[string]struct{}{},
		ch:        make(chan *wtgpb.AlgoQuote, 1),
		done:      make(chan struct{}),
	}
	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	// firstDropAt=0 — 정상.
	s.evictSlowSubs()

	select {
	case <-sub.done:
		t.Fatal("정상 sub 인데 done close 됨")
	default:
	}
	if got := s.disconnectedSlow.Load(); got != 0 {
		t.Errorf("disconnectedSlow=%d want 0", got)
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
