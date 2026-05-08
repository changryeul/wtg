package mymq

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
)

// fakeBroker 는 client.go / reconnect.go 의 통합 시나리오 검증을 위한
// 미니 mymqd 시뮬레이터다.
//
// 동작:
//   - 임의의 TCP 포트에 listen.
//   - 클라이언트가 보낸 DECLARE_SESSION / CONNECT 는 자동으로 응답 (ckey echo).
//   - 그 외 모든 프레임은 received 채널로 외부에 노출 → 테스트가 검증.
//   - Push() 로 외부에서 unsolicited 또는 reply 프레임을 raw 송신 가능.
//   - Close() 로 listener + 활성 연결 종료 (재연결 시나리오 시뮬레이션).
type fakeBroker struct {
	t        testing.TB
	listener net.Listener

	mu     sync.Mutex
	conn   net.Conn // 현재 연결된 클라이언트 (테스트는 1개 클라이언트 가정)
	closed bool

	// received 는 핸드셰이크가 아닌 모든 클라이언트 프레임을 채널로 통과시킨다.
	received chan *DecodedFrame

	// 핸드셰이크 응답 사전 설정 (테스트가 override 가능).
	sessionResp DeclareSessionResponse
	connectResp ConnectResponse

	wg sync.WaitGroup
}

// newFakeBroker 는 listen 후 accept loop 를 가동해서 클라이언트를 기다린다.
// 호출자는 t.Cleanup(b.Close) 로 정리해야 한다.
func newFakeBroker(t testing.TB) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &fakeBroker{
		t:        t,
		listener: ln,
		received: make(chan *DecodedFrame, 64),
		sessionResp: DeclareSessionResponse{
			SocketID:     42,
			SessionID:    100,
			ConnectionID: 12345,
			Heartbeat:    0, // 테스트는 자동 heartbeat 비활성 기본
		},
		connectResp: ConnectResponse{
			SocketID:     42,
			ConnectionID: 12345,
			QueueName:    "test_q",
			QueueSize:    64,
			Heartbeat:    0,
		},
	}
	b.wg.Add(1)
	go b.acceptLoop()
	return b
}

// addr 은 listen 중인 host:port 를 반환한다.
func (b *fakeBroker) addr() (string, int) {
	a := b.listener.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (b *fakeBroker) hostPort() (string, int) {
	host, port := b.addr()
	if host == "" || host == "::" {
		host = "127.0.0.1"
	}
	return host, port
}

// Close 는 listener + 활성 연결을 종료한다.
func (b *fakeBroker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	conn := b.conn
	b.conn = nil
	b.mu.Unlock()

	_ = b.listener.Close()
	if conn != nil {
		_ = conn.Close()
	}
	b.wg.Wait()
}

// CloseClientConn 은 활성 연결만 닫는다 (재연결 시나리오용).
// listener 는 살아있어서 재연결 시 새 연결 받을 수 있음.
func (b *fakeBroker) CloseClientConn() {
	b.mu.Lock()
	conn := b.conn
	b.conn = nil
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Push 는 활성 클라이언트 연결로 raw 프레임을 송신한다.
// 일반적으로 EncodeFrame() 결과를 그대로 넘김.
func (b *fakeBroker) Push(frame []byte) error {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return io.ErrClosedPipe
	}
	_, err := conn.Write(frame)
	return err
}

func (b *fakeBroker) acceptLoop() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.conn = conn
		b.mu.Unlock()
		b.wg.Add(1)
		go b.connLoop(conn)
	}
}

func (b *fakeBroker) connLoop(conn net.Conn) {
	defer b.wg.Done()
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(hdr)
		if length == 4 {
			// heartbeat — 무시 (또는 테스트가 카운트하려면 채널로).
			continue
		}
		if length < HdrSize {
			return
		}
		frame := make([]byte, length)
		copy(frame[:4], hdr)
		if _, err := io.ReadFull(conn, frame[4:]); err != nil {
			return
		}
		df, err := DecodeFrame(frame)
		if err != nil {
			continue
		}
		// 핸드셰이크 자동 응답.
		if df.Header.Func == FCCntl {
			switch df.Header.Subc {
			case SubDeclareSession:
				b.respondDeclareSession(df, conn)
				continue
			case SubConnect:
				b.respondConnect(df, conn)
				continue
			}
		}
		// 그 외는 외부 채널로 노출.
		select {
		case b.received <- df:
		default:
		}
	}
}

