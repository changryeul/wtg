package price

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/winwaysystems/wtg/pkg/netutil"
)

// admin endpoint 가 AdminAllowCIDRs 비어있을 때 모든 요청 거부.
func TestAdmin_NoAllowCIDRs_DefaultsToForbidden(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	// AdminAllowCIDRs 미설정 — 모든 admin 요청 403.

	s := NewServer(cfg, discardLogger())
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"pair": "USD/KRW"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/admin/disallow-pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WTG-User", "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("AdminAllowCIDRs 비었는데 status=%d, want 403", resp.StatusCode)
	}
}

// AdminAllowCIDRs 가 loopback 포함 시 통과.
func TestAdmin_AllowCIDRs_LoopbackPasses(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.AdminAllowCIDRs, _ = netutil.ParseCIDRs("127.0.0.0/8")
	v := NewMemoryPairValidator()
	v.Add("USD/KRW")

	s := NewServer(cfg, discardLogger())
	s.SetPairValidator(v)
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"pair": "USD/KRW"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/admin/disallow-pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WTG-User", "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback 허용 인데 status=%d, want 200", resp.StatusCode)
	}
	// validator 에서 빠졌는지.
	if v.IsAllowed("USD/KRW") {
		t.Errorf("disallow-pair 호출 후에도 USD/KRW 허용됨")
	}
}

// disallow-pair 가 기존 sub 의 필터에서 pair 를 제거하고 알림 전송.
func TestAdmin_DisallowPair_RevokesAndNotifies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.WsPongTimeout = 5 * time.Second
	cfg.WsPingInterval = 60 * time.Second
	cfg.SendQueueSize = 16
	cfg.AdminAllowCIDRs, _ = netutil.ParseCIDRs("127.0.0.0/8")
	v := NewMemoryPairValidator()
	v.Add("USD/KRW", "EUR/USD")

	s := NewServer(cfg, discardLogger())
	s.SetPairValidator(v)
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	// 1) ws connect + USD/KRW 구독
	url := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/subscribe?profile=WEB.BRANCH.VIP"
	conn, _, err := websocket.DefaultDialer.Dial(url, map[string][]string{"X-WTG-User": {"tester"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sub, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	conn.WriteMessage(websocket.TextMessage, sub)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	conn.ReadMessage() // echo drain

	// 2) admin disallow-pair USD/KRW
	body, _ := json.Marshal(map[string]any{"pair": "USD/KRW"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/admin/disallow-pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WTG-User", "admin")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disallow-pair status=%d", resp.StatusCode)
	}

	// 3) ws 에서 revoked 알림 수신
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("revoked 알림 read: %v", err)
	}
	var msg map[string]any
	json.Unmarshal(data, &msg)
	if msg["type"] != "revoked" || msg["pair"] != "USD/KRW" {
		t.Errorf("revoked 알림 모양=%v", msg)
	}

	// 4) validator + sub filter 둘 다 정리됐는지 확인 — 같은 ws 재 subscribe 시도 → 거부.
	resub, _ := json.Marshal(map[string]any{"type": "subscribe", "pairs": []string{"USD/KRW"}})
	conn.WriteMessage(websocket.TextMessage, resub)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, _ = conn.ReadMessage()
	var got map[string]any
	json.Unmarshal(data, &got)
	if got["code"] != "forbidden_pair" {
		t.Errorf("disallow 후 resubscribe 가 통과: %v", got)
	}
}

// allow-pair 가 validator 에 pair 추가.
func TestAdmin_AllowPair_AddsToValidator(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	cfg.AdminAllowCIDRs, _ = netutil.ParseCIDRs("127.0.0.0/8")
	v := NewMemoryPairValidator()
	s := NewServer(cfg, discardLogger())
	s.SetPairValidator(v)
	ts := httptest.NewServer(s.BuildHandler())
	defer ts.Close()

	if v.IsAllowed("USD/KRW") {
		t.Fatal("처음엔 USD/KRW 허용 안 되어야")
	}
	body, _ := json.Marshal(map[string]any{"pair": "USD/KRW"})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/admin/allow-pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-WTG-User", "admin")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("allow-pair status=%d", resp.StatusCode)
	}
	if !v.IsAllowed("USD/KRW") {
		t.Errorf("allow-pair 후에도 USD/KRW 허용 안 됨")
	}
}

// ── Registry.RevokePairFromAll 단위 ──

func TestRegistry_RevokePairFromAll_CountsAffected(t *testing.T) {
	r := NewRegistry(nil)
	a := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	a.SubscribePairs([]string{"USD/KRW", "EUR/USD"})
	b := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"})
	b.SubscribePairs([]string{"EUR/USD"})                                                       // USD/KRW 없음
	c := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4, ProfileKey: "P"}) // all 모드
	r.Add(a)
	r.Add(b)
	r.Add(c)

	affected := r.RevokePairFromAll("USD/KRW")
	if affected != 1 {
		t.Errorf("affected=%d, want 1 (a 만)", affected)
	}
	if a.MatchesPair("USD/KRW") {
		t.Errorf("a 의 USD/KRW 가 안 빠짐")
	}
	if !a.MatchesPair("EUR/USD") {
		t.Errorf("a 의 EUR/USD 가 같이 빠짐")
	}
	// c (all 모드) 는 영향 없음 — 여전히 모든 pair 매칭.
	if !c.MatchesPair("USD/KRW") {
		t.Errorf("all 모드 sub 의 매칭이 깨짐")
	}
}
