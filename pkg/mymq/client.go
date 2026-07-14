package mymq

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Client 라이프사이클 에러.
var (
	ErrClientClosed   = errors.New("mymq: 클라이언트 종료됨")
	ErrSessionTimeout = errors.New("mymq: DECLARE_SESSION 타임아웃")
)

// Options 는 Client 동작을 제어한다.
type Options struct {
	// ApplName 은 DECLARE_SESSION 의 appl_name 필드 (최대 16바이트). 필수.
	// broker 의 클라이언트 식별자로 사용됨. WTG 컨벤션은 conventions.go 의
	// ApplMci* 상수 참조 (mci-api / mci-push / mci-price / mci-admin / mci-e*).
	ApplName string

	// Instance 는 다중 인스턴스 운영 시의 일련번호 (옵션, 1..99 권장).
	// 0 이면 ApplName 그대로 사용. 양수면 "<ApplName>-NN" 으로 자동 조합.
	// 예: ApplName="mci-api" + Instance=1 → 최종 "mci-api-01".
	Instance int

	// Channel 은 사용자 단말 채널 식별자 (mqhdr.chan[4] 에 들어감).
	// WTG 의 모든 outgoing 프레임에 자동 첨부된다. 비어있으면 모두 0 으로
	// 송신. 일반적으로 ChannelWeb / ChannelMobile / ChannelAdmin 사용.
	Channel ChannelCode

	// User 는 사용자 식별자 (옵션). 비어있으면 서버가 fallback 처리.
	User string

	// ExternalIP/Port 는 게이트웨이 뒤의 실제 클라이언트 식별 (옵션).
	ExternalIP   string
	ExternalPort uint32

	// Queue 는 CONNECT 핸드셰이크 (mymq_openx 대응) 사용 시 큐/exchange
	// 선언 정보. nil 이면 DECLARE_SESSION 만 보내고 트랜잭션 reply 만 받음.
	//
	// 채워주면 broker 측에 큐가 등록되어 unsolicited 메시지 (FC_CAST/FC_PUSH) 를
	// 자동 수신할 수 있다. mci-push / mci-price 가 이 옵션을 사용한다.
	Queue *QueueOptions

	// DialTimeout 은 TCP 연결 시도 timeout (기본 5초).
	DialTimeout time.Duration

	// HandshakeTimeout 은 DECLARE_SESSION round-trip timeout (기본 5초).
	HandshakeTimeout time.Duration

	// HeartbeatInterval 은 broker 가 알려준 값을 덮어쓴다 (0 = 서버값 사용).
	HeartbeatInterval time.Duration

	// Reconnect 가 nil 이 아니면 supervisor goroutine 이 connection 끊김 시
	// 자동으로 재연결한다. 기본 nil = 1회용 연결 (끊기면 종료).
	Reconnect *ReconnectOptions

	// Logger 는 구조화 로깅용 slog.Logger. nil 이면 모든 로그가 io.Discard
	// 로 흘러간다. 운영 환경에서는 slog.New(slog.NewJSONHandler(...)) 권장.
	Logger *slog.Logger

	// TLS 가 nil 이 아니면 broker 와 TLS 로 연결한다 (tls.Dial).
	// broker 측에서 TLS listener 를 노출해야 하며, 운영팀과 별도 합의가 필요하다
	// (docs/broker-tls.md 참고). nil 이면 plain TCP — 기존 동작.
	//
	// 일반적 사용:
	//
	//	pkg/tlsutil.LoadClient(...) 결과를 그대로 전달.
	//
	// Reconnect 시에도 동일 *tls.Config 가 재사용된다.
	TLS *tls.Config

	// SubBufferSize 는 Subscribe() 가 노출하는 unsolicited 채널 (subCh) 의
	// buffered capacity. 0 이면 default 256.
	//
	// 채널이 가득 차면 dispatch 가 silent drop 하므로 (broker → mci-* 사이
	// 가장 흔한 backpressure 지점), 고부하 환경에선 큰 값으로 (8192~) 운영
	// 권장. Subscribe consumer 가 빠르게 처리할 수 있어야 효과 있음 — 단순히
	// 늘리는 게 답은 아니고, drop 카운터 (SubDrops()) 로 실측 후 조정.
	SubBufferSize int

	// Metrics — connection lifecycle 이벤트 hook (Prometheus 등록용). 모든
	// 필드 nil 가능. 호출 위치: disconnect 직후 / reconnect 성공 / pending
	// RPC abort / heartbeat watchdog timeout.
	//
	// hook 은 짧은 stateless 함수여야 — counter.Inc 같은 cheap 호출만 권장.
	// 무거운 작업은 별도 goroutine 으로 분리.
	Metrics MetricsHook
}

