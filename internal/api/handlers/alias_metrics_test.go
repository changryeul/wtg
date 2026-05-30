package handlers

import (
	"testing"
	"time"
)

func TestAliasMetrics_RecordAndSnapshot(t *testing.T) {
	m := NewAliasMetrics()

	m.RecordCall("WECHO_PING", 5*time.Millisecond, false)
	m.RecordCall("WECHO_PING", 15*time.Millisecond, false)
	m.RecordCall("WECHO_PING", 30*time.Millisecond, true)
	m.RecordCall("WBALANCE", 100*time.Millisecond, false)

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 alias snapshots, got %d", len(snap))
	}

	// Calls 내림차순 — WECHO_PING (3) 먼저
	if snap[0].Alias != "WECHO_PING" {
		t.Errorf("want WECHO_PING first, got %s", snap[0].Alias)
	}
	if snap[0].Calls != 3 {
		t.Errorf("WECHO_PING calls: want 3, got %d", snap[0].Calls)
	}
	if snap[0].Errors != 1 {
		t.Errorf("WECHO_PING errors: want 1, got %d", snap[0].Errors)
	}
	// avg = (5+15+30)/3 = 16.666ms
	if snap[0].AvgLatencyMs < 16 || snap[0].AvgLatencyMs > 17 {
		t.Errorf("WECHO_PING avg ms: want ~16.66, got %.3f", snap[0].AvgLatencyMs)
	}
	if snap[0].MaxLatencyMs < 29 || snap[0].MaxLatencyMs > 31 {
		t.Errorf("WECHO_PING max ms: want ~30, got %.3f", snap[0].MaxLatencyMs)
	}
	// error rate = 1/3 = 33.33%
	if snap[0].ErrorRatePct < 33 || snap[0].ErrorRatePct > 34 {
		t.Errorf("WECHO_PING error pct: want ~33.33, got %.3f", snap[0].ErrorRatePct)
	}
}

func TestAliasMetrics_EmptyAliasBucketsToRaw(t *testing.T) {
	m := NewAliasMetrics()
	m.RecordCall("", 1*time.Millisecond, false)

	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Alias != "__raw__" {
		t.Fatalf("empty alias should bucket to __raw__, got %+v", snap)
	}
}

func TestAliasMetrics_NilSafe(t *testing.T) {
	// nil receiver — handler 가 AliasMetrics 미주입 환경에서도 panic 없어야.
	var m *AliasMetrics
	m.RecordCall("X", time.Millisecond, false)
	if snap := m.Snapshot(); snap != nil {
		t.Errorf("nil receiver should return nil snapshot, got %+v", snap)
	}
}
