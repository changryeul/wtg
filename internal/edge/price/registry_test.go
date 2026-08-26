package price

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mock subscriber — websocket.Conn 없이 send queue 만 검증.
func mkSubMock(queueSize int) *Subscriber {
	return &Subscriber{
		id:     subIDSeq.Add(1),
		send:   make(chan []byte, queueSize),
		closeC: make(chan struct{}),
		logger: discardLogger(),
	}
}

func TestSubscriberSend(t *testing.T) {
	s := mkSubMock(2)
	if err := s.Send([]byte("a")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := s.Send([]byte("b")); err != nil {
		t.Fatalf("send2: %v", err)
	}
	// 큐 가득 → ErrSendQueueFull.
	if err := s.Send([]byte("c")); !errors.Is(err, ErrSendQueueFull) {
		t.Errorf("ErrSendQueueFull 기대, got %v", err)
	}
}

func TestSubscriberSendOnClosed(t *testing.T) {
	s := mkSubMock(2)
	s.Close()
	if err := s.Send([]byte("x")); !errors.Is(err, ErrSubClosed) {
		t.Errorf("ErrSubClosed 기대, got %v", err)
	}
}

func TestSubscriberCloseIdempotent(t *testing.T) {
	s := mkSubMock(2)
	var calls atomic.Int32
	s.onClose = func(*Subscriber) { calls.Add(1) }
	s.Close()
	s.Close() // 두 번째는 no-op.
	if calls.Load() != 1 {
		t.Errorf("onClose: %d, want 1", calls.Load())
	}
}

func TestRegistryAddBroadcastRemove(t *testing.T) {
	r := NewRegistry(discardLogger())
	s1 := mkSubMock(4)
	s2 := mkSubMock(4)
	r.Add(s1)
	r.Add(s2)

	if r.Count() != 2 {
		t.Errorf("Count: %d", r.Count())
	}
	sent, dropped := r.Broadcast([]byte("tick"))
	if sent != 2 || dropped != 0 {
		t.Errorf("Broadcast: sent=%d dropped=%d", sent, dropped)
	}
	if len(s1.send) != 1 || len(s2.send) != 1 {
		t.Errorf("send queue: s1=%d s2=%d", len(s1.send), len(s2.send))
	}

	r.Remove(s1)
	if r.Count() != 1 {
		t.Errorf("Count after remove: %d", r.Count())
	}
}

func TestRegistrySlowConsumerClose(t *testing.T) {
	r := NewRegistry(discardLogger())
	slow := mkSubMock(1)
	fast := mkSubMock(4)
	r.Add(slow)
	r.Add(fast)

	// 첫 broadcast — slow 큐 가득.
	r.Broadcast([]byte("tick1"))
	// 두 번째 broadcast — slow 는 ErrSendQueueFull → Close.
	r.Broadcast([]byte("tick2"))

	if !slow.IsClosed() {
		t.Error("slow consumer 가 close 안 됨")
	}
	if fast.IsClosed() {
		t.Error("fast consumer 가 잘못 close 됨")
	}
}

func TestRegistryStats(t *testing.T) {
	r := NewRegistry(discardLogger())
	r.Add(mkSubMock(4))
	r.Add(mkSubMock(4))
	r.Broadcast([]byte("x"))

	stats := r.Stats()
	if stats.Count != 2 {
		t.Errorf("Count: %d", stats.Count)
	}
	if stats.Sent != 2 {
		t.Errorf("Sent: %d", stats.Sent)
	}
}

func TestRegistryCloseAll(t *testing.T) {
	r := NewRegistry(discardLogger())
	s1 := mkSubMock(4)
	s2 := mkSubMock(4)
	r.Add(s1)
	r.Add(s2)
	r.CloseAll()
	if !s1.IsClosed() || !s2.IsClosed() {
		t.Error("CloseAll 후 모두 close 여야 함")
	}
}

// 진단 — Snapshot 빈 registry.
func TestRegistrySnapshot_Empty(t *testing.T) {
	r := NewRegistry(discardLogger())
	snap := r.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot 빈 registry 에 nil 반환 — JSON null 위험")
	}
	if len(snap) != 0 {
		t.Errorf("len = %d, want 0", len(snap))
	}
}