// MetricsHook — broker connection lifecycle 이벤트 callback.
type MetricsHook struct {
	// OnDisconnect — connection 끊김 직후 (재연결 시도 전).
	OnDisconnect func(cause error)
	// OnReconnect — 재연결 성공. attempts = 누적 시도 횟수,
	// duration = disconnect 시점부터 핸드셰이크 성공까지 wallclock.
	OnReconnect func(attempts int, duration time.Duration)
	// OnInflightAborted — failPending 으로 ErrBroker 통보된 pending RPC 수.
	OnInflightAborted func(count int)
	// OnHeartbeatTimeout — heartbeat watchdog 이 2*interval 내 무소식으로
	// connection 사망 판정한 시점 (TCP 끊김보다 빠른 감지).
	OnHeartbeatTimeout func()
}

// applySubBufferDefault — Options.SubBufferSize 가 0 이면 default 적용.
// 노출은 테스트 / Open 양쪽에서 일관된 default 보장 위함.
func applySubBufferDefault(o *Options) {
	if o.SubBufferSize == 0 {
		o.SubBufferSize = 256
	}
}

// dialBroker 는 Options.TLS 활성 시 tls.Dial, 아니면 plain TCP.
//
// Open 과 tryReconnect 가 공유 — 일관된 dial 동작을 보장.
// 핸드셰이크는 호출자가 별도 처리 (이 함수는 transport 만 책임).
func dialBroker(ctx context.Context, addr string, opts *Options) (net.Conn, error) {
	d := net.Dialer{Timeout: opts.DialTimeout}
	if opts.TLS == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()
	tlsDialer := &tls.Dialer{NetDialer: &d, Config: opts.TLS}
	return tlsDialer.DialContext(dialCtx, "tcp", addr)
}

// effectiveApplName 은 Instance 가 설정된 경우 "<ApplName>-NN" 으로
// 합성한 최종 ApplName 을 반환한다. 16바이트 제한이 있으므로 호출자가
// ApplName 길이를 미리 짧게 유지해야 한다.
func (o *Options) effectiveApplName() string {
	if o.Instance <= 0 {
		return o.ApplName
	}
	return fmt.Sprintf("%s-%02d", o.ApplName, o.Instance)
}

// Reply 는 동기 Call() 의 결과.
type Reply struct {
	Header  Header
	Body    []byte
	ErrMsg  string
	Errn    uint32
	Cookie  *Cookie
	Decoded *DecodedFrame
}

// Unsolicited 는 서버 측 능동 push 메시지 (broadcast/push/signal).
//
// broker 가 FC_CAST/FC_PUSH/FC_SIGNAL 프레임을 broadcast prefix 와 함께 보낼 때
// (QF_UNSOL_HDR 활성화된 클라이언트의 경우), Prefix 가 채워지고 Body 에는
// 80바이트 prefix 가 제외된 페이로드가 담긴다. prefix 가 없거나 body 가
// 짧은 경우 Prefix 는 nil 이고 Body 는 raw payload 그대로 반환된다.
type Unsolicited struct {
	Header  Header
	Prefix  *BroadcastHeader
	Body    []byte
	ErrMsg  string
	Decoded *DecodedFrame
}

