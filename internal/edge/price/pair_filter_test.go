package price

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── Subscriber.MatchesPair / Subscribe / Unsubscribe 단위 ────────────────

func TestSubscriber_PairsDefault_ReceiveAll(t *testing.T) {
	s := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	if !s.MatchesPair("USD/KRW") || !s.MatchesPair("EUR/USD") {
		t.Errorf("기본 상태(필터 nil)는 모든 pair 매칭이어야")
	}
	if s.SubscribedPairs() != nil {
		t.Errorf("기본 상태 SubscribedPairs()=%v, want nil (all 모드)", s.SubscribedPairs())
	}
}

func TestSubscriber_Subscribe_FilterModeMatchesOnly(t *testing.T) {
	s := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	s.SubscribePairs([]string{"USD/KRW", "EUR/USD"})

	if !s.MatchesPair("USD/KRW") || !s.MatchesPair("EUR/USD") {
		t.Errorf("subscribe 한 pair 매칭 실패")
	}
	if s.MatchesPair("USD/JPY") {
		t.Errorf("subscribe 안 한 pair USD/JPY 매칭됨")
	}
	got := s.SubscribedPairs()
	want := []string{"EUR/USD", "USD/KRW"} // sorted
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SubscribedPairs=%v, want %v", got, want)
	}
}

func TestSubscriber_Subscribe_Idempotent(t *testing.T) {
	s := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	s.SubscribePairs([]string{"USD/KRW"})
	s.SubscribePairs([]string{"USD/KRW", "USD/KRW", "USD/KRW"})
	if len(s.SubscribedPairs()) != 1 {
		t.Errorf("중복 subscribe 후 len=%d, want 1", len(s.SubscribedPairs()))
	}
}

func TestSubscriber_Unsubscribe_PartialThenComplete(t *testing.T) {
	s := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	s.SubscribePairs([]string{"USD/KRW", "EUR/USD"})

	s.UnsubscribePairs([]string{"EUR/USD"})
	if !s.MatchesPair("USD/KRW") {
		t.Errorf("USD/KRW 는 남아있어야")
	}
	if s.MatchesPair("EUR/USD") {
		t.Errorf("EUR/USD 는 제거되었어야")
	}

	// 마지막 pair 도 제거 → "all" 모드 복귀.
	s.UnsubscribePairs([]string{"USD/KRW"})
	if s.SubscribedPairs() != nil {
		t.Errorf("필터 empty 후 SubscribedPairs()=%v, want nil", s.SubscribedPairs())
	}
	if !s.MatchesPair("USD/JPY") {
		t.Errorf("필터 empty 후 모든 pair 매칭 회귀 안 됨")
	}
}

func TestSubscriber_Unsubscribe_FromAllMode_NoOp(t *testing.T) {
	s := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	s.UnsubscribePairs([]string{"USD/KRW"}) // pairs 가 nil 인데 호출
	if !s.MatchesPair("USD/KRW") {
		t.Errorf("all 모드 unsubscribe 가 deafened 시킴")
	}
}

// ─── Registry.SendByProfile pair 필터 통합 ────────────────────────────────

func TestRegistry_SendByProfile_PairMatches(t *testing.T) {
	r := NewRegistry(nil)
	subAll := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "WEB.BRANCH.VIP"})
	subKRWOnly := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "WEB.BRANCH.VIP"})
	subKRWOnly.SubscribePairs([]string{"USD/KRW"})
	r.Add(subAll)
	r.Add(subKRWOnly)

	// USD/KRW 전송 — 둘 다 수신.
	sent, _ := r.SendByProfile("WEB.BRANCH.VIP", "USD/KRW", []byte("krw-tick"))
	if sent != 2 {
		t.Errorf("USD/KRW sent=%d, want 2", sent)
	}

	// EUR/USD 전송 — subAll 만 수신.
	sent, _ = r.SendByProfile("WEB.BRANCH.VIP", "EUR/USD", []byte("eur-tick"))
	if sent != 1 {
		t.Errorf("EUR/USD sent=%d, want 1 (subKRWOnly 는 매칭 안 함)", sent)
	}

	// subAll: 두 메시지 모두 받음. subKRWOnly: USD/KRW 만.
	if len(subAll.send) != 2 {
		t.Errorf("subAll 큐 len=%d, want 2", len(subAll.send))
	}
	if len(subKRWOnly.send) != 1 {
		t.Errorf("subKRWOnly 큐 len=%d, want 1", len(subKRWOnly.send))
	}
}

