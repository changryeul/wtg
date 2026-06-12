package price

import (
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ─── Backpressure 자동 alert ─────────────────────────────────────────────
//
// Subscriber.Send 가 enqueue 성공 직후 큐 점유율을 검사. 80% 넘으면 1분당
// 1회 WARN — drop 직전 단계 (ErrSendQueueFull → slow consumer 격리 전) 에
// 발견할 수 있도록. mci-price 의 checkBackpressure 와 같은 패턴.

const backpressureRatio = 0.8
const backpressureInterval = 60 * time.Second

var backpressureGate sync.Map // map[string]*atomic.Int64

// ─── Backpressure 통계 (N7) ─────────────────────────────────────────────
// 누적 카운터 + 최근 history. /v1/backpressure 로 노출.

const backpressureHistoryCap = 100

type BackpressureEvent struct {
	TS         time.Time `json:"ts"`
	SubID      uint64    `json:"sub_id"`
	ProfileKey string    `json:"profile_key"`
	Kind       string    `json:"kind"`
	QueueDepth int       `json:"queue_depth"`
	QueueCap   int       `json:"queue_cap"`
}

type BackpressureStats struct {
	TotalWarnings uint64              `json:"total_warnings"`
	HistoryCap    int                 `json:"history_cap"`
	Recent        []BackpressureEvent `json:"recent"`
}

var (
	backpressureWarnTotal atomic.Uint64
	backpressureHistMu    sync.Mutex
	backpressureHist      = make([]BackpressureEvent, 0, backpressureHistoryCap)
)

func recordBackpressureEvent(ev BackpressureEvent) {
	backpressureWarnTotal.Add(1)
	backpressureHistMu.Lock()
	if len(backpressureHist) < backpressureHistoryCap {
		backpressureHist = append(backpressureHist, ev)
	} else {
		copy(backpressureHist, backpressureHist[1:])
		backpressureHist[len(backpressureHist)-1] = ev
	}
	backpressureHistMu.Unlock()
}

// SnapshotBackpressureStats — 최신순 정렬된 snapshot.
func SnapshotBackpressureStats() BackpressureStats {
	backpressureHistMu.Lock()
	cp := make([]BackpressureEvent, len(backpressureHist))
	for i, j := 0, len(backpressureHist)-1; j >= 0; i, j = i+1, j-1 {
		cp[i] = backpressureHist[j]
	}
	backpressureHistMu.Unlock()
	return BackpressureStats{
		TotalWarnings: backpressureWarnTotal.Load(),
		HistoryCap:    backpressureHistoryCap,
		Recent:        cp,
	}
}

func checkBackpressure(logger *slog.Logger, queueLen, queueCap int, subID uint64, profileKey, kind string) {
	if queueCap == 0 || float64(queueLen) < backpressureRatio*float64(queueCap) {
		return
	}
	now := time.Now()
	key := kind + ":" + strconv.FormatUint(subID, 10)
	actual, loaded := backpressureGate.LoadOrStore(key, &atomic.Int64{})
	p := actual.(*atomic.Int64)
	if !loaded {
		p.Store(now.UnixNano())
		logger.Warn("backpressure 감지 — 큐 80% 도달",
			slog.Uint64("sub_id", subID),
			slog.String("profile", profileKey),
			slog.String("kind", kind),
			slog.Int("queue_depth", queueLen),
			slog.Int("queue_cap", queueCap),
		)
		recordBackpressureEvent(BackpressureEvent{
			TS: now, SubID: subID, ProfileKey: profileKey, Kind: kind,
			QueueDepth: queueLen, QueueCap: queueCap,
		})
		return
	}
	last := p.Load()
	if now.UnixNano()-last < backpressureInterval.Nanoseconds() {
		return
	}
	if p.CompareAndSwap(last, now.UnixNano()) {
		logger.Warn("backpressure 감지 — 큐 80% 도달",
			slog.Uint64("sub_id", subID),
			slog.String("profile", profileKey),
			slog.String("kind", kind),
			slog.Int("queue_depth", queueLen),
			slog.Int("queue_cap", queueCap),
		)
		recordBackpressureEvent(BackpressureEvent{
			TS: now, SubID: subID, ProfileKey: profileKey, Kind: kind,
			QueueDepth: queueLen, QueueCap: queueCap,
		})
	}
}

// Subscriber 는 단일 ws 클라이언트의 fan-out 큐 + lifecycle.
//
// 세 fan-out 모델 동시 지원:
//
//   - raw tick broadcast — profileKey 무관, 전체 송신 (Registry.Broadcast).
//   - quote per-profile  — profileKey 매칭만 수신 (Registry.SendByProfile).
//   - quote per-customer — Phase 4c. customerID 매칭만 수신
//     (Registry.SendByCustomerID). customer-specific 마진
//     적용된 quote 전용 경로.
//
// profileKey / customerID 는 ws upgrade 시 결정되며 이후 immutable.
type Subscriber struct {
	id         uint64
	profileKey string // 예: "WEB.BRANCH.VIP"; 빈값 = profile 매칭 quote 미수신
	customerID string // Phase 4c. 빈값 = customer-specific quote 미수신.
	conn       *websocket.Conn
	send       chan []byte
	closed     atomic.Bool
	closeC     chan struct{}
	logger     *slog.Logger

	onClose func(*Subscriber)

	// pair 필터 — Phase 1 per-ws subscription.
	//   nil           : 필터 없음, 그 Profile 의 모든 pair 수신 (기존 동작).
	//   non-empty set : 해당 pair 만 수신.
	//   subscribe → empty 로 떨어지면 nil 로 되돌려 "all" 모드 재진입.
	pairsMu sync.RWMutex
	pairs   map[string]struct{}
}

// ProfileKey 는 Subscriber 가 매칭될 quote profile key (immutable).
func (s *Subscriber) ProfileKey() string { return s.profileKey }

// CustomerID — Phase 4c. customer-specific quote 매칭에 사용 (immutable).
func (s *Subscriber) CustomerID() string { return s.customerID }

// MatchesPair — 이 subscriber 가 해당 pair 의 시세를 받기로 선언했는지.
// pairs 가 nil (subscribe 한 적 없거나 unsubscribe 로 비워짐) 이면 모두 매칭.
func (s *Subscriber) MatchesPair(pair string) bool {
	s.pairsMu.RLock()
	defer s.pairsMu.RUnlock()
	if s.pairs == nil {
		return true
	}
	_, ok := s.pairs[pair]
	return ok
}

// SubscribePairs — pair 들을 필터 셋에 추가 (idempotent). 처음 호출 시 "all"
// 모드에서 "filtered" 모드로 전환.
func (s *Subscriber) SubscribePairs(pairs []string) {
	if len(pairs) == 0 {
		return
	}
	s.pairsMu.Lock()
	defer s.pairsMu.Unlock()
	if s.pairs == nil {
		s.pairs = make(map[string]struct{}, len(pairs))
	}
	for _, p := range pairs {
		if p != "" {
			s.pairs[p] = struct{}{}
		}
	}
}

// UnsubscribePairs — pair 들을 필터 셋에서 제거. 결과가 empty 면 nil 로
// 되돌려 "all" 모드 재진입.
func (s *Subscriber) UnsubscribePairs(pairs []string) {
	if len(pairs) == 0 {
		return
	}
	s.pairsMu.Lock()
	defer s.pairsMu.Unlock()
	if s.pairs == nil {
		return
	}
	for _, p := range pairs {
		delete(s.pairs, p)
	}
	if len(s.pairs) == 0 {
		s.pairs = nil
	}
}

// SubscribedPairs — 현재 필터 셋 스냅샷 (정렬된 슬라이스). nil 은 "all 모드"
// 의미 — 빈 슬라이스가 아니라 nil 반환.
func (s *Subscriber) SubscribedPairs() []string {
	s.pairsMu.RLock()
	defer s.pairsMu.RUnlock()
	if s.pairs == nil {
		return nil
	}
	out := make([]string, 0, len(s.pairs))
	for p := range s.pairs {
		out = append(out, p)
	}
	sortStrings(out)
	return out
}

// sortStrings — 작은 helper. sort 패키지 임포트 회피 (이미 sync 만 사용).
func sortStrings(s []string) {
	// insertion sort — n <= 수십이라 충분.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

var subIDSeq atomic.Uint64

// 에러.
var (
	ErrSubClosed     = errors.New("edge-price: subscriber 종료됨")
	ErrSendQueueFull = errors.New("edge-price: send queue 가득")
)

// SubscriberOptions 는 Subscriber 생성 옵션.
type SubscriberOptions struct {
	SendQueueSize int
	Logger        *slog.Logger
	OnClose       func(*Subscriber)
	// ProfileKey 는 quote fan-out 매칭에 사용. 빈값이면 quote 미수신.
	ProfileKey string
	// CustomerID — Phase 4c. customer-specific quote 매칭. 빈값이면
	// customer-quote 미수신 (Profile-only quote 만).
	CustomerID string
}

// NewSubscriber 는 Subscriber 를 구성한다 (read/write goroutine 은 caller 가 가동).
func NewSubscriber(ws *websocket.Conn, opts SubscriberOptions) *Subscriber {
	if opts.SendQueueSize <= 0 {
		opts.SendQueueSize = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Subscriber{
		id:         subIDSeq.Add(1),
		profileKey: opts.ProfileKey,
		customerID: opts.CustomerID,
		conn:       ws,
		send:       make(chan []byte, opts.SendQueueSize),
		closeC:     make(chan struct{}),
		onClose:    opts.OnClose,
	}
	s.logger = opts.Logger.With(
		slog.Uint64("sub_id", s.id),
		slog.String("profile", s.profileKey),
		slog.String("customer_id", s.customerID),
	)
	return s
}

// Send 는 페이로드를 send queue 에 enqueue.
func (s *Subscriber) Send(p []byte) error {
	if s.closed.Load() {
		return ErrSubClosed
	}
	select {
	case s.send <- p:
		checkBackpressure(s.logger, len(s.send), cap(s.send), s.id, s.profileKey, "ws")
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close 는 idempotent 정리.
func (s *Subscriber) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.closeC)
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.onClose != nil {
		s.onClose(s)
	}
}

// IsClosed 는 외부 상태 조회.
func (s *Subscriber) IsClosed() bool { return s.closed.Load() }

// Registry 는 모든 활성 ws subscriber 의 모음.
//
// 시세는 broadcast 모델 (모든 사용자에게 same payload) 이므로 단순한 슬라이스
// + lock 이면 충분. mci-push 처럼 logon_id 매핑 불필요.
type Registry struct {
	mu     sync.RWMutex
	subs   map[uint64]*Subscriber
	logger *slog.Logger

	totalSent atomic.Uint64
	totalDrop atomic.Uint64
}

// NewRegistry 는 빈 Registry 생성.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		subs:   make(map[uint64]*Subscriber),
		logger: logger,
	}
}

// Add 는 신규 subscriber 등록.
func (r *Registry) Add(s *Subscriber) {
	r.mu.Lock()
	r.subs[s.id] = s
	r.mu.Unlock()
}

// Remove 는 subscriber 제거 (idempotent).
func (r *Registry) Remove(s *Subscriber) {
	r.mu.Lock()
	delete(r.subs, s.id)
	r.mu.Unlock()
}

// Broadcast 는 모든 활성 subscriber 에게 동일 payload 송신.
// slow consumer 는 자동 Close 격리.
func (r *Registry) Broadcast(p []byte) (sent, dropped int) {
	r.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		snapshot = append(snapshot, s)
	}
	r.mu.RUnlock()

	for _, s := range snapshot {
		err := s.Send(p)
		if err == nil {
			sent++
			continue
		}
		dropped++
		if errors.Is(err, ErrSendQueueFull) {
			r.logger.Warn("slow consumer 격리", slog.Uint64("sub_id", s.id))
			s.Close()
		}
	}
	r.totalSent.Add(uint64(sent))
	r.totalDrop.Add(uint64(dropped))
	return sent, dropped
}