// Client 는 MyMQ broker 클라이언트.
//
// mqhdr.ckey 필드를 correlation_id 로 활용해서 단일 TCP 연결에서 다수의
// 동시 Call() 을 멀티플렉싱한다. WTG 의 모든 Go 서비스(mci-api / push /
// price / admin)가 이 Client 를 통해 mymqd 와 통신한다.
type Client struct {
	opts        Options
	session     DeclareSessionResponse
	connectInfo *ConnectResponse // CONNECT 핸드셰이크 사용 시에만 채워짐
	whoamiSc    uint32           // session connection id (= ConnectionID)
	chanCode    [4]byte          // opts.Channel 의 패딩된 4바이트 — 모든 송신 프레임에 자동 첨부

	// 재연결 시 재사용할 접속 파라미터.
	host string
	port int

	// 활성 connection 의 가변 상태. 재연결 시 connMu 보호 하에 swap 된다.
	connMu sync.Mutex
	conn   net.Conn
	doneCh chan struct{} // 현재 readLoop 가 종료되면 close

	writeMu sync.Mutex // 프레임 송신 직렬화
	closeMu sync.Mutex
	closed  atomic.Bool // 영구 종료
	closing atomic.Bool // Close() 진입 시점 플래그 (supervisor 가 보고 종료 결정)

	pending  sync.Map      // ckey(uint32) -> chan *Reply  (동시 Call 매칭 테이블)
	nextCkey atomic.Uint32 // 다음 ckey 할당용 카운터

	subCh    chan *Unsolicited // unsolicited 메시지 채널 (Subscribe() 가 노출)
	subDrops atomic.Uint64     // subCh 가 가득 차서 drop 된 누적 메시지 수
	// (Options.SubBufferSize 와 함께 backpressure 진단). SubDrops() 로 노출.

	// 직전 readLoop 종료 사유 (재연결 진단용).
	// atomic.Value 는 첫 Store 의 concrete type 으로 고정되므로 wrapper 사용.
	readErr atomic.Pointer[errBox]

	lastRecv atomic.Int64 // 마지막 수신 시각 (unix nano) — heartbeat watchdog
	lastSent atomic.Int64 // 마지막 송신 시각 (unix nano) — idle suppression

	reconnecting atomic.Bool // supervisor 가 재연결 시도 중
}

// Open 은 mymqd 에 TCP 연결 + DECLARE_SESSION 핸드셰이크를 수행하고
// reader goroutine 을 시작한다. heartbeat 자동 송신 루프도 함께 가동된다.
func Open(ctx context.Context, host string, port int, opts Options) (*Client, error) {
	if opts.ApplName == "" {
		return nil, errors.New("mymq: ApplName 필수")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 5 * time.Second
	}
	applySubBufferDefault(&opts)

	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := dialBroker(ctx, addr, &opts)
	if err != nil {
		return nil, fmt.Errorf("mymq: dial %s: %w", addr, err)
	}

	c := &Client{
		opts:     opts,
		host:     host,
		port:     port,
		conn:     conn,
		chanCode: opts.Channel.Bytes(),
		subCh:    make(chan *Unsolicited, opts.SubBufferSize),
		doneCh:   make(chan struct{}),
	}
	// ckey 0 은 서버측 unsolicited 메시지 식별자로 예약 — 카운터는 1 부터 발급.
	c.nextCkey.Store(0)

	go c.readLoop()

	// 큐 옵션이 있으면 CONNECT (mymq_openx 대응), 없으면 DECLARE_SESSION.
	var hsErr error
	if opts.Queue != nil {
		hsErr = c.connectHandshake(ctx)
	} else {
		hsErr = c.handshake(ctx)
	}
	if hsErr != nil {
		_ = c.Close()
		return nil, hsErr
	}

	// broker 가 알려준 heartbeat 주기로 자동 송신 루프 가동.
	// 0 이면 자동 heartbeat 비활성 (호출자가 SendHeartbeat 직접 호출).
	if interval := c.heartbeatInterval(); interval > 0 {
		go c.heartbeatLoop(interval)
	}

	// Reconnect 옵션이 있으면 supervisor goroutine 가동.
	if opts.Reconnect != nil {
		go c.supervisorLoop(context.Background())
	}
	c.logBase().With(slog.Uint64(LogKeyConnID, uint64(c.whoamiSc))).Info("연결 성공")
	return c, nil
}

// heartbeatInterval 은 실제 사용할 heartbeat 주기를 결정한다.
// 우선순위: opts.HeartbeatInterval (override) > 서버 응답값 > 0 (off).
func (c *Client) heartbeatInterval() time.Duration {
	if c.opts.HeartbeatInterval > 0 {
		return c.opts.HeartbeatInterval
	}
	if c.session.Heartbeat > 0 {
		return time.Duration(c.session.Heartbeat) * time.Second
	}
	return 0
}

