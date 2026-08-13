package futures

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// buildKA — 합성 KA 전문(234B). code/last 지정.
func buildKA(code string, last float64) []byte {
	b := make([]byte, 234)
	for i := range b {
		b[i] = ' '
	}
	put := func(off, n int, s string) {
		if len(s) > n {
			s = s[:n]
		}
		copy(b[off:off+n], s)
	}
	put(0, 2, "KA")
	put(2, 12, code)
	put(24, 9, fmt.Sprintf("%-9.02f", last)) // oprc
	put(34, 9, fmt.Sprintf("%-9.02f", last)) // hprc
	put(44, 9, fmt.Sprintf("%-9.02f", last)) // lprc
	put(54, 9, fmt.Sprintf("%-9.02f", last)) // eprc last
	put(107, 1, "+")
	put(108, 12, "090005123456")
	return b
}

// G3 — web ws 가 구독한 종목의 fut.trade JSON 만 수신함을 e2e 로 증명.
func TestE2E_SubscribeFanout(t *testing.T) {
	srv := NewServer(nil)
	ts := httptest.NewServer(http.HandlerFunc(srv.ServeWS))
	defer ts.Close()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1)

	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// 101V6000 만 구독
	sub, _ := json.Marshal(control{Type: "subscribe", Symbols: []string{"101V6000"}})
	if err := c.WriteMessage(websocket.TextMessage, sub); err != nil {
		t.Fatalf("subscribe write: %v", err)
	}
	// 구독이 서버에 반영될 시간
	waitFor(t, func() bool { return len(srv.Hub().subs) == 1 && subscribedHas(srv, "101V6000") })

	// 미구독 종목 주입 → 수신 없어야
	if _, sent, _, err := srv.IngestKA(buildKA("105V3000", 100.00)); err != nil || sent != 0 {
		t.Fatalf("미구독 105V3000 sent=%d err=%v (want 0)", sent, err)
	}
	// 구독 종목 주입 → 수신
	if _, sent, _, err := srv.IngestKA(buildKA("101V6000", 265.75)); err != nil || sent != 1 {
		t.Fatalf("구독 101V6000 sent=%d err=%v (want 1)", sent, err)
	}

	// ws 에서 딱 1건(101V6000)만, last=265.75 JSON 수신
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatalf("json: %v (%s)", err, msg)
	}
	if got["kind"] != "fut.trade" || got["code"] != "101V6000" || got["last"].(float64) != 265.75 {
		t.Errorf("수신 JSON 예상밖: %s", msg)
	}

	// 미구독 종목 메시지는 큐에 없어야 (다음 read 는 타임아웃)
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Error("미구독 종목이 새어나옴 — 구독 필터 실패")
	}
}

func subscribedHas(srv *Server, code string) bool {
	srv.hub.mu.RLock()
	defer srv.hub.mu.RUnlock()
	for _, s := range srv.hub.subs {
		for _, c := range s.Subscribed() {
			if c == code {
				return true
			}
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("조건 대기 타임아웃")
}