// RevokePairFromAll — Phase 4b admin override. 모든 subscriber 의 pair 필터
// 에서 해당 pair 를 제거한다 (admin disallow-pair endpoint 에서 호출).
// 영향 받은 subscriber 수 반환. all 모드 (필터 nil) sub 는 영향 없음 —
// 이미 자기 set 에 그 pair 가 명시되지 않은 상태.
func (r *Registry) RevokePairFromAll(pair string) (affected int) {
	if pair == "" {
		return 0
	}
	r.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		snapshot = append(snapshot, s)
	}
	r.mu.RUnlock()

	for _, s := range snapshot {
		// pair 가 sub 의 필터에 실제 있을 때만 카운트.
		s.pairsMu.RLock()
		_, had := s.pairs[pair]
		s.pairsMu.RUnlock()
		if had {
			s.UnsubscribePairs([]string{pair})
			affected++
		}
	}
	return affected
}

// BroadcastForPair 는 pair 매칭 subscriber 에게만 송신한다 (profile 무관).
// stale / fresh 알림 같은 system message 전송에 사용. pair 의 sub 가 어떤
// profile 이든 그 pair 에 영향 받으므로 profile filter 는 적용 안 함.
//
// MatchesPair 가 nil 필터 (=all 모드) 도 true 반환하므로 subscribe 안 한
// sub 도 알림 수신 — 무해 (그 sub 는 아예 quote 안 받지만 시스템 알림은
// 받는 게 운영 일관성).
func (r *Registry) BroadcastForPair(pair string, p []byte) (sent, dropped int) {
	if pair == "" {
		return 0, 0
	}
	r.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		if s.MatchesPair(pair) {
			snapshot = append(snapshot, s)
		}
	}
	r.mu.RUnlock()

	for _, s := range snapshot {
		if err := s.Send(p); err == nil {
			sent++
		} else {
			dropped++
		}
	}
	return sent, dropped
}

