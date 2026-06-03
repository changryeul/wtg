package handlers

import (
	"sync"
	"testing"
	"time"
)

func TestTxRing_AppendAndSnapshot(t *testing.T) {
	r := NewTxRing(3)
	for i := 1; i <= 5; i++ {
		r.Append(TxEntry{TS: time.Unix(int64(i), 0), Usid: "u", HTTPStatus: 200, LatencyMs: float64(i)})
	}
	// cap=3, 가장 최근 3건만 (3,4,5). Snapshot 은 최신 → 옛.
	snap := r.Snapshot(0)
	if len(snap) != 3 {
		t.Fatalf("len=%d, want 3", len(snap))
	}
	if int(snap[0].LatencyMs) != 5 || int(snap[1].LatencyMs) != 4 || int(snap[2].LatencyMs) != 3 {
		t.Errorf("order: got %+v", snap)
	}
}

func TestTxRing_LimitLessThanSize(t *testing.T) {
	r := NewTxRing(10)
	for i := 1; i <= 6; i++ {
		r.Append(TxEntry{HTTPStatus: i})
	}
	snap := r.Snapshot(2)
	if len(snap) != 2 || snap[0].HTTPStatus != 6 || snap[1].HTTPStatus != 5 {
		t.Errorf("limit=2: %+v", snap)
	}
}

func TestTxRing_PartialFill(t *testing.T) {
	r := NewTxRing(5)
	r.Append(TxEntry{HTTPStatus: 1})
	r.Append(TxEntry{HTTPStatus: 2})
	if r.Size() != 2 {
		t.Errorf("size=%d, want 2", r.Size())
	}
	snap := r.Snapshot(0)
	if len(snap) != 2 || snap[0].HTTPStatus != 2 || snap[1].HTTPStatus != 1 {
		t.Errorf("partial: %+v", snap)
	}
}

func TestTxRing_NilSafe(t *testing.T) {
	var r *TxRing
	r.Append(TxEntry{HTTPStatus: 1})
	if r.Snapshot(10) != nil {
		t.Errorf("nil snapshot")
	}
	if r.Size() != 0 || r.Cap() != 0 {
		t.Errorf("nil counters: size=%d cap=%d", r.Size(), r.Cap())
	}
}

func TestTxRing_ConcurrentAppend(t *testing.T) {
	r := NewTxRing(100)
	var wg sync.WaitGroup
	const goroutines = 8
	const perG = 50
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				r.Append(TxEntry{HTTPStatus: 200, LatencyMs: float64(i)})
			}
		}()
	}
	wg.Wait()
	if r.Size() != 100 {
		t.Errorf("size=%d, want 100 (cap)", r.Size())
	}
}