// heartbeatLoop 은 connection 이 송신 측에서 idle 인 동안 주기적으로 4바이트
// 빈 프레임을 보내고, 수신 측에서 2 * interval 동안 침묵하면 connection 을
// 사망 처리한다 (mq_socket.c 의 C 클라이언트와 동일 동작).
//
// 자기가 가동될 때의 doneCh 를 capture 해서, 재연결 후 새 heartbeatLoop 이
// 가동되더라도 이전 인스턴스가 새 connection 에 영향을 주지 않게 한다.
func (c *Client) heartbeatLoop(interval time.Duration) {
	c.connMu.Lock()
	doneCh := c.doneCh
	c.connMu.Unlock()
	if doneCh == nil {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadInterval := 2 * interval

	for {
		select {
		case <-doneCh:
			return
		case now := <-ticker.C:
			if c.closed.Load() {
				return
			}
			// 수신 watchdog: 2*interval 동안 broker 로부터 무소식이면 사망 판정.
			lastRecv := time.Unix(0, c.lastRecv.Load())
			if !lastRecv.IsZero() && now.Sub(lastRecv) > deadInterval {
				c.storeReadErr(errors.New("mymq: heartbeat timeout"))
				c.logBase().With(slog.Duration(LogKeyHeartbeat, deadInterval)).
					Warn("heartbeat 타임아웃 — connection 사망 처리")
				if hook := c.opts.Metrics.OnHeartbeatTimeout; hook != nil {
					hook()
				}
				c.connMu.Lock()
				if c.conn != nil {
					_ = c.conn.Close() // readLoop 의 read 를 깨움
				}
				c.connMu.Unlock()
				return
			}
			// 송신 watchdog: 최근에 뭔가 보냈으면 skip.
			lastSent := time.Unix(0, c.lastSent.Load())
			if !lastSent.IsZero() && now.Sub(lastSent) < interval {
				continue
			}
			if err := c.writeHeartbeat(); err != nil {
				return
			}
		}
	}
}

// Subscribe returns the channel of unsolicited (push/broadcast) messages.
// The channel is closed when the client disconnects.
func (c *Client) Subscribe() <-chan *Unsolicited {
	return c.subCh
}

// SubDrops 는 subCh 가 가득 차서 dispatch 가 drop 한 unsolicited 메시지의
// 누적 count. backpressure 진단용 — 정상 운영에선 0 이어야 하고, 증가하면
// (a) Options.SubBufferSize 가 작거나 (b) Subscribe consumer 가 느림.
func (c *Client) SubDrops() uint64 {
	return c.subDrops.Load()
}

// SubBufferCapacity 는 현재 subCh 의 buffered capacity (디버그용).
func (c *Client) SubBufferCapacity() int {
	return cap(c.subCh)
}

// SessionInfo 는 broker 가 핸드셰이크 응답으로 알려준 세션 파라미터를 반환한다.
// CONNECT/DECLARE_SESSION 어느 쪽이든 공통 필드는 동일하게 채워진다.
func (c *Client) SessionInfo() DeclareSessionResponse {
	return c.session
}

// Reconnecting 은 supervisor 가 재연결 시도 중인지 반환. 운영 health endpoint 가
// "broker 와의 통신 가능 여부" 표시할 때 사용. nil receiver 안전.
func (c *Client) Reconnecting() bool {
	if c == nil {
		return false
	}
	return c.reconnecting.Load()
}

// Closed 은 Client 가 종료된 상태인지 반환 (closing 또는 영구 종료).
func (c *Client) Closed() bool {
	if c == nil {
		return true
	}
	return c.closed.Load() || c.closing.Load()
}

// ConnectInfo 는 CONNECT 핸드셰이크 사용 시 broker 가 부여한 큐 정보를 반환한다.
// DECLARE_SESSION 만 사용한 클라이언트의 경우 nil.
func (c *Client) ConnectInfo() *ConnectResponse {
	return c.connectInfo
}

// Close 는 클라이언트 connection 을 종료한다. 자동 재연결이 활성화되어 있어도
// supervisor 가 closing flag 를 보고 멈춘다.
func (c *Client) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed.Swap(true) {
		return nil
	}
	c.closing.Store(true)

	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()

	var err error
	if conn != nil {
		err = conn.Close()
	}
	// in-flight Call 모두 통보 후 정리.
	c.failPending(ErrClientClosed)
	// 영구 채널 close — Subscribe() 사용자에게 종료 알림.
	close(c.subCh)
	return err
}

// nextCorrelation allocates a unique ckey, skipping 0.
func (c *Client) nextCorrelation() uint32 {
	for {
		v := c.nextCkey.Add(1)
		if v != 0 {
			return v
		}
	}
}

// writeFrame 은 단일 outbound 프레임을 connection 에 송신한다.
// 프레임은 length-prefixed 이며 EncodeFrame 이 이미 길이를 바이트 0..3 에
// 채워놓는다. writeMu 로 직렬화하므로 다수 goroutine 에서 안전하게 호출 가능.
//
// 재연결 시 c.conn 이 swap 되므로 connMu 로 현재 활성 conn 을 가져온다.
func (c *Client) writeFrame(frame []byte) error {
	if c.closed.Load() {
		return ErrClientClosed
	}
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return ErrClientClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := conn.Write(frame)
	if err == nil {
		c.lastSent.Store(time.Now().UnixNano())
	}
	return err
}

