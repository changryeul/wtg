package handlers

import (
	"sync"
	"time"
)

// TxRing — 최근 N 건 매매 transaction 의 in-memory circular buffer.
//
// 운영 감사 / 분쟁 재구성용 — mci-api 의 /v1/tx + /v1/tx/bulk 호출 1건마다
// Append. mci-admin 의 매매 감사 dashboard 가 /v1/admin/recent-tx 로 snapshot
// 을 polling.
//
// 동시성: 단일 RWMutex 보호. 운영 부하 (수백 tps × per-call 1 append + read
// 는 가끔) 에서 무경쟁. Ring 사이즈 = 1000 default — 1.5KB × 1000 ≈ 1.5MB
// 메모리.
//
// 영속화 안 함 — restart 후 비움. 영구 audit 은 mci-admin 의 audit ring +
// SIEM 으로. 본 ring 은 "최근 분쟁 / spike 분석" 용.
type TxRing struct {
	mu      sync.RWMutex
	entries []TxEntry
	idx     int // 다음 write 위치
	full    bool
}

// TxEntry — 단건 매매 audit 기록.
type TxEntry struct {
	TS         time.Time `json:"ts"`
	Usid       string    `json:"usid"`
	Channel    string    `json:"channel,omitempty"`
	Tier       string    `json:"tier,omitempty"`
	Alias      string    `json:"alias,omitempty"`
	Exchange   string    `json:"exchange,omitempty"`
	RoutingKey string    `json:"routing_key,omitempty"`
	HTTPStatus int       `json:"http_status"`
	BrokerErrn uint32    `json:"broker_errn,omitempty"`
	LatencyMs  float64   `json:"latency_ms"`
	RequestID  string    `json:"rid,omitempty"`
	TraceIDHex string    `json:"trace_id,omitempty"`
	IsBulk     bool      `json:"is_bulk,omitempty"`
}

// NewTxRing — capacity N 의 ring 생성. 0 이하면 1000.
func NewTxRing(capacity int) *TxRing {
	if capacity <= 0 {
		capacity = 1000
	}
	return &TxRing{entries: make([]TxEntry, capacity)}
}

// Append — 한 매매 record 누적. 가득 차면 가장 옛것 덮어씀.
func (r *TxRing) Append(e TxEntry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.idx] = e
	r.idx = (r.idx + 1) % len(r.entries)
	if r.idx == 0 {
		r.full = true
	}
}

// Snapshot — 최근 → 옛 순으로 반환. 운영자가 "최신이 위" 표시할 때 자연스러움.
//
// limit 가 0 이거나 cap 보다 크면 ring 전체. 음수면 nil.
func (r *TxRing) Snapshot(limit int) []TxEntry {
	if r == nil || limit < 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	size := r.idx
	if r.full {
		size = len(r.entries)
	}
	if limit == 0 || limit > size {
		limit = size
	}
	out := make([]TxEntry, 0, limit)
	// idx 는 다음 write 위치 — 가장 옛 entry. 역순으로 limit 개.
	for i := 0; i < limit; i++ {
		// (r.idx - 1 - i + len) % len = 최신부터 거꾸로
		pos := (r.idx - 1 - i + len(r.entries)) % len(r.entries)
		out = append(out, r.entries[pos])
	}
	return out
}

// Size — 현재 누적된 entry 수 (cap 까지).
func (r *TxRing) Size() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return len(r.entries)
	}
	return r.idx
}

// Cap — ring capacity.
func (r *TxRing) Cap() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}