// SendByProfile 는 (profileKey, pair) 매칭 subscriber 에게만 송신한다.
//   - profileKey 빈값 subscriber : 절대 받지 않음 (quote 미구독)
//   - pair 빈값 인자             : pair 필터 미적용 (backward-compat 경로)
//   - sub 가 pair 필터 셋이 nil  : 그 Profile 의 모든 pair 수신 (Phase 1 default)
//   - sub 가 pair 필터 셋 비어있지 않음 : pair 일치 subscriber 만
func (r *Registry) SendByProfile(profileKey, pair string, p []byte) (sent, dropped int) {
	if profileKey == "" {
		return 0, 0
	}
	r.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		if s.profileKey != profileKey {
			continue
		}
		if pair != "" && !s.MatchesPair(pair) {
			continue
		}
		snapshot = append(snapshot, s)
	}
	r.mu.RUnlock()

	for _, s := range snapshot {
		err := s.Send(p)
		if err == nil {
			sent++
			continue
		}
		dropped++
		if errors.Is(err, ErrSendQueueFull) {
			r.logger.Warn("slow quote consumer 격리",
				slog.Uint64("sub_id", s.id), slog.String("profile", profileKey))
			s.Close()
		}
	}
	r.totalSent.Add(uint64(sent))
	r.totalDrop.Add(uint64(dropped))
	return sent, dropped
}

