// Package integration 은 실 mymqd 인스턴스를 대상으로 동작하는 통합 테스트.
//
// MYMQD_HOST 환경변수가 비어있으면 자동 skip 되므로, broker 없이도 CI 의
// `go test ./...` 가 green 으로 유지된다. 활성화하려면:
//
//	MYMQD_HOST=10.0.0.10 MYMQD_PORT=11217 go test -v ./test/integration/...
//
// 핵심 검증: TestCkeyEcho 는 WTG Phase 1 의 GO/NO-GO 분기점이다.
package integration

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// brokerEndpoint 는 환경변수에서 broker 접속 정보를 읽고, 미설정 시 skip.
func brokerEndpoint(t *testing.T) (string, int) {
	t.Helper()
	host := os.Getenv("MYMQD_HOST")
	if host == "" {
		t.Skip("MYMQD_HOST 미설정 — 실 mymqd 통합 테스트 skip")
	}
	port := 11217
	if p := os.Getenv("MYMQD_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("invalid MYMQD_PORT %q: %v", p, err)
		}
		port = v
	}
	return host, port
}

// TestConnectAndHandshake 는 기본 TCP 연결 + DECLARE_SESSION 핸드셰이크를 검증한다.
func TestConnectAndHandshake(t *testing.T) {
	host, port := brokerEndpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := mymq.Open(ctx, host, port, mymq.Options{
		ApplName: "mci-it-handshake",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	si := c.SessionInfo()
	if si.ConnectionID == 0 {
		t.Errorf("ConnectionID should be assigned, got 0")
	}
	t.Logf("session: socket=%d session=%d conn=%d hb=%ds compress=%d/%d",
		si.SocketID, si.SessionID, si.ConnectionID,
		si.Heartbeat, si.CompressMethod, si.CompressSize)
}

// TestCkeyEcho 는 option C 멀티플렉싱의 GO/NO-GO 검증이다.
//
// 알려진 ckey 를 보내고 broker 가 응답에 그대로 echo 하는지 확인한다.
// 실패 시 libmymq-go 는 connection pool (option B) 모델로 폴백해야 한다.
func TestCkeyEcho(t *testing.T) {
	host, port := brokerEndpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := mymq.Open(ctx, host, port, mymq.Options{
		ApplName: "mci-it-ckey",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	const sentinel uint32 = 0xDEADBEEF

	in := &mymq.FrameInput{
		Func: mymq.FCAdmin,
		Subc: mymq.SubGetStatus,
		Dirf: mymq.DirIoctl,
		Keyc: mymq.KeySend,
		Ckey: sentinel,
	}
	r, err := c.Call(ctx, in)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	t.Logf("reply: func=%d subc=%d errn=%d body_len=%d ckey=0x%08X",
		r.Header.Func, r.Header.Subc, r.Header.Errn, len(r.Body), r.Header.Ckey)

	if r.Header.Ckey != sentinel {
		t.Fatalf("ckey echo 안 됨: 송신 0x%08X 수신 0x%08X — option C 불가, connection pool 폴백 필요",
			sentinel, r.Header.Ckey)
	}
}

// TestConcurrentCalls 는 다수 동시 요청을 보내서 멀티플렉싱 dispatch 가
// end-to-end 로 동작하는지 확인한다.
func TestConcurrentCalls(t *testing.T) {
	host, port := brokerEndpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := mymq.Open(ctx, host, port, mymq.Options{
		ApplName: "mci-it-concurrent",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	const N = 16
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err := c.Call(callCtx, &mymq.FrameInput{
				Func: mymq.FCAdmin,
				Subc: mymq.SubGetStatus,
				Dirf: mymq.DirIoctl,
				Keyc: mymq.KeySend,
			})
			errs <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent call %d: %v", i, err)
		}
	}
}

// TestUnsolicitedSmoke 는 짧은 시간 동안 unsolicited 메시지를 수집해서
// 카운트를 보고한다. 트래픽이 없으면 조용히 끝남 (실패 아님).
func TestUnsolicitedSmoke(t *testing.T) {
	host, port := brokerEndpoint(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := mymq.Open(ctx, host, port, mymq.Options{
		ApplName: "mci-it-unsol",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	deadline := time.After(5 * time.Second)
	count := 0
	sub := c.Subscribe()
	for {
		select {
		case msg, ok := <-sub:
			if !ok {
				t.Logf("subscription closed; total=%d", count)
				return
			}
			count++
			if count <= 3 {
				exch := ""
				if msg.Prefix != nil {
					exch = msg.Prefix.ExchangeString()
				}
				t.Logf("unsol[%d]: func=%d subc=%d exchange=%q body_len=%d",
					count, msg.Header.Func, msg.Header.Subc, exch, len(msg.Body))
			}
		case <-deadline:
			t.Logf("observed %d unsolicited messages in 5s", count)
			return
		}
	}
}