// writeHeartbeat sends a 4-byte empty frame.
func (c *Client) writeHeartbeat() error {
	var hb [4]byte
	binary.BigEndian.PutUint32(hb[:], 4)
	return c.writeFrame(hb[:])
}

// handshake 는 DECLARE_SESSION 을 보내고 응답을 기다린다 (단순 핸드셰이크).
// CONNECT 보다 가벼우며, request/reply 만 주고받는 클라이언트(mci-api 등)에 적합.
//
// ckey 는 일반 멀티플렉싱 슬롯으로 발급되어 reader 가 매칭 dispatch 한다.
// broker 가 control 응답에 ckey 를 echo 하지 않을 가능성에 대비해 구현된
// fallback 은 readLoop 의 unsolicited 분기에서 처리된다.
func (c *Client) handshake(ctx context.Context) error {
	req := &DeclareSessionRequest{
		ApplName:     c.opts.effectiveApplName(),
		User:         c.opts.User,
		Pid:          uint32(processID()),
		ExternalIP:   c.opts.ExternalIP,
		ExternalPort: c.opts.ExternalPort,
	}
	body := EncodeDeclareSessionRequest(req)

	// Reserve ckey for handshake using a normal correlation slot, so the
	// reader can dispatch the reply via pending. If broker doesn't echo
	// ckey for control responses, we'll need to revisit this; recv side
	// also has a fallback that catches DECLARE_SESSION responses.
	ckey := c.nextCorrelation()
	replyCh := make(chan *Reply, 1)
	c.pending.Store(ckey, replyCh)
	defer c.pending.Delete(ckey)

	frame, err := EncodeFrame(&FrameInput{
		Func: FCCntl,
		Subc: SubDeclareSession,
		Dirf: DirIoctl,
		Keyc: KeySend,
		Ckey: ckey,
		Body: body,
	})
	if err != nil {
		return fmt.Errorf("encode declare_session: %w", err)
	}
	if err := c.writeFrame(frame); err != nil {
		return fmt.Errorf("send declare_session: %w", err)
	}

	hsCtx, cancel := context.WithTimeout(ctx, c.opts.HandshakeTimeout)
	defer cancel()
	select {
	case r := <-replyCh:
		if r.Errn != 0 {
			return fmt.Errorf("declare_session: errn=%d %s", r.Errn, r.ErrMsg)
		}
		resp, err := DecodeDeclareSessionResponse(r.Body)
		if err != nil {
			return fmt.Errorf("decode declare_session response: %w", err)
		}
		c.session = resp
		c.whoamiSc = resp.ConnectionID
		return nil
	case <-hsCtx.Done():
		return ErrSessionTimeout
	case <-c.currentDoneCh():
		if err := c.lastReadErr(); err != nil {
			return err
		}
		return ErrClientClosed
	}
}

