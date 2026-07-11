// tcp-tester — mci-edge-tcp 검증용 레거시 cs 클라이언트 시뮬레이터.
//
// 지속 TCP 연결을 유지하며 주기 heartbeat (빈 프레임) 를 보내 echo 왕복
// RTT 를 찍는다. --send-file / --send-hex 로 고정폭 전문을 (옵션: 주기)
// 송신하고 응답 전문 preview 를 출력. 연결 단절 시 --reconnect 면 재접속.
//
// 사용:
//
//	tcp-tester --addr 127.0.0.1:5021                        # heartbeat 만
//	tcp-tester --addr 127.0.0.1:5021 --send-file msg.bin    # 전문 1회 + heartbeat
//	tcp-tester --addr <ec2>:5021 --send-file msg.bin --send-interval 5s --reconnect
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		addr         = flag.String("addr", "127.0.0.1:5021", "mci-edge-tcp 주소")
		hbInterval   = flag.Duration("hb-interval", 10*time.Second, "heartbeat 주기")
		sendFile     = flag.String("send-file", "", "송신할 고정폭 전문 파일 (bytes 그대로)")
		sendHex      = flag.String("send-hex", "", "송신할 전문 hex 문자열 (send-file 과 택일)")
		sendInterval = flag.Duration("send-interval", 0, "전문 반복 송신 주기 (0 = 1회)")
		reconnect    = flag.Bool("reconnect", false, "단절 시 5s 후 재접속")
		maxFrame     = flag.Int("max-frame", 1<<20, "수신 frame 상한")
	)
	flag.Parse()

	var msg []byte
	var err error
	if *sendFile != "" {
		if msg, err = os.ReadFile(*sendFile); err != nil {
			fatal("send-file 읽기: %v", err)
		}
	} else if *sendHex != "" {
		if msg, err = hex.DecodeString(strings.ReplaceAll(*sendHex, " ", "")); err != nil {
			fatal("send-hex 파싱: %v", err)
		}
	}

	for {
		err := run(*addr, *hbInterval, *sendInterval, msg, *maxFrame)
		fmt.Printf("[%s] 연결 종료: %v\n", ts(), err)
		if !*reconnect {
			os.Exit(1)
		}
		fmt.Printf("[%s] 5s 후 재접속...\n", ts())
		time.Sleep(5 * time.Second)
	}
}

// run — 연결 1회의 수명. 에러 시 반환 (호출자가 재접속 결정).
func run(addr string, hbInterval, sendInterval time.Duration, msg []byte, maxFrame int) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Printf("[%s] 연결됨 %s → %s\n", ts(), conn.LocalAddr(), conn.RemoteAddr())

	hbTick := time.NewTicker(hbInterval)
	defer hbTick.Stop()
	var sendTick <-chan time.Time
	if len(msg) > 0 && sendInterval > 0 {
		t := time.NewTicker(sendInterval)
		defer t.Stop()
		sendTick = t.C
	}

	// 최초 heartbeat + (있으면) 전문 1회.
	if err := roundTrip(conn, nil, maxFrame, "heartbeat"); err != nil {
		return err
	}
	if len(msg) > 0 {
		if err := roundTrip(conn, msg, maxFrame, "전문"); err != nil {
			return err
		}
	}

	for {
		select {
		case <-hbTick.C:
			if err := roundTrip(conn, nil, maxFrame, "heartbeat"); err != nil {
				return err
			}
		case <-sendTick:
			if err := roundTrip(conn, msg, maxFrame, "전문"); err != nil {
				return err
			}
		}
	}
}

// roundTrip — frame 송신 → 응답 1 frame 수신 (서버는 connection 당 직렬 처리).
func roundTrip(conn net.Conn, payload []byte, maxFrame int, kind string) error {
	t0 := time.Now()
	if err := writeFrame(conn, payload); err != nil {
		return fmt.Errorf("%s 송신: %w", kind, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp, err := readFrame(conn, maxFrame)
	if err != nil {
		return fmt.Errorf("%s 수신: %w", kind, err)
	}
	rtt := time.Since(t0)
	if payload == nil {
		fmt.Printf("[%s] heartbeat ok — rtt %.1fms\n", ts(), float64(rtt.Microseconds())/1000)
		return nil
	}
	fmt.Printf("[%s] 전문 응답 %dB — rtt %.1fms | preview: %s\n",
		ts(), len(resp), float64(rtt.Microseconds())/1000, preview(resp, 100))
	return nil
}

func preview(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, `\x%02x`, c)
		}
	}
	return sb.String()
}

func ts() string { return time.Now().Format("15:04:05.000") }

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tcp-tester: "+format+"\n", args...)
	os.Exit(2)
}

// framing — mci-edge-tcp 와 동일: [4B big-endian length][payload], 0 = heartbeat.

func readFrame(r io.Reader, maxFrame int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil
	}
	if int(n) > maxFrame {
		return nil, fmt.Errorf("frame %dB > 상한 %dB", n, maxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}