// Subscriber.SnapshotInfo — 모든 필드 검증.
func TestSubscriberSnapshotInfo(t *testing.T) {
	s := mkSubMock(64)
	s.profileKey = "WEB.BRANCH.VIP"
	s.customerID = "cust_001"
	// queue 에 3개 enqueue → QueueDepth=3.
	for i := 0; i < 3; i++ {
		s.send <- []byte("payload")
	}
	info := s.SnapshotInfo()
	if info.ID != s.id {
		t.Errorf("ID = %d, want %d", info.ID, s.id)
	}
	if info.ProfileKey != "WEB.BRANCH.VIP" {
		t.Errorf("ProfileKey = %q", info.ProfileKey)
	}
	if info.CustomerID != "cust_001" {
		t.Errorf("CustomerID = %q", info.CustomerID)
	}
	if info.QueueDepth != 3 || info.QueueCap != 64 {
		t.Errorf("Queue = %d/%d, want 3/64", info.QueueDepth, info.QueueCap)
	}
	if info.Pairs != nil {
		t.Errorf("초기 Pairs 는 nil (all 모드) 여야 — got %+v", info.Pairs)
	}
	if info.Closed {
		t.Error("새 Subscriber 가 Closed=true")
	}
}

// Registry.Snapshot 가 등록된 각 Subscriber 의 SnapshotInfo 반환.
func TestRegistrySnapshot_Populated(t *testing.T) {
	r := NewRegistry(discardLogger())
	s1 := mkSubMock(32)
	s1.profileKey = "WEB.HQ.VIP"
	s1.customerID = "cust_A"
	s2 := mkSubMock(32)
	s2.profileKey = "MOB.BRANCH.STD"
	r.Add(s1)
	r.Add(s2)
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len = %d, want 2", len(snap))
	}
	// id → info map 으로 검증 (순서 보장 X).
	byID := map[uint64]SubscriberInfo{}
	for _, info := range snap {
		byID[info.ID] = info
	}
	if byID[s1.id].ProfileKey != "WEB.HQ.VIP" || byID[s1.id].CustomerID != "cust_A" {
		t.Errorf("s1 정보 미반영: %+v", byID[s1.id])
	}
	if byID[s2.id].ProfileKey != "MOB.BRANCH.STD" || byID[s2.id].CustomerID != "" {
		t.Errorf("s2 정보 미반영: %+v", byID[s2.id])
	}
}

// per-source 라우팅 — 같은 customerID 라도 BEST 화면(source="") 과 LP별 화면
// (source="SHB") 이 source 태그로 분리 수신한다.
func TestSendByCustomerIDV_SourceRouting(t *testing.T) {
	r := NewRegistry(discardLogger())

	best := &Subscriber{ // 원천 미선택 → BEST 만
		id: subIDSeq.Add(1), customerID: "U1", send: make(chan []byte, 4),
		closeC: make(chan struct{}), logger: discardLogger(),
	}
	shb := &Subscriber{ // SHB 선택 → SHB per-source 만
		id: subIDSeq.Add(1), customerID: "U1", sources: map[string]struct{}{"SHB": {}},
		send: make(chan []byte, 4), closeC: make(chan struct{}), logger: discardLogger(),
	}
	r.Add(best)
	r.Add(shb)

	// BEST quote (source="") → best 만.
	if sent, _ := r.SendByCustomerIDV("U1", "USD/KRW", "", []byte("BEST"), nil); sent != 1 {
		t.Fatalf("BEST sent=%d, want 1", sent)
	}
	if got := string(<-best.send); got != "BEST" {
		t.Errorf("best 수신 %q, want BEST", got)
	}
	select {
	case p := <-shb.send:
		t.Errorf("SHB 화면이 BEST quote 수신: %q", p)
	default:
	}

	// SHB per-source quote → shb 만.
	if sent, _ := r.SendByCustomerIDV("U1", "USD/KRW", "SHB", []byte("SHBQ"), nil); sent != 1 {
		t.Fatalf("SHB sent=%d, want 1", sent)
	}
	if got := string(<-shb.send); got != "SHBQ" {
		t.Errorf("shb 수신 %q, want SHBQ", got)
	}
	select {
	case p := <-best.send:
		t.Errorf("BEST 화면이 SHB quote 수신: %q", p)
	default:
	}

	// 아무도 안 고른 KMB → 0.
	if sent, _ := r.SendByCustomerIDV("U1", "USD/KRW", "KMB", []byte("KMBQ"), nil); sent != 0 {
		t.Errorf("KMB sent=%d, want 0 (구독자 없음)", sent)
	}
}