// respondDeclareSession 은 DECLARE_SESSION 응답 88바이트를 만들어 ckey echo 후 송신.
func (b *fakeBroker) respondDeclareSession(req *DecodedFrame, conn net.Conn) {
	body := make([]byte, declareSessionSize)
	// 클라이언트 영역 echo (선택).
	if len(req.Body) >= 16+16+4+20+4 {
		copy(body, req.Body[:16+16+4+20+4])
	}
	// 응답 영역 채우기.
	off := 16 + 16 + 4 + 20 + 4
	putU32(body[off:off+4], b.sessionResp.SocketID)
	off += 4
	putU32(body[off:off+4], b.sessionResp.SessionID)
	off += 4
	putU32(body[off:off+4], b.sessionResp.ConnectionID)
	off += 4
	body[off] = b.sessionResp.HowToRouting[0]
	body[off+1] = b.sessionResp.HowToRouting[1]
	off += 2
	body[off] = b.sessionResp.HowToBroadcast
	off++
	body[off] = b.sessionResp.LogSuffix
	off++
	putU32(body[off:off+4], b.sessionResp.Heartbeat)
	off += 4
	putU32(body[off:off+4], b.sessionResp.CompressMethod)
	off += 4
	putU32(body[off:off+4], b.sessionResp.CompressSize)

	frame, err := EncodeFrame(&FrameInput{
		Func: FCCntl,
		Subc: SubDeclareSession,
		Dirf: DirOrigin,
		Ckey: req.Header.Ckey, // option C 멀티플렉싱 — ckey echo
		Body: body,
	})
	if err != nil {
		b.t.Errorf("respondDeclareSession encode: %v", err)
		return
	}
	if _, err := conn.Write(frame); err != nil {
		// 클라이언트가 종료됐을 수 있음 — 무시.
		return
	}
}

// respondConnect 는 CONNECT 응답을 만들어 ckey echo 후 송신.
func (b *fakeBroker) respondConnect(req *DecodedFrame, conn net.Conn) {
	body := make([]byte, connectionSize)
	// 클라이언트 instance 영역 echo.
	if len(req.Body) >= instanceSize {
		copy(body, req.Body[:instanceSize])
	}
	off := instanceSize
	putU32(body[off:off+4], b.connectResp.SocketID)
	off += 4
	putU32(body[off:off+4], b.connectResp.ConnectionID)
	off += 4
	copy(body[off:off+16], b.connectResp.QueueName)
	off += 16
	putU32(body[off:off+4], b.connectResp.QueueKey)
	off += 4
	putU32(body[off:off+4], b.connectResp.QueueID)
	off += 4
	putU32(body[off:off+4], b.connectResp.QueueMsgID)
	off += 4
	putU32(body[off:off+4], b.connectResp.QueueSize)
	off += 4
	body[off] = b.connectResp.HowToRouting[0]
	body[off+1] = b.connectResp.HowToRouting[1]
	off += 2
	body[off] = b.connectResp.HowToBroadcast
	off++
	body[off] = b.connectResp.LogSuffix
	off++
	putU32(body[off:off+4], b.connectResp.Heartbeat)
	off += 4
	putU32(body[off:off+4], b.connectResp.CompressMethod)
	off += 4
	putU32(body[off:off+4], b.connectResp.CompressSize)

	frame, err := EncodeFrame(&FrameInput{
		Func: FCCntl,
		Subc: SubConnect,
		Dirf: DirOrigin,
		Ckey: req.Header.Ckey,
		Body: body,
	})
	if err != nil {
		b.t.Errorf("respondConnect encode: %v", err)
		return
	}
	if _, err := conn.Write(frame); err != nil {
		return
	}
}
