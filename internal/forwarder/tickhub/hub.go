// Package tickhub — forwarder 의 tick fan-out 허브 (시세 gRPC-only HA).
//
// broker FANOUT 을 대체: dial-in 한 mci-price 구독자(SubscribeTicks stream)들을
// Hub 가 들고, worker 가 parse+batch 한 envelope 를 Broadcast 로 전체에 push.
// slow consumer(느린 mci-price)는 자동 격리 — 남은 인스턴스를 안 막는다.
//
// mci-edge-price 의 Registry/Broadcast/eviction 과 동일 패턴 (검증된 구조 재사용).
// gRPC 서버 핸들러가 SubscribeTicks 마다 Subscriber 를 만들어 Add, 스트림 종료 시 Close.
package tickhub

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ErrSendQueueFull — 구독자 send 큐가 가득 (slow consumer).
var ErrSendQueueFull = errors.New("tickhub: send queue 가득")

var subIDSeq atomic.Uint64

// SubscriberOptions — Subscriber 생성 옵션.
type SubscriberOptions struct {
	SendQueueSize int // default 256
	OnClose       func(*Subscriber)
}

// Subscriber — dial-in 한 mci-price 하나. gRPC 핸들러가 C() 를 읽어 stream.Send.
type Subscriber struct {
	id      uint64
	send    chan []byte
	closeC  chan struct{}
	closed  atomic.Bool
	onClose func(*Subscriber)
}

// NewSubscriber — Subscriber 구성 (Hub.Add 는 caller).
func NewSubscriber(opts SubscriberOptions) *Subscriber {
	if opts.SendQueueSize <= 0 {
		opts.SendQueueSize = 256
	}
	return &Subscriber{
		id:      subIDSeq.Add(1),
		send:    make(chan []byte, opts.SendQueueSize),
		closeC:  make(chan struct{}),
		onClose: opts.OnClose,
	}
}

// ID — 구독자 식별자.
func (s *Subscriber) ID() uint64 { return s.id }

// C — 발행 payload 채널 (gRPC 핸들러가 range 로 소비해 stream.Send).
func (s *Subscriber) C() <-chan []byte { return s.send }

// Done — Close 신호 채널 (gRPC 핸들러가 select 로 종료 감지).
func (s *Subscriber) Done() <-chan struct{} { return s.closeC }

// Send — payload enqueue (non-blocking). 가득 차면 ErrSendQueueFull.
func (s *Subscriber) Send(p []byte) error {
	if s.closed.Load() {
		return ErrSendQueueFull
	}
	select {
	case s.send <- p:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close — idempotent 정리. onClose 로 Hub 에서 제거.
func (s *Subscriber) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.closeC)
	if s.onClose != nil {
		s.onClose(s)
	}
}

// IsClosed — 상태 조회.
func (s *Subscriber) IsClosed() bool { return s.closed.Load() }

// Hub — 활성 구독자 모음. 시세는 broadcast 모델이라 단순 map + lock.
type Hub struct {
	mu     sync.RWMutex
	subs   map[uint64]*Subscriber
	logger *slog.Logger

	totalSent atomic.Uint64
	totalDrop atomic.Uint64
}

// New — 빈 Hub.
func New(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{subs: make(map[uint64]*Subscriber), logger: logger}
}

// Add — 구독자 등록. onClose 를 Hub.Remove 로 자동 연결 (자기 정리).
func (h *Hub) Add(s *Subscriber) {
	prev := s.onClose
	s.onClose = func(sub *Subscriber) {
		h.Remove(sub)
		if prev != nil {
			prev(sub)
		}
	}
	h.mu.Lock()
	h.subs[s.id] = s
	h.mu.Unlock()
}

// Remove — 구독자 제거 (idempotent).
func (h *Hub) Remove(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s.id)
	h.mu.Unlock()
}

// Count — 현재 구독자 수.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Broadcast — 모든 구독자에 payload push. slow consumer 는 격리(Close).
func (h *Hub) Broadcast(p []byte) (sent, dropped int) {
	h.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		snapshot = append(snapshot, s)
	}
	h.mu.RUnlock()

	for _, s := range snapshot {
		if err := s.Send(p); err == nil {
			sent++
			continue
		}
		dropped++
		if h.logger != nil {
			h.logger.Warn("slow consumer 격리 (tick 허브)", slog.Uint64("sub_id", s.id))
		}
		s.Close() // Add 가 연결한 onClose → Hub.Remove
	}
	h.totalSent.Add(uint64(sent))
	h.totalDrop.Add(uint64(dropped))
	return sent, dropped
}

// Stats — 누적 카운터 (관측).
func (h *Hub) Stats() (sent, dropped uint64) {
	return h.totalSent.Load(), h.totalDrop.Load()
}
