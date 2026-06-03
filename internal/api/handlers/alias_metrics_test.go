package handlers

import (
	"testing"
	"time"
)

func TestAliasMetrics_RecordAndSnapshot(t *testing.T) {
	m := NewAliasMetrics()

	m.RecordCall("WECHO_PING", "VIP", 5*time.Millisecond, false)
	m.RecordCall("WECHO_PING", "VIP", 15*time.Millisecond, false)
	m.RecordCall("WECHO_PING", "VIP", 30*time.Millisecond, true)
	m.RecordCall("WBALANCE", "VIP", 100*time.Millisecond, false)

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 (alias,tier) snapshots, got %d", len(snap))
	}

	// Calls 내림차순 — WECHO_PING (3) 먼저
	if snap[0].Alias != "WECHO_PING" || snap[0].Tier != "VIP" {
		t.Errorf("want (WECHO_PING, VIP) first, got (%s, %s)", snap[0].Alias, snap[0].Tier)
	}
	if snap[0].Calls != 3 {
		t.Errorf("WECHO_PING calls: want 3, got %d", snap[0].Calls)
	}
	if snap[0].Errors != 1 {
		t.Errorf("WECHO_PING errors: want 1, got %d", snap[0].Errors)
	}
	if snap[0].AvgLatencyMs < 16 || snap[0].AvgLatencyMs > 17 {
		t.Errorf("WECHO_PING avg ms: want ~16.66, got %.3f", snap[0].AvgLatencyMs)
	}
	if snap[0].MaxLatencyMs < 29 || snap[0].MaxLatencyMs > 31 {
		t.Errorf("WECHO_PING max ms: want ~30, got %.3f", snap[0].MaxLatencyMs)
	}
	if snap[0].ErrorRatePct < 33 || snap[0].ErrorRatePct > 34 {
		t.Errorf("WECHO_PING error pct: want ~33.33, got %.3f", snap[0].ErrorRatePct)
	}
}

// 같은 alias 의 다른 tier 는 별도 row — VIP 와 STD 의 latency / error 분리 관찰.
func TestAliasMetrics_TierMatrix(t *testing.T) {
	m := NewAliasMetrics()
	m.RecordCall("ORDER_NEW", "VIP", 5*time.Millisecond, false)
	m.RecordCall("ORDER_NEW", "VIP", 8*time.Millisecond, false)
	m.RecordCall("ORDER_NEW", "STD", 50*time.Millisecond, false)
	m.RecordCall("ORDER_NEW", "STD", 60*time.Millisecond, true)

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 rows (VIP/STD matrix), got %d", len(snap))
	}
	// 동률 — alias 같고 calls 같음. tier 사전순.
	var vip, std *AliasStatSnapshot
	for i := range snap {
		if snap[i].Tier == "VIP" {
			vip = &snap[i]
		}
		if snap[i].Tier == "STD" {
			std = &snap[i]
		}
	}
	if vip == nil || std == nil {
		t.Fatalf("VIP/STD row 누락: %+v", snap)
	}
	if vip.Errors != 0 || std.Errors != 1 {
		t.Errorf("tier 별 error 분리 실패: VIP=%d STD=%d", vip.Errors, std.Errors)
	}
	if vip.AvgLatencyMs > 10 || std.AvgLatencyMs < 50 {
		t.Errorf("tier 별 latency 분리 실패: VIP=%v STD=%v", vip.AvgLatencyMs, std.AvgLatencyMs)
	}
}

// alias 빈값 → __raw__. tier 빈값 → __notier__ (UserProfileResolver 비활성).
func TestAliasMetrics_EmptyBucketsToSentinels(t *testing.T) {
	m := NewAliasMetrics()
	m.RecordCall("", "", 1*time.Millisecond, false)
	m.RecordCall("PING", "", 1*time.Millisecond, false)

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snap=%+v", snap)
	}
	found := map[string]bool{}
	for _, s := range snap {
		found[s.Alias+"|"+s.Tier] = true
	}
	if !found["__raw__|__notier__"] {
		t.Errorf("__raw__ + __notier__ 버킷 누락: %+v", snap)
	}
	if !found["PING|__notier__"] {
		t.Errorf("PING + __notier__ 버킷 누락: %+v", snap)
	}
}

func TestAliasMetrics_NilSafe(t *testing.T) {
	// nil receiver — handler 가 AliasMetrics 미주입 환경에서도 panic 없어야.
	var m *AliasMetrics
	m.RecordCall("X", "VIP", time.Millisecond, false)
	if snap := m.Snapshot(); snap != nil {
		t.Errorf("nil receiver should return nil snapshot, got %+v", snap)
	}
}

// 동률 Calls 시 alias 사전순, alias 같으면 tier 사전순.
func TestAliasMetrics_SortStability(t *testing.T) {
	m := NewAliasMetrics()
	// 모두 1 call — Calls 동률.
	m.RecordCall("B", "VIP", 0, false)
	m.RecordCall("A", "STD", 0, false)
	m.RecordCall("A", "VIP", 0, false)
	m.RecordCall("B", "STD", 0, false)
	snap := m.Snapshot()
	want := []struct{ alias, tier string }{
		{"A", "STD"}, {"A", "VIP"}, {"B", "STD"}, {"B", "VIP"},
	}
	if len(snap) != len(want) {
		t.Fatalf("len=%d, want %d", len(snap), len(want))
	}
	for i, w := range want {
		if snap[i].Alias != w.alias || snap[i].Tier != w.tier {
			t.Errorf("snap[%d]=(%s,%s), want (%s,%s)", i, snap[i].Alias, snap[i].Tier, w.alias, w.tier)
		}
	}
}
