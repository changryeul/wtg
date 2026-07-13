package tcp

import (
	"context"
	"encoding/json"
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

// stats HTTP — 연결/프레임/heartbeat 카운터와 연결별 상세가 노출된다.
func TestStatsEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := Config{UpstreamURL: upstream.URL, APIUser: "u", StatsAddr: "127.0.0.1:0"}
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := NewServer(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil || srv.StatsAddr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("listen 시작 안 됨")
		}
		time.Sleep(5 * time.Millisecond)
	}

	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// heartbeat 1회 + 전문 1회.
	_ = writeFrame(conn, nil)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = readFrame(conn, 1<<20)
	_ = writeFrame(conn, []byte("W1101T01        x"))
	_, _ = readFrame(conn, 1<<20)

	resp, err := http.Get("http://" + srv.StatsAddr().String() + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cors := resp.Header.Get("Access-Control-Allow-Origin"); cors != "*" {
		t.Errorf("CORS: %q", cors)
	}
	var st struct {
		Active     int64 `json:"active_conns"`
		Total      int64 `json:"total_conns"`
		FramesIn   int64 `json:"frames_in"`
		Heartbeats int64 `json:"heartbeats"`
		Conns      []struct {
			Remote string `json:"remote"`
			Frames int64  `json:"frames"`
		} `json:"conns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Active != 1 || st.Total != 1 {
		t.Errorf("conns: active=%d total=%d", st.Active, st.Total)
	}
	if st.Heartbeats != 1 || st.FramesIn != 1 {
		t.Errorf("counters: hb=%d frames_in=%d", st.Heartbeats, st.FramesIn)
	}
	if len(st.Conns) != 1 || st.Conns[0].Frames != 1 {
		t.Errorf("conn 상세: %+v", st.Conns)
	}

	// healthz.
	hr, err := http.Get("http://" + srv.StatsAddr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hr.Body.Close()
	if hr.StatusCode != 200 {
		t.Errorf("healthz: %d", hr.StatusCode)
	}

	// 연결 종료 → active 0.
	conn.Close()
	time.Sleep(100 * time.Millisecond)
	resp2, _ := http.Get("http://" + srv.StatsAddr().String() + "/stats")
	var st2 struct {
		Active int64 `json:"active_conns"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&st2)
	resp2.Body.Close()
	if st2.Active != 0 {
		t.Errorf("종료 후 active=%d", st2.Active)
	}
}

// 연결별 사용자 식별 — 전문 COMHDR 의 usid (offset 74, 30B) 를 캡처해
// stats 에 노출. HTS 는 연결당 1사용자 — 마지막 전문의 usid 가 연결 주인.
func TestStatsCapturesUsid(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := Config{UpstreamURL: upstream.URL, APIUser: "gw", StatsAddr: "127.0.0.1:0", ListenAddr: "127.0.0.1:0"}
	srv, err := NewServer(cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil || srv.StatsAddr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("listen 시작 안 됨")
		}
		time.Sleep(5 * time.Millisecond)
	}

	conn, _ := net.Dial("tcp", srv.Addr().String())
	defer conn.Close()
	// COMHDR 배치: trxc16+scrn6+loip16+auip16+maca20 = 74 → usid[30]
	buf := append([]byte("W1101T01"), []byte(strings.Repeat(" ", 8))...) // trxc 16B
	buf = append(buf, []byte(strings.Repeat(" ", 58))...)                // scrn..maca 58B
	buf = append(buf, []byte("hts-user-77"+strings.Repeat(" ", 19))...)  // usid 30B
	buf = append(buf, []byte("AH")...)
	_ = writeFrame(conn, buf)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readFrame(conn, 1<<20)

	resp, err := http.Get("http://" + srv.StatsAddr().String() + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		ForwardUser string `json:"forward_user"`
		Conns       []struct {
			Usid string `json:"usid"`
		} `json:"conns"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.ForwardUser != "gw" {
		t.Errorf("forward_user: %q", st.ForwardUser)
	}
	if len(st.Conns) != 1 || st.Conns[0].Usid != "hts-user-77" {
		t.Errorf("usid 캡처: %+v", st.Conns)
	}
}

// select-server (FC 0x01) — cs 가 연결 직후 보내는 [00 00 00 03][0c 00 01].
// edge-tcp 는 ip1==ip2 응답 (TH echo + ip1 24B + ip2 24B) 을 줘야 cs 가 진행.
func TestSelectServer(t *testing.T) {
	addr, _ := startServer(t, Config{UpstreamURL: "http://127.0.0.1:1", SelectServerIP: "10.9.8.7"})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// select-server 요청: TH = [0x0c, 0x00, 0x01] (BPI+EPI, FC=0x01).
	if err := writeFrame(conn, []byte{0x0c, 0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := readFrame(conn, 1<<20)
	if err != nil {
		t.Fatalf("select-server 응답 수신: %v", err)
	}
	// 응답: TH echo(3B) + ip1(24B) + ip2(24B) = 51B.
	if len(resp) != 3+24+24 {
		t.Fatalf("응답 길이 %d, want 51", len(resp))
	}
	if resp[0] != 0x0c || resp[2] != 0x01 {
		t.Errorf("TH echo: % x", resp[:3])
	}
	ip1 := strings.TrimRight(string(resp[3:27]), "\x00")
	ip2 := strings.TrimRight(string(resp[27:51]), "\x00")
	if ip1 != "10.9.8.7" || ip2 != "10.9.8.7" {
		t.Errorf("ip1=%q ip2=%q, want 둘 다 10.9.8.7", ip1, ip2)
	}
	if ip1 != ip2 {
		t.Error("ip1 != ip2 면 cs 가 재접속함")
	}

	// select-server 후 연결 유지 — heartbeat 왕복.
	if err := writeFrame(conn, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := readFrame(conn, 1<<20); err != nil || len(p) != 0 {
		t.Fatalf("select-server 후 heartbeat 실패: %v", err)
	}
}

// SelectServerIP 빈값이면 conn LocalAddr IP 사용 — ip1==ip2 유지.
func TestSelectServerDefaultIP(t *testing.T) {
	addr, _ := startServer(t, Config{UpstreamURL: "http://127.0.0.1:1"})
	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	_ = writeFrame(conn, []byte{0x0c, 0x00, 0x01})
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := readFrame(conn, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ip1 := strings.TrimRight(string(resp[3:27]), "\x00")
	ip2 := strings.TrimRight(string(resp[27:51]), "\x00")
	if ip1 == "" || ip1 != ip2 {
		t.Errorf("기본 IP: ip1=%q ip2=%q", ip1, ip2)
	}
}
