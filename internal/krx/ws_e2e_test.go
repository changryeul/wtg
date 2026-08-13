package krx

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

// buildA306Ffor — 합성 A306F 원 체결(173B). code/last(=cprc) 지정.
func buildA306Ffor(code string, last float64) []byte {
	b := make([]byte, 173)
	for i := range b {
		b[i] = ' '
	}
	put := func(off, n int, s string) {
		if len(s) > n {
			s = s[:n]
		}
		copy(b[off:off+n], s)
	}
	put(0, 5, "A306F")
	put(17, 12, code)
	put(35, 12, "090005123456")
	put(47, 9, fmt.Sprintf("%9.02f", last)) // cprc 체결가
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
	if _, sent, _, err := srv.IngestA306F(buildA306Ffor("105V3000", 100.00)); err != nil || sent != 0 {
		t.Fatalf("미구독 105V3000 sent=%d err=%v (want 0)", sent, err)
	}
	// 구독 종목 주입 → 수신
	if _, sent, _, err := srv.IngestA306F(buildA306Ffor("101V6000", 265.75)); err != nil || sent != 1 {
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

	// 같은 구독 종목의 호가(B606F)도 Ingest 자동판별로 fan-out 되는지 (conn 정상일 때 먼저)
	if _, sent, _, err := srv.Ingest(buildB606Ffor("101V6000")); err != nil || sent != 1 {
		t.Fatalf("호가 B606F sent=%d err=%v (want 1)", sent, err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg2, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("호가 read: %v", err)
	}
	var book map[string]interface{}
	_ = json.Unmarshal(msg2, &book)
	if book["kind"] != "fut.book" || book["code"] != "101V6000" {
		t.Errorf("호가 JSON 예상밖: %s", msg2)
	}
	if ask, ok := book["ask"].([]interface{}); !ok || len(ask) != 5 {
		t.Errorf("호가 ask 5단 아님: %s", msg2)
	}

	// 미구독 종목이 client 큐에 없음을 마지막에 확인 (read-timeout 이 conn 을 끝내므로 맨끝).
	// 서버측 sent=0 으로 이미 증명됐고, 여기선 client 큐가 빈 것만 재확인.
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Error("미구독 종목이 새어나옴 — 구독 필터 실패")
	}
}

// buildB606Ffor — 지정 종목의 최소 B606F(호가) 원 TR(324B).
func buildB606Ffor(code string) []byte {
	b := make([]byte, 324)
	for i := range b {
		b[i] = ' '
	}
	copy(b[0:5], "B606F")
	copy(b[17:29], code)
	copy(b[35:47], "090005123456")
	// hoga[0] @47 — sprc@0/bprc@9 1단만 값.
	copy(b[47:56], fmt.Sprintf("%9.02f", 265.80))
	copy(b[56:65], fmt.Sprintf("%9.02f", 265.75))
	return b
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