// connectHandshake 는 CONNECT (FC_CNTL/SubConnect) 를 보내고 응답을 기다린다.
// mymq_openx 에 대응하는 풍부한 핸드셰이크로, 큐/exchange 선언 + unsolicited
// 수신 등록을 한 번에 처리한다.
//
// 응답으로 받은 broker 정보(scid, queue 정보, heartbeat 등)는 c.session 호환
// 필드로 매핑되어 자동 heartbeat 루프 등 다운스트림 로직에서 그대로 사용된다.
func (c *Client) connectHandshake(ctx context.Context) error {
	q := c.opts.Queue
	req := &connectRequest{
		MqUser: c.opts.User,
		Pid:    uint32(processID()),
		MyName: c.opts.effectiveApplName(),
		ChIpad: c.opts.ExternalIP,
		ChPort: c.opts.ExternalPort,
		ExName: q.ExchangeName,
		ExType: uint32(q.ExchangeType),
		QuName: q.Name,
		QuFlag: q.Flags,
		QuAttr: uint32(q.Attr),
		QuSize: q.Size,
		QuExpt: q.ExportDomain,
	}
	body := encodeConnectRequest(req)

	ckey := c.nextCorrelation()
	replyCh := make(chan *Reply, 1)
	c.pending.Store(ckey, replyCh)
	defer c.pending.Delete(ckey)

	frame, err := EncodeFrame(&FrameInput{
		Func: FCCntl,
		Subc: SubConnect,
		Dirf: DirIoctl,
		Keyc: KeySend,
		Ckey: ckey,
		Body: body,
	})
	if err != nil {
		return fmt.Errorf("encode connect: %w", err)
	}
	if err := c.writeFrame(frame); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	hsCtx, cancel := context.WithTimeout(ctx, c.opts.HandshakeTimeout)
	defer cancel()
	select {
	case r := <-replyCh:
		if r.Errn != 0 {
			return fmt.Errorf("connect: errn=%d %s", r.Errn, r.ErrMsg)
		}
		resp, err := decodeConnectResponse(r.Body)
		if err != nil {
			return fmt.Errorf("decode connect response: %w", err)
		}
		// CONNECT 응답을 DeclareSessionResponse 와 호환되는 필드로 채워서
		// 자동 heartbeat 루프 등 기존 로직이 그대로 동작하게 한다.
		c.session = DeclareSessionResponse{
			SocketID:       resp.SocketID,
			ConnectionID:   resp.ConnectionID,
			HowToRouting:   resp.HowToRouting,
			HowToBroadcast: resp.HowToBroadcast,
			LogSuffix:      resp.LogSuffix,
			Heartbeat:      resp.Heartbeat,
			CompressMethod: resp.CompressMethod,
			CompressSize:   resp.CompressSize,
		}
		c.whoamiSc = resp.ConnectionID
		c.connectInfo = &resp
		return nil
	case <-hsCtx.Done():
		return ErrSessionTimeout
	case <-c.currentDoneCh():
		if err := c.lastReadErr(); err != nil {
			return err
		}
		return ErrClientClosed
	}
}

// readLoop 는 broker 로부터 length-prefixed 프레임을 읽어서 ckey 매칭으로
// pending Call 에 전달하거나, 매칭되지 않으면 subCh 로 unsolicited 전달한다.
//
// 종료 시 doneCh 만 close 하고 subCh 는 그대로 둔다 (subCh 는 영구 채널이며
// Close() 에서만 close 됨). 종료 후 supervisor 가 doneCh 신호로 깨어나서
// 재연결 또는 영구 종료를 처리한다.
func (c *Client) readLoop() {
	c.connMu.Lock()
	conn := c.conn
	doneCh := c.doneCh
	c.connMu.Unlock()
	if conn == nil || doneCh == nil {
		return
	}
	defer close(doneCh)

	hdrBuf := make([]byte, 4)
	for {
		// Read the 4-byte length prefix.
		if _, err := io.ReadFull(conn, hdrBuf); err != nil {
			c.storeReadErr(err)
			return
		}
		c.lastRecv.Store(time.Now().UnixNano())
		length := binary.BigEndian.Uint32(hdrBuf)
		if length < 4 {
			c.storeReadErr(fmt.Errorf("mymq: invalid frame length %d", length))
			return
		}
		if length == 4 {
			// Heartbeat; nothing else to read.
			continue
		}
		if length > MaxMsgSize {
			c.storeReadErr(fmt.Errorf("mymq: frame too large: %d", length))
			return
		}
		// 프레임 본문을 읽어서 prefix 와 합쳐 DecodeFrame 이 길이 검증 가능하게.
		frame := make([]byte, length)
		copy(frame[:4], hdrBuf)
		if _, err := io.ReadFull(conn, frame[4:]); err != nil {
			c.storeReadErr(err)
			return
		}

		df, err := DecodeFrame(frame)
		if err != nil {
			// Don't kill the connection on a parse error — log and continue.
			// (In Phase 1 we just drop; logger added later.)
			continue
		}
		c.dispatch(df)
	}
}

func (c *Client) dispatch(df *DecodedFrame) {
	// 본문이 압축되어 있으면 자동 해제. 실패 시 raw 그대로 전달하고 warn 만.
	// (호출자가 Decoded.Body 와 BodyZipf 를 직접 보고 처리할 수도 있다.)
	body := c.decompressBody(df)

	ckey := df.Header.Ckey

	// Pending Call match — option C multiplexing.
	if ckey != 0 {
		if v, ok := c.pending.LoadAndDelete(ckey); ok {
			ch := v.(chan *Reply)
			ch <- &Reply{
				Header:  df.Header,
				Body:    cloneBytes(body),
				ErrMsg:  df.ErrMsg,
				Errn:    df.Header.Errn,
				Cookie:  df.Cookie,
				Decoded: df,
			}
			return
		}
	}

	// Unsolicited (push/broadcast/signal/control without correlation).
	c.deliverUnsolicited(df)
}