// SendByCustomerID — Phase 4c. customer-tag 된 quote 를 customerID 매칭
// subscriber 에게만 송신.
//
//   - customerID 빈 인자 : 함수 자체 noop (잘못된 호출 방어).
//   - sub.customerID 빈값 : 절대 받지 않음 (customer 미등록 sub).
//   - pair 필터          : SendByProfile 와 동일 — MatchesPair 통과 시만.
//
// slow consumer 는 격리. 일반 quote (SendByProfile) 경로와 독립 — 한 subscriber
// 가 양쪽 매칭이면 두 종류의 quote 가 별도 메시지로 동시 도착할 수 있음.
func (r *Registry) SendByCustomerID(customerID, pair string, p []byte) (sent, dropped int) {
	if customerID == "" {
		return 0, 0
	}
	r.mu.RLock()
	snapshot := make([]*Subscriber, 0)
	for _, s := range r.subs {
		if s.customerID != customerID {
			continue
		}
		if pair != "" && !s.MatchesPair(pair) {
			continue
		}
		snapshot = append(snapshot, s)
	}
	r.mu.RUnlock()

	for _, s := range snapshot {
		err := s.Send(p)
		if err == nil {
			sent++
			continue
		}
		dropped++
		if errors.Is(err, ErrSendQueueFull) {
			r.logger.Warn("slow customer-quote consumer 격리",
				slog.Uint64("sub_id", s.id),
				slog.String("customer_id", customerID))
			s.Close()
		}
	}
	r.totalSent.Add(uint64(sent))
	r.totalDrop.Add(uint64(dropped))
	return sent, dropped
}

