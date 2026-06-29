package price

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── MemoryPairValidator 단위 ─────────────────────────────────────────────

func TestMemoryPairValidator_EmptyDefault(t *testing.T) {
	v := NewMemoryPairValidator()
	if v.IsAllowed("USD/KRW") {
		t.Errorf("빈 validator 가 USD/KRW 허용 — passive learning 전엔 deaf 여야")
	}
	if got := v.AllowedSnapshot(); len(got) != 0 {
		t.Errorf("AllowedSnapshot()=%v, want empty", got)
	}
}

func TestMemoryPairValidator_AddIdempotent(t *testing.T) {
	v := NewMemoryPairValidator()
	v.Add("USD/KRW", "EUR/USD", "USD/KRW") // 중복
	v.Add("USD/KRW")                       // 다시 중복
	v.Add("")                              // 빈 문자열 무시
	if !v.IsAllowed("USD/KRW") || !v.IsAllowed("EUR/USD") {
		t.Errorf("Add 한 pair 허용 실패")
	}
	if v.IsAllowed("") {
		t.Errorf("빈 문자열 허용됨")
	}
	got := v.AllowedSnapshot()
	want := []string{"EUR/USD", "USD/KRW"} // sorted
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("snapshot=%v, want %v", got, want)
	}
}

// ─── gateSubscribe 단위 ──────────────────────────────────────────────────

func TestGateSubscribe_NilValidator_AllowsAll(t *testing.T) {
	s := NewServer(DefaultConfig(), discardLogger())
	// SetPairValidator 없이 — backward compat 경로.
	acc, rej := s.gateSubscribe(nil, []string{"USD/KRW", "EUR/USD"})
	if len(acc) != 2 || len(rej) != 0 {
		t.Errorf("nil validator accepted=%v rejected=%v, want all accepted", acc, rej)
	}
}

func TestGateSubscribe_PartialReject(t *testing.T) {
	s := NewServer(DefaultConfig(), discardLogger())
	v := NewMemoryPairValidator()
	v.Add("USD/KRW", "EUR/USD")
	s.SetPairValidator(v)

	acc, rej := s.gateSubscribe(nil, []string{"USD/KRW", "JPY/KRW", "EUR/USD", "FAKE/PAIR"})
	if len(acc) != 2 {
		t.Errorf("accepted=%v, want 2", acc)
	}
	if len(rej) != 2 {
		t.Errorf("rejected=%v, want 2", rej)
	}
}

// ─── ws control E2E — Phase 2 ──────────────────────────────────────────

func TestEdgePrice_Phase2_ForbiddenPair_ErrorFrame(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16
	cfg.QuoteSeedPairs = []string{"USD/KRW"} // 시드

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	conn, _, err := websocket.DefaultDialer.Dial(url, map[string][]string{"X-WTG-User": {"tester"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// USD/KRW 는 허용, EUR/USD 는 미허용 → 부분 reject + accept.
	m, _ := json.Marshal(map[string]any{
		"type":  "subscribe",
		"pairs": []string{"USD/KRW", "EUR/USD"},
	})
	conn.WriteMessage(websocket.TextMessage, m)

	// 첫 응답: error frame (forbidden_pair).
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(msg, &got)
	if got["type"] != "error" || got["code"] != "forbidden_pair" {
		t.Errorf("error frame=%v, want type=error code=forbidden_pair", got)
	}
	rej, _ := got["rejected"].([]any)
	if len(rej) != 1 || rej[0] != "EUR/USD" {
		t.Errorf("rejected=%v, want [EUR/USD]", rej)
	}

	// 두 번째 응답: echo. 허용된 pair 만 등록되어 있어야.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg2, _ := conn.ReadMessage()
	var echo map[string]any
	json.Unmarshal(msg2, &echo)
	if echo["type"] != "subscribed" {
		t.Errorf("echo type=%v, want subscribed", echo["type"])
	}
	pairs, _ := echo["pairs"].([]any)
	if len(pairs) != 1 || pairs[0] != "USD/KRW" {
		t.Errorf("echo pairs=%v, want [USD/KRW] only", pairs)
	}
}

func TestEdgePrice_Phase2_PassiveLearning_AllowsAfterFirstQuote(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16

	s := NewServer(cfg, discardLogger())
	// seed 없이 빈 MemoryPairValidator 만 set — 처음엔 모든 pair 거부.
	v := NewMemoryPairValidator()
	s.SetPairValidator(v)

	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	conn, _, _ := websocket.DefaultDialer.Dial(url, map[string][]string{"X-WTG-User": {"tester"}})
	defer conn.Close()

	// 첫 subscribe — 모두 reject.
	first, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	conn.WriteMessage(websocket.TextMessage, first)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, _ := conn.ReadMessage()
	var got map[string]any
	json.Unmarshal(msg, &got)
	if got["code"] != "forbidden_pair" {
		t.Errorf("seed 없는데 첫 subscribe 가 통과: %v", got)
	}
	conn.ReadMessage() // echo drain (empty pairs)

	// passive learning 시뮬 — 도착 quote 의 pair 가 validator 에 들어왔다고 가정.
	v.Add("USD/KRW")

	// 두 번째 subscribe — 이제 통과.
	second, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	conn.WriteMessage(websocket.TextMessage, second)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg2, _ := conn.ReadMessage()
	var echo map[string]any
	json.Unmarshal(msg2, &echo)
	if echo["type"] != "subscribed" {
		t.Errorf("passive learning 후 응답 type=%v, want subscribed (no error preceding)", echo["type"])
	}
}

func TestEdgePrice_Phase2_QuoteSeedPairs_ConfigDriven(t *testing.T) {
	// cfg.QuoteSeedPairs 로 NewServer 가 자동 MemoryPairValidator wire.
	cfg := DefaultConfig()
	cfg.QuoteSeedPairs = []string{"USD/KRW", "EUR/USD"}
	s := NewServer(cfg, discardLogger())

	if s.pairValidator == nil {
		t.Fatalf("seed 가 있는데 pairValidator=nil")
	}
	if !s.pairValidator.IsAllowed("USD/KRW") || !s.pairValidator.IsAllowed("EUR/USD") {
		t.Errorf("seed pair 가 허용 set 에 없음")
	}
	if s.pairValidator.IsAllowed("JPY/KRW") {
		t.Errorf("seed 에 없는 pair 가 허용됨")
	}
}
