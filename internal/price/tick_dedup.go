package price

import "sync"

// tickResetGap — seq 가 hwm 보다 이만큼 이상 역행하면 feed reset/wrap 으로 보고
// hwm 을 재설정(통과). 정상 중복(dual-active 인터리브 역행 ≤ 수 개)과 명확히 구분.
// 2^16: reset 은 seq 가 저값으로 급락(대폭 역행)이라 넘어서고, 인터리브 역행은
// 훨씬 작아 안전. (임계보다 큰 lag 의 forwarder 는 사실상 死 — false-reset 무시가능)
const tickResetGap = 1 << 16

// tickDedup — dual-active forwarder HA 용 (source, seq) 중복 제거.
//
// 두 forwarder 가 같은 LP feed 를 relay 하면 mci-price 가 같은 (source, seq) tick 을
// 양쪽에서 받는다. BEST(max/min)는 멱등이라 중복이 무해하지만 체결(last)·bar 는
// 중복 집계되므로 dedup 필수. per-source high-water-mark: seq <= hwm 면 drop.
//
// src=="" 또는 seq==0 이면 dedup 불가 → 통과 (seq 미부여 경로 안 깨짐).
// feed reset(seq 큰 폭 역행)은 hwm 재설정 후 통과.
type tickDedup struct {
	mu  sync.Mutex
	hwm map[string]uint64 // source → 최고 seq
}

func newTickDedup() *tickDedup {
	return &tickDedup{hwm: make(map[string]uint64)}
}

// seen — (src, seq) 가 이미 처리됐으면 true(=drop). 처음이면 false(=통과) + hwm 갱신.
func (d *tickDedup) seen(src string, seq uint64) bool {
	if src == "" || seq == 0 {
		return false // dedup 불가 → 통과
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.hwm[src]
	if !ok || seq > h {
		d.hwm[src] = seq
		return false
	}
	// seq <= h: 중복. 단 큰 폭 역행이면 feed reset → 재설정 + 통과.
	if h-seq >= tickResetGap {
		d.hwm[src] = seq
		return false
	}
	return true
}
