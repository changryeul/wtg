package price

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// Subscriber 는 단일 ws 클라이언트의 fan-out 큐 + lifecycle.
//
// 시세는 broker fan-out 후 모든 web 클라이언트에 same payload 를 보내므로
// mci-push 의 Connection 보다 단순하다 (사용자별 매핑 불필요).
type Subscriber struct {
	id     uint64
	conn   *websocket.Conn
	send   chan []byte
	closed atomic.Bool
	closeC chan struct{}
	logger *slog.Logger

	onClose func(*Subscriber)
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
		id:      subIDSeq.Add(1),
		conn:    ws,
		send:    make(chan []byte, opts.SendQueueSize),
		closeC:  make(chan struct{}),
		onClose: opts.OnClose,
	}
	s.logger = opts.Logger.With(slog.Uint64("sub_id", s.id))
	return s
}

// Send 는 페이로드를 send queue 에 enqueue.
func (s *Subscriber) Send(p []byte) error {
	if s.closed.Load() {
		return ErrSubClosed
	}
	select {
	case s.send <- p:
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
