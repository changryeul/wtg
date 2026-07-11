package tcp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServer — 테스트용: :0 listen, 실제 주소 반환.
func startServer(t *testing.T, cfg Config) (addr string, cancel context.CancelFunc) {
	t.Helper()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := NewServer(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelFn := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("listen 시작 안 됨")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(cancelFn)
	return srv.Addr().String(), cancelFn
}

// heartbeat — 빈 frame 을 보내면 빈 frame 이 echo 된다 (연결 유지 확인 왕복).
func TestHeartbeatEcho(t *testing.T) {
	addr, _ := startServer(t, Config{UpstreamURL: "http://127.0.0.1:1"})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for i := 0; i < 3; i++ { // 주기 heartbeat 시뮬레이션 — 연결이 계속 유지되는지
		if err := writeFrame(conn, nil); err != nil {
			t.Fatalf("hb %d 송신: %v", i, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		p, err := readFrame(conn, 1<<20)
		if err != nil {
			t.Fatalf("hb %d 수신: %v", i, err)
		}
		if len(p) != 0 {
			t.Fatalf("hb echo 가 빈 frame 이 아님: %dB", len(p))
		}
	}
}

// 전문 왕복 — trxc 추출 → mci-api raw 모드 헤더로 forward → 응답 bytes frame.
func TestTxRoundTrip(t *testing.T) {
	reqMsg := []byte("W1101T01        " + strings.Repeat(" ", 496) + "1") // trxc 16B + 나머지
	repMsg := []byte("W1101T01  OUTPUT-MSG")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tx" {
			t.Errorf("path: %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type: %q", ct)
		}
		if a := r.Header.Get("X-WTG-Alias"); a != "W1101T01" {
			t.Errorf("X-WTG-Alias: %q", a)
		}
		if u := r.Header.Get("X-WTG-User"); u != "hts01" {
			t.Errorf("X-WTG-User: %q", u)
		}
		if ch := r.Header.Get("X-WTG-Channel"); ch != "HTS" {
			t.Errorf("X-WTG-Channel: %q", ch)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != string(reqMsg) {
			t.Errorf("body 무변형 실패: %dB", len(body))
		}
		_, _ = w.Write(repMsg)
	}))
	defer upstream.Close()

	addr, _ := startServer(t, Config{UpstreamURL: upstream.URL, APIUser: "hts01"})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := writeFrame(conn, reqMsg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := readFrame(conn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(repMsg) {
		t.Errorf("응답 frame: %q", got)
	}

	// 같은 연결로 두 번째 왕복 — connection 유지 검증.
	if err := writeFrame(conn, reqMsg); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(conn, 1<<20); err != nil {
		t.Fatalf("2번째 왕복: %v", err)
	}
}

// idle timeout — heartbeat 없이 방치하면 서버가 연결을 끊는다.
func TestIdleTimeoutCloses(t *testing.T) {
	addr, _ := startServer(t, Config{
		UpstreamURL: "http://127.0.0.1:1",
		IdleTimeout: 150 * time.Millisecond,
	})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFrame(conn, 1<<20); err == nil {
		t.Fatal("idle 인데 연결이 유지됨")
	}
}

// upstream 죽음 — 연결은 유지하고 에러 text 를 frame 으로 알림.
func TestUpstreamDownKeepsConnection(t *testing.T) {
	dead := httptest.NewServer(nil)
	dead.Close()

	addr, _ := startServer(t, Config{UpstreamURL: dead.URL, APIUser: "hts01"})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	msg := []byte("W1101T01        payload")
	if err := writeFrame(conn, msg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := readFrame(conn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "WTG-EDGE-TCP-ERROR:") {
		t.Errorf("에러 통지 frame: %q", got)
	}
	// 연결은 살아있어야 함 — heartbeat 왕복.
	if err := writeFrame(conn, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := readFrame(conn, 1<<20); err != nil || len(p) != 0 {
		t.Fatalf("에러 후 heartbeat 실패: %v", err)
	}
}

// 짧은 전문 (trxc 16B 미만) — 에러 frame 통지 후 연결 유지.
func TestShortFrameNotified(t *testing.T) {
	addr, _ := startServer(t, Config{UpstreamURL: "http://127.0.0.1:1"})
	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()

	_ = writeFrame(conn, []byte("short"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readFrame(conn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "전문 길이 부족") {
		t.Errorf("짧은 전문 통지: %q", got)
	}
}

// 다중 connection 동시 처리.
func TestConcurrentConnections(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	addr, _ := startServer(t, Config{UpstreamURL: upstream.URL, APIUser: "u"})
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			if err := writeFrame(conn, []byte("W1101T01        x")); err != nil {
				done <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, err = readFrame(conn, 1<<20)
			done <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 4 {
		t.Errorf("upstream hit: %d", hits.Load())
	}
}
