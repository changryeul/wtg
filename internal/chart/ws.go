package chart

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// Subscriber 는 단일 ws 클라이언트의 lifecycle + send queue + (pair, tf) 필터.
//
// 챠트 화면이 통화쌍·timeframe 조합으로 구독 — 필터가 매칭되는 봉만 송신.
type Subscriber struct {
	id     uint64
	conn   *websocket.Conn
	send   chan []byte
	closed atomic.Bool
	closeC chan struct{}
	logger *slog.Logger

	mu    sync.RWMutex
	pairs map[string]struct{} // 빈 set = 모든 pair
	tfs   map[string]struct{} // 빈 set = 모든 tf

	onClose func(*Subscriber)
}

var (
	subIDSeq atomic.Uint64

	ErrSubClosed     = errors.New("chart: subscriber 종료됨")
	ErrSendQueueFull = errors.New("chart: send queue 가득")
)

// SubscriberOptions 는 Subscriber 생성 옵션.
type SubscriberOptions struct {
	SendQueueSize int
	Logger        *slog.Logger
	OnClose       func(*Subscriber)
}

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
		pairs:   make(map[string]struct{}),
		tfs:     make(map[string]struct{}),
		onClose: opts.OnClose,
	}
	s.logger = opts.Logger.With(slog.Uint64("sub_id", s.id))
	return s
}

// SetFilters 는 (pairs, tfs) 필터를 통째로 교체한다 (subscribe 메시지 처리용).
// 빈 슬라이스/nil → 모든 항목 매칭 (filter 해제).
func (s *Subscriber) SetFilters(pairs, tfs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairs = make(map[string]struct{}, len(pairs))
	for _, p := range pairs {
		s.pairs[p] = struct{}{}
	}
	s.tfs = make(map[string]struct{}, len(tfs))
	for _, t := range tfs {
		s.tfs[t] = struct{}{}
	}
}

// Matches 는 (pair, tf) 가 현재 필터를 통과하는지 반환.
func (s *Subscriber) Matches(pair, tf string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.pairs) > 0 {
		if _, ok := s.pairs[pair]; !ok {
			return false
		}
	}
	if len(s.tfs) > 0 {
		if _, ok := s.tfs[tf]; !ok {
			return false
		}
	}
	return true
}

// Send 는 페이로드를 send queue 에 enqueue (non-blocking).
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

func (s *Subscriber) IsClosed() bool { return s.closed.Load() }

// Hub 는 모든 활성 ws subscriber 의 집합 + (pair, tf) 매칭 fan-out.
type Hub struct {
	mu     sync.RWMutex
	subs   map[uint64]*Subscriber
	logger *slog.Logger

	totalSent atomic.Uint64
	totalDrop atomic.Uint64
}

func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		subs:   make(map[uint64]*Subscriber),
		logger: logger,
	}
}

func (h *Hub) Add(s *Subscriber) {
	h.mu.Lock()
	h.subs[s.id] = s
	h.mu.Unlock()
}

func (h *Hub) Remove(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s.id)
	h.mu.Unlock()
}

// Publish 는 (pair, tf) 매칭 subscriber 에게 payload 송신.
func (h *Hub) Publish(pair, tf string, payload []byte) (sent, dropped int) {
	h.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		if s.Matches(pair, tf) {
			snapshot = append(snapshot, s)
		}
	}
	h.mu.RUnlock()

	for _, s := range snapshot {
		if err := s.Send(payload); err != nil {
			dropped++
			if errors.Is(err, ErrSendQueueFull) {
				h.logger.Warn("slow chart consumer 격리",
					slog.Uint64("sub_id", s.id),
				)
				s.Close()
			}
			continue
		}
		sent++
	}
	h.totalSent.Add(uint64(sent))
	h.totalDrop.Add(uint64(dropped))
	return sent, dropped
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

type HubStats struct {
	Count   int    `json:"count"`
	Sent    uint64 `json:"sent"`
	Dropped uint64 `json:"dropped"`
}

func (h *Hub) Stats() HubStats {
	return HubStats{
		Count:   h.Count(),
		Sent:    h.totalSent.Load(),
		Dropped: h.totalDrop.Load(),
	}
}

func (h *Hub) CloseAll() {
	h.mu.RLock()
	all := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		all = append(all, s)
	}
	h.mu.RUnlock()
	for _, s := range all {
		s.Close()
	}
}
