package price

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── pairLiveness 단위 ───────────────────────────────────────────────

func TestPairLiveness_FirstUpdate_NotFresh(t *testing.T) {
	l := newPairLiveness()
	// 첫 Update — stale 인 적 없으므로 becameFresh=false.
	if l.Update("USD/KRW", time.Now()) {
		t.Errorf("첫 Update 가 becameFresh=true")
	}
}

func TestPairLiveness_ScanForStale_NewlyStale(t *testing.T) {
	l := newPairLiveness()
	t0 := time.Now()
	l.Update("USD/KRW", t0.Add(-60*time.Second))
	l.Update("EUR/USD", t0.Add(-5*time.Second))

	newly := l.ScanForStale(t0, 30*time.Second)
	if len(newly) != 1 || newly[0] != "USD/KRW" {
		t.Errorf("newlyStale=%v, want [USD/KRW]", newly)
	}
}

func TestPairLiveness_ScanForStale_AlreadyStale_NoDup(t *testing.T) {
	l := newPairLiveness()
	t0 := time.Now()
	l.Update("USD/KRW", t0.Add(-60*time.Second))

	first := l.ScanForStale(t0, 30*time.Second)
	if len(first) != 1 {
		t.Fatalf("first scan=%v, want 1", first)
	}
	second := l.ScanForStale(t0, 30*time.Second)
	if len(second) != 0 {
		t.Errorf("second scan=%v, want 0 (이미 stale 표시됨)", second)
	}
}

func TestPairLiveness_UpdateAfterStale_BecomesFresh(t *testing.T) {
	l := newPairLiveness()
	t0 := time.Now()
	l.Update("USD/KRW", t0.Add(-60*time.Second))
	l.ScanForStale(t0, 30*time.Second) // stale 마킹

	became := l.Update("USD/KRW", t0)
	if !became {
		t.Errorf("stale 후 Update 가 becameFresh=false")
	}
	// 다시 stale 가능 (회복 후 또 무음일 때).
	if got := l.Update("USD/KRW", t0); got {
		t.Errorf("회복 직후 같은 Update 가 또 becameFresh=true")
	}
}

func TestPairLiveness_Snapshot_IncludesAllPairs(t *testing.T) {
	l := newPairLiveness()
	t0 := time.Now()
	l.Update("USD/KRW", t0.Add(-60*time.Second))
	l.Update("EUR/USD", t0)
	l.ScanForStale(t0, 30*time.Second)

	snap := l.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len=%d, want 2", len(snap))
	}
	staleCount := 0
	for _, e := range snap {
		if e.IsStale {
			staleCount++
			if e.Pair != "USD/KRW" {
				t.Errorf("stale=%v, want USD/KRW", e.Pair)
			}
		}
	}
	if staleCount != 1 {
		t.Errorf("stale=%d, want 1", staleCount)
	}
}

// ─── BroadcastForPair — Registry ─────────────────────────────────────

func TestRegistry_BroadcastForPair_MatchesFilteredAndAllMode(t *testing.T) {
	r := NewRegistry(nil)
	allMode := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	krwOnly := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	krwOnly.SubscribePairs([]string{"USD/KRW"})
	eurOnly := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	eurOnly.SubscribePairs([]string{"EUR/USD"})
	r.Add(allMode)
	r.Add(krwOnly)
	r.Add(eurOnly)

	sent, _ := r.BroadcastForPair("USD/KRW", []byte("payload"))
	if sent != 2 {
		t.Errorf("USD/KRW broadcast sent=%d, want 2 (allMode + krwOnly)", sent)
	}
	if len(allMode.send) != 1 || len(krwOnly.send) != 1 || len(eurOnly.send) != 0 {
		t.Errorf("큐: all=%d krw=%d eur=%d", len(allMode.send), len(krwOnly.send), len(eurOnly.send))
	}
}

// ─── ws E2E — stale / fresh 알림 ─────────────────────────────────────

func TestEdgePrice_Phase4_StaleFresh_E2E(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16
	// scanner 는 비활성 (background goroutine 의 timing 비결정성 회피).
	// 본 테스트는 sendStaleNotification / Update 직접 호출로 결정적 검증.
	cfg.StaleThreshold = 100 * time.Millisecond
	cfg.StaleScanInterval = 1 * time.Hour

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	conn, _, err := websocket.DefaultDialer.Dial(url, map[string][]string{"X-WTG-User": {"tester"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 1단계: pair Update 1회 → 시간 흐름 → ScanForStale 직접 호출 → stale 알림 발송
	t0 := time.Now()
	s.liveness.Update("USD/KRW", t0.Add(-200*time.Millisecond))
	newly := s.liveness.ScanForStale(t0, cfg.StaleThreshold)
	if len(newly) != 1 || newly[0] != "USD/KRW" {
		t.Fatalf("ScanForStale=%v, want [USD/KRW]", newly)
	}
	s.sendStaleNotification("USD/KRW", cfg.StaleThreshold)

	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("stale 알림 read: %v", err)
	}
	var msg map[string]any
	if e := json.Unmarshal(data, &msg); e != nil {
		t.Fatalf("stale unmarshal: %v (raw=%s)", e, data)
	}
	if msg["type"] != "stale" || msg["pair"] != "USD/KRW" {
		t.Errorf("stale 알림 모양=%v", msg)
	}

	// 2단계: 회복 — Update 호출 → fresh 알림 발송.
	became := s.liveness.Update("USD/KRW", t0)
	if !became {
		t.Errorf("liveness.Update becameFresh=false")
	}
	s.sendFreshNotification("USD/KRW")

	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("fresh 알림 read: %v", err)
	}
	if e := json.Unmarshal(data, &msg); e != nil {
		t.Fatalf("fresh unmarshal: %v (raw=%s)", e, data)
	}
	if msg["type"] != "fresh" || msg["pair"] != "USD/KRW" {
		t.Errorf("fresh 알림 모양=%v", msg)
	}

	// 카운터 검증
	if s.totalStaleSent.Load() != 1 {
		t.Errorf("totalStaleSent=%d, want 1", s.totalStaleSent.Load())
	}
	if s.totalFreshSent.Load() != 1 {
		t.Errorf("totalFreshSent=%d, want 1", s.totalFreshSent.Load())
	}
}
