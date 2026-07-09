package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// ws upgrade 가 proxy 를 통과해 echo 왕복하는지 — 모니터의 실사용 형태.
func TestWsmonProxyWebSocketRoundTrip(t *testing.T) {
	up := websocket.Upgrader{}
	var seenQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscribe" {
			t.Errorf("upstream path: %q", r.URL.Path)
		}
		seenQuery = r.URL.RawQuery
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		mt, msg, _ := c.ReadMessage()
		_ = c.WriteMessage(mt, append([]byte("echo:"), msg...))
	}))
	defer upstream.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/wsmon/{svc}/{rest...}",
		WsmonProxy("edge-price="+upstream.URL, quietLogger()))
	front := httptest.NewServer(mux)
	defer front.Close()

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") +
		"/v1/admin/wsmon/edge-price/v1/subscribe?x_wtg_user=op1"
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%+v)", err, resp)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "echo:ping" {
		t.Errorf("echo: %q", msg)
	}
	if !strings.Contains(seenQuery, "x_wtg_user=op1") {
		t.Errorf("인증 query 미전달: %q", seenQuery)
	}
}

// 미등록 svc → 404, upstream 죽음 → 502.
func TestWsmonProxyErrors(t *testing.T) {
	dead := httptest.NewServer(nil)
	dead.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/wsmon/{svc}/{rest...}",
		WsmonProxy("edge-price="+dead.URL, quietLogger()))
	front := httptest.NewServer(mux)
	defer front.Close()

	r1, _ := http.Get(front.URL + "/v1/admin/wsmon/unknown/v1/x")
	if r1.StatusCode != http.StatusNotFound {
		t.Errorf("unknown svc: %d", r1.StatusCode)
	}
	r2, _ := http.Get(front.URL + "/v1/admin/wsmon/edge-price/v1/x")
	if r2.StatusCode != http.StatusBadGateway {
		t.Errorf("dead upstream: %d", r2.StatusCode)
	}
}