// customerRegManager: 같은 customerID 의 다중 연결이 원천 합집합으로 등록되고,
// 한 연결이 닫혀도 남은 연결의 원천은 유지된다 (refcount).
func TestCustomerRegManager_SourceUnionRefcount(t *testing.T) {
	mgr := &customerRegManager{
		logger: discardLogger(),
		active: make(map[string]*custRegState),
		queue:  make(chan *wtgpb.CustomerRegistration, 16),
	}
	// BEST 화면 + SHB 화면 (같은 customerID).
	mgr.Register("U1", "WEB.HQ.VIP", nil)
	mgr.Register("U1", "WEB.HQ.VIP", []string{"SHB"})

	mgr.amu.Lock()
	st := mgr.active["U1"]
	if st == nil || st.conns != 2 {
		mgr.amu.Unlock()
		t.Fatalf("conns=%v, want 2", st)
	}
	if _, ok := st.srcRefs["SHB"]; !ok || st.srcRefs["SHB"] != 1 {
		mgr.amu.Unlock()
		t.Fatalf("srcRefs=%v, want SHB:1", st.srcRefs)
	}
	mgr.amu.Unlock()

	// SHB 화면 닫힘 → conns=1, SHB refcount 0 → 제거. 하지만 U1 등록은 유지.
	mgr.Unregister("U1", []string{"SHB"})
	mgr.amu.Lock()
	st = mgr.active["U1"]
	if st == nil || st.conns != 1 {
		mgr.amu.Unlock()
		t.Fatalf("SHB 닫힌 후 conns=%v, want 1 (U1 유지)", st)
	}
	if len(st.srcRefs) != 0 {
		mgr.amu.Unlock()
		t.Fatalf("srcRefs=%v, want 비어있음", st.srcRefs)
	}
	mgr.amu.Unlock()

	// 마지막 BEST 화면도 닫힘 → U1 완전 제거.
	mgr.Unregister("U1", nil)
	mgr.amu.Lock()
	_, exists := mgr.active["U1"]
	mgr.amu.Unlock()
	if exists {
		t.Error("모든 연결 닫힌 후에도 U1 등록 남음")
	}
}

// per-source(LP별) 구독자는 BEST profile 브로드캐스트를 받지 않는다 (자기 LP만).
func TestSendByProfileV_ExcludesPerSourceSubs(t *testing.T) {
	r := NewRegistry(discardLogger())
	best := &Subscriber{ // 원천 미선택 → profile BEST 수신
		id: subIDSeq.Add(1), profileKey: "WEB.HQ.VIP", send: make(chan []byte, 4),
		closeC: make(chan struct{}), logger: discardLogger(),
	}
	lp := &Subscriber{ // SHB 선택 → profile BEST 제외
		id: subIDSeq.Add(1), profileKey: "WEB.HQ.VIP", sources: map[string]struct{}{"SHB": {}},
		send: make(chan []byte, 4), closeC: make(chan struct{}), logger: discardLogger(),
	}
	r.Add(best)
	r.Add(lp)

	sent, _ := r.SendByProfileV("WEB.HQ.VIP", "USD/KRW", []byte("V1"), nil)
	if sent != 1 {
		t.Fatalf("profile BEST sent=%d, want 1 (best 만, LP화면 제외)", sent)
	}
	if got := string(<-best.send); got != "V1" {
		t.Errorf("best 수신 %q, want V1", got)
	}
	select {
	case p := <-lp.send:
		t.Errorf("LP화면이 BEST profile quote 수신: %q", p)
	default:
	}
}
