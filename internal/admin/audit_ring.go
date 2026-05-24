package admin

import (
	"sync"
	"time"
)

// AuditEntry 는 단일 admin 액션 기록 (auth.md §10 ADMIN_ACTION).
//
// 운영에서는 immutable 외부 sink (7년 보관) 가 single source of truth 이고,
// 본 ring 은 UI 의 즉시 표시 + 로컬 디버깅용 sliding window.
type AuditEntry struct {
	Action string         `json:"action"`          // PUT_ROUTE / DELETE_ROUTE / SET_ROUTE_ACTIVE / 등
	Usid   string         `json:"usid,omitempty"`  // 액션 수행 admin
	RID    string         `json:"rid,omitempty"`   // request id
	At     time.Time      `json:"at"`              // 발생 시각
	Attrs  map[string]any `json:"attrs,omitempty"` // 액션별 상세 (alias / active / 등)
}

// AuditRing 은 in-memory 고정크기 ring buffer.
//
// 동기화: sync.RWMutex. 작은 사이즈 (200) 라 락 경합은 무시 가능.
// FIFO — 가장 오래된 항목부터 덮어씀.
type AuditRing struct {
	mu  sync.RWMutex
	cap int
	buf []AuditEntry
	// next 는 다음 쓰기 위치 (0..cap-1).
	next int
	// full 은 한 바퀴 돌았는지 — true 면 모든 슬롯이 유효.
	full bool
	// onPush — 신규 항목 추가 시 호출 (ws 브로드캐스트 등). nil 가능.
	onPush func(AuditEntry)
}

// SetOnPush 는 push 콜백을 설정한다 (등록은 1회).
func (r *AuditRing) SetOnPush(f func(AuditEntry)) {
	r.mu.Lock()
	r.onPush = f
	r.mu.Unlock()
}

// NewAuditRing 은 capacity 크기의 ring 을 만든다.
// capacity <= 0 이면 200.
func NewAuditRing(capacity int) *AuditRing {
	if capacity <= 0 {
		capacity = 200
	}
	return &AuditRing{
		cap: capacity,
		buf: make([]AuditEntry, capacity),
	}
}

// Push 는 항목을 추가한다 (At 가 0 이면 자동 채움).
func (r *AuditRing) Push(e AuditEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	r.mu.Lock()
	r.buf[r.next] = e
	r.next++
	if r.next >= r.cap {
		r.next = 0
		r.full = true
	}
	cb := r.onPush
	r.mu.Unlock()
	if cb != nil {
		cb(e)
	}
}

// List 는 시간 역순 (최신 → 오래된) 으로 모든 항목을 복사 반환한다.
// limit > 0 이면 처음 limit 개만.
func (r *AuditRing) List(limit int) []AuditEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.cap
	if !r.full {
		n = r.next
	}
	if n == 0 {
		return nil
	}

	out := make([]AuditEntry, 0, n)
	// 가장 최근 항목부터 거꾸로 — 시작은 next-1 (또는 cap-1 if next==0).
	idx := r.next - 1
	if idx < 0 {
		idx = r.cap - 1
	}
	for i := 0; i < n; i++ {
		out = append(out, r.buf[idx])
		if limit > 0 && len(out) >= limit {
			break
		}
		idx--
		if idx < 0 {
			idx = r.cap - 1
		}
	}
	return out
}

// Len 은 현재 저장된 항목 수.
func (r *AuditRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return r.cap
	}
	return r.next
}