// deliverUnsolicited 는 dispatch 의 unsolicited 분기 — subCh 로 비차단 송신.
// 채널이 가득 차면 silent drop 대신 subDrops 카운터를 증가시켜 SubDrops()
// 로 외부 진단 가능하게 한다 (load-gen 측정 시 broker → consumer backpressure
// 의 정량적 지표).
func (c *Client) deliverUnsolicited(df *DecodedFrame) {
	body := df.Body
	out := cloneBytes(body)
	var prefix *BroadcastHeader
	switch df.Header.Func {
	case FCCast, FCPush, FCSignal:
		if h, payload, err := SplitBroadcast(out); err == nil && h != nil {
			prefix = h
			out = payload
		}
	}

	select {
	case c.subCh <- &Unsolicited{
		Header:  df.Header,
		Prefix:  prefix,
		Body:    out,
		ErrMsg:  df.ErrMsg,
		Decoded: df,
	}:
	default:
		c.subDrops.Add(1)
	}
}

// decompressBody 는 df.Body 가 압축되어 있으면 평문으로 복원해서 반환한다.
// 실패 시 raw 본문을 그대로 반환하고 warn 로그만 남긴다 (호출자가 직접 처리
// 가능하도록 손상 없이 통과).
func (c *Client) decompressBody(df *DecodedFrame) []byte {
	if df.BodyZipf == ZipfNone || len(df.Body) == 0 {
		return df.Body
	}
	plain, err := Decompress(df.Body, df.BodyZipf)
	if err != nil {
		c.logBase().With(
			slog.Any(LogKeyError, err),
			slog.Int("zipf", int(df.BodyZipf)),
			slog.Int("body_len", len(df.Body)),
		).Warn("본문 압축 해제 실패 — raw 그대로 전달")
		return df.Body
	}
	return plain
}

// Call 은 요청을 보내고 ckey 로 매칭된 reply 를 기다린다.
//
// option-C 멀티플렉싱 경로: 다수의 Call() 동시 호출이 단일 Client
// 연결을 공유할 수 있다. ckey 는 자동 발급되며, FrameInput.Chan 이
// 비어있으면 Client 의 채널 코드(opts.Channel)가 자동으로 첨부된다.
func (c *Client) Call(ctx context.Context, in *FrameInput) (*Reply, error) {
	if c.closed.Load() {
		return nil, ErrClientClosed
	}
	if c.reconnecting.Load() {
		return nil, ErrReconnecting
	}
	if in.Ckey == 0 {
		in.Ckey = c.nextCorrelation()
	}
	c.applyDefaults(in)
	replyCh := make(chan *Reply, 1)
	c.pending.Store(in.Ckey, replyCh)

	frame, err := EncodeFrame(in)
	if err != nil {
		c.pending.Delete(in.Ckey)
		return nil, err
	}
	if err := c.writeFrame(frame); err != nil {
		c.pending.Delete(in.Ckey)
		return nil, err
	}

	select {
	case r := <-replyCh:
		return r, nil
	case <-ctx.Done():
		c.pending.Delete(in.Ckey)
		return nil, ctx.Err()
	case <-c.currentDoneCh():
		c.pending.Delete(in.Ckey)
		// 자동 재연결이 활성이면 호출자가 재시도할 수 있도록 ErrReconnecting 반환.
		if c.opts.Reconnect != nil && !c.closing.Load() {
			return nil, ErrReconnecting
		}
		if err := c.lastReadErr(); err != nil {
			return nil, err
		}
		return nil, ErrClientClosed
	}
}

// Send 는 응답을 기다리지 않고 프레임을 송신한다 (FC_NOTIFY / 단방향).
// FrameInput.Chan 이 비어있으면 Client 의 채널 코드가 자동 첨부된다.
func (c *Client) Send(in *FrameInput) error {
	c.applyDefaults(in)
	frame, err := EncodeFrame(in)
	if err != nil {
		return err
	}
	return c.writeFrame(frame)
}

// errBox 는 atomic.Pointer 와 함께 쓰기 위한 error wrapper.
// 다양한 concrete error 타입을 저장해도 atomic.Pointer 의 단일 타입 제약을
// 지킬 수 있다 (atomic.Value 와 달리).
type errBox struct{ err error }