// ─── ws control message E2E ──────────────────────────────────────────────

func TestEdgePrice_ControlSubscribe_FiltersIncomingQuote(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	// ws connect — DevMode + profile 쿼리 (clientWithJWT 우회).
	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	header := map[string][]string{"X-WTG-User": {"tester"}}
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Subscribe USD/KRW
	subMsg, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	if err := conn.WriteMessage(websocket.TextMessage, subMsg); err != nil {
		t.Fatal(err)
	}

	// echo 받아서 확인.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, echo, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("echo read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(echo, &got); err != nil {
		t.Fatalf("echo unmarshal: %v (raw=%s)", err, echo)
	}
	if got["type"] != "subscribed" {
		t.Errorf("echo type=%v, want subscribed", got["type"])
	}
	pairs, _ := got["pairs"].([]any)
	if len(pairs) != 1 || pairs[0] != "USD/KRW" {
		t.Errorf("echo pairs=%v, want [USD/KRW]", pairs)
	}

	// 서버 측 시뮬레이션 — Registry 직접 SendByProfile 호출. EUR/USD 는 안
	// 받아야 (필터에 없음). USD/KRW 는 받아야.
	s.registry.SendByProfile("WEB.BRANCH.VIP", "EUR/USD", []byte(`{"pair":"EUR/USD","bid":1}`))
	s.registry.SendByProfile("WEB.BRANCH.VIP", "USD/KRW", []byte(`{"pair":"USD/KRW","bid":1}`))

	// 다음 read — USD/KRW 만 와야 (1개).
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("quote read: %v", err)
	}
	var q map[string]any
	_ = json.Unmarshal(msg, &q)
	if q["pair"] != "USD/KRW" {
		t.Errorf("받은 메시지 pair=%v, want USD/KRW", q["pair"])
	}

	// 추가 read — 100ms timeout 안 와야 (EUR/USD 는 필터)
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, msg, err := conn.ReadMessage(); err == nil {
		t.Errorf("EUR/USD 가 새어 들어옴: %s", msg)
	}
}

func TestEdgePrice_ControlUnsubscribe_RestoresAllMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	header := map[string][]string{"X-WTG-User": {"tester"}}
	conn, _, _ := websocket.DefaultDialer.Dial(url, header)
	defer conn.Close()

	// Subscribe USD/KRW → Unsubscribe USD/KRW → all 모드 복귀
	for _, m := range []map[string]any{
		{"type": "subscribe", "pairs": []string{"USD/KRW"}},
		{"type": "unsubscribe", "pairs": []string{"USD/KRW"}},
	} {
		b, _ := json.Marshal(m)
		conn.WriteMessage(websocket.TextMessage, b)
		conn.SetReadDeadline(time.Now().Add(time.Second))
		conn.ReadMessage() // echo drain
	}

	// EUR/USD 도 받아야 (all 복귀)
	s.registry.SendByProfile("WEB.BRANCH.VIP", "EUR/USD", []byte(`{"pair":"EUR/USD"}`))
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("quote 안 옴: %v", err)
	}
	var q map[string]any
	_ = json.Unmarshal(msg, &q)
	if q["pair"] != "EUR/USD" {
		t.Errorf("all 모드 복귀 후 EUR/USD 안 받음: %v", q)
	}
}

func TestEdgePrice_ControlBadJSON_ErrorFrame(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()
	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	conn, _, _ := websocket.DefaultDialer.Dial(url, map[string][]string{"X-WTG-User": {"tester"}})
	defer conn.Close()

	conn.WriteMessage(websocket.TextMessage, []byte("not json"))

	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(msg, &got)
	if got["type"] != "error" {
		t.Errorf("bad JSON 응답 type=%v, want error", got["type"])
	}
	if got["code"] != "bad_request" {
		t.Errorf("error code=%v, want bad_request", got["code"])
	}

	// ws 연결은 끊지 말아야 — 정상 메시지 후속이 통과해야.
	good, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	conn.WriteMessage(websocket.TextMessage, good)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg2, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("error 후 정상 메시지 응답 실패: %v", err)
	}
	json.Unmarshal(msg2, &got)
	if got["type"] != "subscribed" {
		t.Errorf("error 후속 type=%v, want subscribed", got["type"])
	}
}
