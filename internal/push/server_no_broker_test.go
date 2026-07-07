package push

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestServer_NoBroker_BootAndHTTPPush — NoBroker=true 로 mci-push 부팅 +
// HTTP push 호출 + dispatcher 가 정상 recvHTTP 카운트.
//
// broker 미존재 환경에서 mci-push 단독 동작 검증.
func TestServer_NoBroker_BootAndHTTPPush(t *testing.T) {
	// 빈 포트 확보 (httptest 가 아닌 실제 mci-push.Server 사용).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := DefaultConfig()
	cfg.ListenAddr = addr
	cfg.NoBroker = true           // ← 핵심
	cfg.PushSecret = "testsecret" // HTTP push 인증
	cfg.DevMode = true            // ws / auth 우회
	cfg.LogLevel = "warn"

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// 부팅 대기 — listener 가 accept 가능할 때까지 짧게 polling.
	waitListen(t, addr, 3*time.Second)

	// HTTP push 호출.
	body := `{"user":"dealer01","data":{"orderId":123,"status":"FILLED"}}`
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/internal/push",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Push-Secret", "testsecret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, buf)
	}
	var out HTTPPushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Injected {
		t.Errorf("Injected=true 기대")
	}

	// dispatcher 가 메시지 처리할 시간.
	time.Sleep(50 * time.Millisecond)
	stats := srv.dispatcher.Stats()
	if stats.RecvHTTP != 1 {
		t.Errorf("RecvHTTP=%d (기대 1)", stats.RecvHTTP)
	}
	if stats.RecvBroker != 0 {
		t.Errorf("RecvBroker=%d (기대 0 — broker 비활성)", stats.RecvBroker)
	}

	// /metrics 에 recv_http_total 노출 확인.
	mreq, _ := http.NewRequest("GET", "http://"+addr+"/metrics", nil)
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatalf("/metrics Do: %v", err)
	}
	defer mresp.Body.Close()
	mbody, _ := io.ReadAll(mresp.Body)
	if !strings.Contains(string(mbody), "mci_push_dispatcher_recv_http_total 1") {
		t.Errorf("/metrics 에 recv_http_total=1 미노출")
	}

	cancel()
	select {
	case err := <-errCh:
		// http.ErrServerClosed 또는 nil 기대.
		if err != nil && !strings.Contains(err.Error(), "closed") {
			t.Errorf("Start 종료 에러: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Server 종료 대기 timeout")
	}
}

// TestServer_NoBroker_DispatcherNilSub — broker 없이 dispatcher.Run 이
// panic 안 하고 ctx cancel 까지 정상 동작. nil-channel select 검증.
func TestServer_NoBroker_DispatcherNilSub(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := NewDispatcher(DispatcherOptions{
		Sub:      nil, // ← broker 없음
		Registry: NewRegistry(logger),
		Logger:   logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		disp.Run(ctx)
		close(done)
	}()

	// Sub 없이 select 가 ctx.Done 과 injectCh 만 기다리는지 검증.
	// 일정 시간 후 cancel → Run 정상 종료해야 (broker case 에 stuck X).
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatcher 종료 timeout — nil-channel select 정상 동작 X")
	}
}

// waitListen — addr 가 accept 가능할 때까지 polling.
func waitListen(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener %s 가 %v 안에 ready 안 됨", addr, timeout)
}
