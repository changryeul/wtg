// Package futures (internal) — 선물시세 종목 구독 fan-out Hub.
//
// mci-edge-price 의 Subscriber/Registry(종목 필터 + backpressure 격리) 패턴을 선물용
// 으로 격리 구현한다 (FX 코드 무변경). 클라는 종목(code)을 구독하고, 구독한 종목의
// fut.* JSON 만 받는다. 구독 안 한(all 모드) 클라는 전체 수신.
//
// wire/codec 은 pkg/futures (KA→JSON) 가 담당 — Hub 는 []byte(JSON)를 code 로 라우팅.
package krx

import (
	"errors"
	"sort"
	"sync"
)

// ErrSendQueueFull — subscriber 송신 큐 포화 (slow consumer).
var ErrSendQueueFull = errors.New("krx: send queue full")

// Subscriber 는 단일 ws 클라이언트의 fan-out 큐 + 종목 필터.
// filter == nil 이면 "all 모드"(구독 전 or 전체구독). 아니면 그 종목만 매칭.
type Subscriber struct {
	id     string
	sendQ  chan []byte
	mu     sync.RWMutex
	filter map[string]struct{} // nil = all
	closed bool
}

// NewSubscriber — id + 송신 큐 깊이.
func NewSubscriber(id string, queueDepth int) *Subscriber {
	if queueDepth <= 0 {
		queueDepth = 256
	}
	return &Subscriber{id: id, sendQ: make(chan []byte, queueDepth)}
}

// ID 반환.
func (s *Subscriber) ID() string { return s.id }

// Out 은 송신 큐 (writer goroutine 이 ws 로 flush).
func (s *Subscriber) Out() <-chan []byte { return s.sendQ }

// Matches — 이 subscriber 가 code 시세를 받기로 했는지. filter nil = all → true.
func (s *Subscriber) Matches(code string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.filter == nil {
		return true
	}
	_, ok := s.filter[code]
	return ok
}

// Subscribe — code 들을 필터에 추가 (idempotent). 첫 호출 시 all→filtered 전환.
func (s *Subscriber) Subscribe(codes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filter == nil {
		s.filter = map[string]struct{}{}
	}
	for _, c := range codes {
		if c != "" {
			s.filter[c] = struct{}{}
		}
	}
}

// Unsubscribe — code 제거. 결과가 비면 nil(all 모드)로 되돌린다.
func (s *Subscriber) Unsubscribe(codes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filter == nil {
		return
	}
	for _, c := range codes {
		delete(s.filter, c)
	}
	if len(s.filter) == 0 {
		s.filter = nil
	}
}

// Subscribed — 현재 필터 스냅샷 (정렬). nil = all 모드(빈 슬라이스).
func (s *Subscriber) Subscribed() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.filter == nil {
		return nil
	}
	out := make([]string, 0, len(s.filter))
	for c := range s.filter {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// send — 큐에 non-blocking enqueue. 포화면 ErrSendQueueFull (slow consumer 격리).
func (s *Subscriber) send(p []byte) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("krx: subscriber closed")
	}
	s.mu.RUnlock()
	select {
	case s.sendQ <- p:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Close — 큐 종료.
func (s *Subscriber) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.sendQ)
	}
}

// Hub 는 subscriber 집합 + 종목 라우팅.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]*Subscriber
}

// NewHub — 빈 Hub.
func NewHub() *Hub { return &Hub{subs: map[string]*Subscriber{}} }

// Add / Remove — subscriber 등록/해제.
func (h *Hub) Add(s *Subscriber) {
	h.mu.Lock()
	h.subs[s.id] = s
	h.mu.Unlock()
}
func (h *Hub) Remove(id string) {
	h.mu.Lock()
	delete(h.subs, id)
	h.mu.Unlock()
}

// Count — 등록 subscriber 수.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// BroadcastBySymbol — code 시세 JSON 을 구독(Matches) subscriber 에게만 fan-out.
// 반환: 송신 성공 수 / drop 수(포화·종료). slow consumer 는 격리(전체 지연 없음).
func (h *Hub) BroadcastBySymbol(code string, p []byte) (sent, dropped int) {
	h.mu.RLock()
	snapshot := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		if s.Matches(code) {
			snapshot = append(snapshot, s)
		}
	}
	h.mu.RUnlock()

	for _, s := range snapshot {
		if err := s.send(p); err != nil {
			dropped++
			continue
		}
		sent++
	}
	return sent, dropped
}