// Count 는 현재 활성 subscriber 수.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}

// Stats 는 누적 카운터.
type RegistryStats struct {
	Count   int    `json:"count"`
	Sent    uint64 `json:"sent"`
	Dropped uint64 `json:"dropped"`
}

func (r *Registry) Stats() RegistryStats {
	return RegistryStats{
		Count:   r.Count(),
		Sent:    r.totalSent.Load(),
		Dropped: r.totalDrop.Load(),
	}
}

// ─── 운영 진단 (/v1/connections) ─────────────────────────────────────────

// SubscriberInfo — 단일 ws 클라이언트의 sanitized 정보. 운영자가
// "지금 누가 연결되어 있나" 진단 목적. 큐 깊이 = backpressure 신호.
type SubscriberInfo struct {
	ID         uint64   `json:"id"`
	ProfileKey string   `json:"profile_key"` // 빈값 = quote 미구독 (raw tick only)
	CustomerID string   `json:"customer_id"` // 빈값 = customer-quote 미구독
	RemoteAddr string   `json:"remote_addr"` // ws upgrade 시점의 원격 IP:port
	QueueDepth int      `json:"queue_depth"` // send chan 의 len
	QueueCap   int      `json:"queue_cap"`   // send chan 의 cap
	Pairs      []string `json:"pairs"`       // nil = all 모드 (Phase 1 default)
	Closed     bool     `json:"closed"`
}

// SnapshotInfo — 자신의 진단 dump. registry 에서 호출.
func (s *Subscriber) SnapshotInfo() SubscriberInfo {
	addr := ""
	if s.conn != nil {
		if ra := s.conn.RemoteAddr(); ra != nil {
			addr = ra.String()
		}
	}
	return SubscriberInfo{
		ID:         s.id,
		ProfileKey: s.profileKey,
		CustomerID: s.customerID,
		RemoteAddr: addr,
		QueueDepth: len(s.send),
		QueueCap:   cap(s.send),
		Pairs:      s.SubscribedPairs(),
		Closed:     s.closed.Load(),
	}
}

// Snapshot — 현재 등록된 모든 subscriber 의 진단 dump. hot path 아님.
func (r *Registry) Snapshot() []SubscriberInfo {
	r.mu.RLock()
	out := make([]SubscriberInfo, 0, len(r.subs))
	for _, s := range r.subs {
		out = append(out, s.SnapshotInfo())
	}
	r.mu.RUnlock()
	return out
}

// CloseAll 은 모든 subscriber 일괄 종료 (서버 셧다운 시).
func (r *Registry) CloseAll() {
	r.mu.RLock()
	all := make([]*Subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		all = append(all, s)
	}
	r.mu.RUnlock()
	for _, s := range all {
		s.Close()
	}
}
