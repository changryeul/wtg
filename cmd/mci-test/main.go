// mci-test 는 WTG (Winway Trading Gateway) Phase 1 검증 CLI 다.
//
// 주 목적: option C 멀티플렉싱의 GO/NO-GO 검증.
// 즉, mymqd 가 클라이언트 송신 ckey 를 응답에 echo back 하는지 확인.
//
// 사용 예:
//
//	mci-test --host=localhost --port=11217 --appl=mci-test
//	mci-test --ckey-echo --ckey=0xDEADBEEF
//
// broker 가 ckey 를 echo 하면 "PASS" 출력, 아니면 "FAIL" + 관찰된 ckey 출력.
// FAIL 시 libmymq-go 는 connection pool (option B) 모델로 폴백해야 한다.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

func main() {
	var (
		host    = flag.String("host", "127.0.0.1", "mymqd host")
		port    = flag.Int("port", 11217, "mymqd port (default = MQ_DMON_PORT)")
		appl    = flag.String("appl", "mci-test", "appl_name for DECLARE_SESSION")
		user    = flag.String("user", "", "user (optional)")
		ckeyHex = flag.String("ckey", "0xDEADBEEF", "ckey to send (hex)")
		echo    = flag.Bool("ckey-echo", true, "verify ckey is echoed in the reply")
		exch    = flag.String("xchg", "", "exchange to call (default: empty = admin path)")
		rkey    = flag.String("rkey", "", "routing key to call")
		timeout = flag.Duration("timeout", 5*time.Second, "request timeout")
	)
	flag.Parse()

	var ckey uint32
	_, err := fmt.Sscanf(*ckeyHex, "0x%X", &ckey)
	if err != nil {
		_, err = fmt.Sscanf(*ckeyHex, "%d", &ckey)
		if err != nil {
			die("invalid --ckey: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Fprintf(os.Stderr, "==> connecting to %s:%d as %q\n", *host, *port, *appl)
	c, err := mymq.Open(ctx, *host, *port, mymq.Options{
		ApplName:         *appl,
		Channel:          mymq.ChannelAdmin, // 검증용 CLI 는 ADM 채널로 식별
		User:             *user,
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		die("Open: %v", err)
	}
	defer c.Close()

	si := c.SessionInfo()
	fmt.Fprintf(os.Stderr, "==> handshake OK\n")
	fmt.Fprintf(os.Stderr, "    socket_id=%d session_id=%d connection_id=%d\n",
		si.SocketID, si.SessionID, si.ConnectionID)
	fmt.Fprintf(os.Stderr, "    routing=ap2ap=%d ap2br=%d bcast=%d\n",
		si.HowToRouting[0], si.HowToRouting[1], si.HowToBroadcast)
	fmt.Fprintf(os.Stderr, "    heartbeat=%ds compress=method:%d size:%d\n",
		si.Heartbeat, si.CompressMethod, si.CompressSize)

	if !*echo {
		return
	}

	fmt.Fprintf(os.Stderr, "==> sending GET_STATUS with ckey=0x%08X\n", ckey)
	callCtx, cancel2 := context.WithTimeout(ctx, *timeout)
	defer cancel2()

	in := &mymq.FrameInput{
		Func: mymq.FCAdmin,
		Subc: mymq.SubGetStatus,
		Dirf: mymq.DirIoctl,
		Keyc: mymq.KeySend,
		Ckey: ckey,
		Xchg: *exch,
		Rkey: *rkey,
	}
	r, err := c.Call(callCtx, in)
	if err != nil {
		die("Call: %v", err)
	}

	fmt.Fprintf(os.Stderr, "==> got reply\n")
	fmt.Fprintf(os.Stderr, "    func=%d subc=%d errn=%d\n",
		r.Header.Func, r.Header.Subc, r.Header.Errn)
	fmt.Fprintf(os.Stderr, "    sent ckey=0x%08X received ckey=0x%08X\n",
		ckey, r.Header.Ckey)
	if r.ErrMsg != "" {
		fmt.Fprintf(os.Stderr, "    errm=%q\n", r.ErrMsg)
	}
	fmt.Fprintf(os.Stderr, "    body len=%d\n", len(r.Body))

	if r.Header.Ckey == ckey {
		fmt.Println("PASS: broker 가 ckey 를 echo 함 — option C 멀티플렉싱 가능.")
		return
	}
	fmt.Printf("FAIL: ckey echo 안 됨 (송신 0x%08X, 수신 0x%08X). connection pool 폴백 필요.\n",
		ckey, r.Header.Ckey)
	os.Exit(2)
}

// die 는 stderr 에 에러 메시지를 출력하고 종료한다.
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mci-test: "+format+"\n", args...)
	os.Exit(1)
}
