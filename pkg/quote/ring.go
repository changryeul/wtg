package quote

import (
	"sync"

	"github.com/winwaysystems/wtg/pkg/session"
)

// RingBuffer 는 심볼(Pair)별 최근 N tick 을 메모리에 보관하는 bounded ring.
//
// 용도:
//
//   - 챠트 초기 1프레임 응답 (DB hit 회피, 즉시 응답)
//   - 시세 재현 / 디버깅
//
// 동시성 모델:
//
//   - 1 writer (broker subscribe goroutine) + N readers (챠트 API).
//   - per-pair RWMutex — write 끼리는 직렬화 (같은 pair 의 producer 가 단일이라 사실상 무경쟁),
//     read 와 write 는 RWMutex 로 분리.
//   - 전체 map 의 동시 추가는 outer RWMutex 로 보호.
//
// 메모리 산정 (예시):
//
//   - cap=1000, Quote 약 40B → 40KB/pair
//   - 30 pair → ~1.2MB. 무시 가능.
type RingBuffer struct {
	capPerPair int

	mu    sync.RWMutex
	pairs map[session.Pair]*pairRing
}

// pairRing 은 단일 pair 의 ring slot.
type pairRing struct {
	mu   sync.RWMutex
	data []Quote
	next int  // 다음 write 위치 (0..cap-1)
	full bool // 한 바퀴 돌았는지
}

// NewRingBuffer 는 pair 당 capPerPair tick 을 보관하는 RingBuffer 를 생성한다.
// capPerPair <= 0 이면 panic — 호출자 실수.
func NewRingBuffer(capPerPair int) *RingBuffer {
	if capPerPair <= 0 {
		panic("quote: RingBuffer capacity must be > 0")
	}
	return &RingBuffer{
		capPerPair: capPerPair,
		pairs:      make(map[session.Pair]*pairRing),
	}
}

// Add 는 새 tick 한 건을 ring 에 추가한다.
// 가득 차면 가장 오래된 tick 이 자동으로 밀려난다.
func (r *RingBuffer) Add(q Quote) {
	ring := r.getOrCreate(q.Pair)
	ring.mu.Lock()
	ring.data[ring.next] = q
	ring.next++
	if ring.next >= r.capPerPair {
		ring.next = 0
		ring.full = true
	}
	ring.mu.Unlock()
}

// Snapshot 은 해당 pair 의 ring 을 chronological order (오래된 것 → 최신) 로
// 반환한다. max <= 0 이면 가용한 전체. max > 보유량 이면 보유량만큼.
//
// 반환 slice 는 호출자 소유 (내부 상태와 분리된 복사본).
func (r *RingBuffer) Snapshot(pair session.Pair, max int) []Quote {
	r.mu.RLock()
	ring, ok := r.pairs[pair]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	ring.mu.RLock()
	defer ring.mu.RUnlock()

	// 보유 tick 개수 결정.
	have := ring.next
	if ring.full {
		have = r.capPerPair
	}
	if max <= 0 || max > have {
		max = have
	}
	if max == 0 {
		return nil
	}

	// chronological start 인덱스 계산.
	//   - !full : data[0..next) 가 시간순. 최근 max 개는 data[next-max..next).
	//   - full  : data[next..cap) + data[0..next) 가 시간순. 최근 max 개를 잘라낸다.
	out := make([]Quote, max)
	if !ring.full {
		copy(out, ring.data[ring.next-max:ring.next])
		return out
	}
	// full: 시작 인덱스 = (next - max + cap) % cap, 길이 = max.
	start := (ring.next - max + r.capPerPair) % r.capPerPair
	if start+max <= r.capPerPair {
		copy(out, ring.data[start:start+max])
	} else {
		// wrap-around.
		first := r.capPerPair - start
		copy(out, ring.data[start:])
		copy(out[first:], ring.data[:max-first])
	}
	return out
}

// Size 는 해당 pair 의 현재 보유 tick 수.
func (r *RingBuffer) Size(pair session.Pair) int {
	r.mu.RLock()
	ring, ok := r.pairs[pair]
	r.mu.RUnlock()
	if !ok {
		return 0
	}
	ring.mu.RLock()
	defer ring.mu.RUnlock()
	if ring.full {
		return r.capPerPair
	}
	return ring.next
}

// Pairs 는 현재 ring 이 존재하는 pair 목록 (정렬 보장 X).
func (r *RingBuffer) Pairs() []session.Pair {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]session.Pair, 0, len(r.pairs))
	for p := range r.pairs {
		out = append(out, p)
	}
	return out
}

// getOrCreate 는 pair 용 ring 이 없으면 생성한다 (double-checked locking).
func (r *RingBuffer) getOrCreate(pair session.Pair) *pairRing {
	r.mu.RLock()
	ring, ok := r.pairs[pair]
	r.mu.RUnlock()
	if ok {
		return ring
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// race 회피 — 다른 goroutine 이 먼저 생성했을 수 있음.
	if ring, ok = r.pairs[pair]; ok {
		return ring
	}
	ring = &pairRing{data: make([]Quote, r.capPerPair)}
	r.pairs[pair] = ring
	return ring
}