// storeReadErr 는 readErr 에 에러를 기록한다 (nil 무시).
func (c *Client) storeReadErr(err error) {
	if err == nil {
		return
	}
	c.readErr.Store(&errBox{err: err})
}

// currentDoneCh 는 현재 활성 connection 의 doneCh 를 반환한다.
// supervisor 가 이 채널을 watch 해서 readLoop 종료를 감지한다.
func (c *Client) currentDoneCh() <-chan struct{} {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.doneCh
}

// lastReadErr 는 직전 readLoop 종료 사유를 반환한다 (없으면 nil).
func (c *Client) lastReadErr() error {
	v := c.readErr.Load()
	if v == nil {
		return nil
	}
	return v.err
}

// applyDefaults 는 FrameInput 에 클라이언트 단위 기본값을 채워넣는다.
// 명시적으로 설정된 필드는 덮어쓰지 않는다.
func (c *Client) applyDefaults(in *FrameInput) {
	// chan 필드가 모두 0 일 때만 client 의 채널 코드를 채워넣는다.
	var zero [4]byte
	if in.Chan == zero {
		in.Chan = c.chanCode
	}

	// Broadcast 계열 (FCCast/FCPush/FCSignal) 은 broker 의 packet_proc 가
	// nvia==0 인 경우에만 publish_packet (fan-out) 으로 분기한다. nvia!=0 이면
	// message_packet_transfer (1:1 transaction) 로 분기 후 dirf 가 0(IOCTL)
	// 이면 default 케이스에서 receiver scid 를 찾지 못해
	//   WRN Lost reply message 'XCHG@' for a receiver scid=0(sock=0)
	// 로 drop 된다. 따라서 navi 자동채움은 transaction 계열에만 적용하고,
	// broadcast 는 Dirf 만 DirPublish 로 보정한다.
	isBroadcast := in.Func == FCCast || in.Func == FCPush || in.Func == FCSignal

	if isBroadcast {
		if in.Dirf == 0 {
			in.Dirf = DirPublish
		}
		return
	}

	// Navigation 자동 채움 — C 라이브러리의 mymq_send 가
	// content_set_this(whoami) + content_set_dstn(xchg, rkey) 로 자동
	// 채우는 것과 동일한 동작.
	//
	// 호출자가 Navis 를 비워두고 Xchg/Rkey 만 채운 경우:
	//   navi[0] = origin (whoami): scid = client connection_id
	//   navi[1] = destination: xchg/rkey 만 셋
	// broker 는 navi[1] 을 보고 라우팅하고, reply 시 navi[0] 으로 회신.
	if len(in.Navis) == 0 && (in.Xchg != "" || in.Rkey != "") {
		var origin Navi
		origin.Scid = c.whoamiSc
		var dest Navi
		copy(dest.Xchg[:], in.Xchg)
		copy(dest.Rkey[:], in.Rkey)
		in.Navis = []Navi{origin, dest}
	}
}

// SendHeartbeat exposes the empty-frame heartbeat for callers that
// implement their own keepalive scheduling.
func (c *Client) SendHeartbeat() error {
	return c.writeHeartbeat()
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// BindService 는 선언된 큐에 (exchange, routing_key) 바인딩을 추가한다
// (FC_CNTL + BIND_SERVICE — C 의 mymq_bind_service 동등). broker 가 이후
// 해당 rkey 의 transaction 을 이 클라이언트 큐로 라우팅한다. rkey 는 후행
// 와일드카드 허용 ("W95*" / "W950?"). AP 역할 (transaction 수신 서버) 이
// Open 직후 호출한다. Queue 미선언 클라이언트에서는 broker 가 거부한다.
func (c *Client) BindService(ctx context.Context, exchange, rkey string) error {
	if rkey == "" || len(rkey) > LRkey {
		return fmt.Errorf("bind_service: rkey 길이 불량 %q", rkey)
	}
	if len(exchange) > 16 {
		return fmt.Errorf("bind_service: exchange 길이 불량 %q", exchange)
	}
	r, err := c.Call(ctx, &FrameInput{
		Func: FCCntl,
		Subc: SubBindService,
		Dirf: DirIoctl,
		Keyc: KeySend,
		Body: bindServiceBody(exchange, rkey),
	})
	if err != nil {
		return fmt.Errorf("bind_service(%s/%s): %w", exchange, rkey, err)
	}
	if r.Errn != 0 {
		return fmt.Errorf("bind_service(%s/%s): errn=%d %s", exchange, rkey, r.Errn, r.ErrMsg)
	}
	return nil
}
