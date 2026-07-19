package price

import "testing"

// tickDedup — dual-active forwarder HA 시 (source, seq) 중복 tick 을 drop.
// per-source high-water-mark: seq <= hwm 면 중복.

func TestTickDedup_DropsDuplicate(t *testing.T) {
	d := newTickDedup()
	// 첫 등장 통과.
	if d.seen("SMB", 1) {
		t.Fatal("첫 SMB/1 이 drop 됨")
	}
	// 같은 (src, seq) 재등장 = 중복 → drop.
	if !d.seen("SMB", 1) {
		t.Error("SMB/1 중복이 통과됨")
	}
	// 다음 seq 통과.
	if d.seen("SMB", 2) {
		t.Error("SMB/2 가 drop 됨")
	}
	// 역행(이미 지난 seq) = 중복.
	if !d.seen("SMB", 2) {
		t.Error("SMB/2 중복이 통과됨")
	}
}

func TestTickDedup_PerSourceIndependent(t *testing.T) {
	d := newTickDedup()
	d.seen("SMB", 5)
	// 다른 source 는 독립 — KMB/1 은 통과.
	if d.seen("KMB", 1) {
		t.Error("KMB/1 이 SMB hwm 에 영향받아 drop 됨")
	}
}

// dual-active 인터리브: A,B 두 forwarder 가 같은 feed 를 relay → seq 중복 인터리브.
func TestTickDedup_InterleavedDualActive(t *testing.T) {
	d := newTickDedup()
	// A:1, B:1, A:2, B:2, A:3, B:3 → 중복 3건 drop, 고유 3건 통과.
	seq := []uint64{1, 1, 2, 2, 3, 3}
	passed := 0
	for _, s := range seq {
		if !d.seen("SMB", s) {
			passed++
		}
	}
	if passed != 3 {
		t.Errorf("통과 %d 건, want 3 (중복 제거)", passed)
	}
}

// seq==0 또는 src=="" 면 dedup 불가 → 항상 통과 (mock-lp 반복 등 안 깨짐).
func TestTickDedup_NoKeyPassesThrough(t *testing.T) {
	d := newTickDedup()
	for i := 0; i < 3; i++ {
		if d.seen("SMB", 0) {
			t.Error("seq==0 이 drop 됨")
		}
		if d.seen("", 5) {
			t.Error("src=='' 가 drop 됨")
		}
	}
}

// feed reset(seq 큰 폭 역행) → 재설정 후 통과.
func TestTickDedup_FeedReset(t *testing.T) {
	d := newTickDedup()
	d.seen("SMB", 1_000_000)
	// seq 가 1 로 리셋 (feed 재시작) — reset 감지 → 통과.
	if d.seen("SMB", 1) {
		t.Error("feed reset 후 seq=1 이 drop 됨")
	}
	// 리셋 이후는 다시 monotonic.
	if !d.seen("SMB", 1) {
		t.Error("리셋 후 seq=1 중복이 통과됨")
	}
}
