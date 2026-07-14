package mymq

// CONNECT 핸드셰이크 (FC_CNTL + SubConnect, mymq_openx 대응).
//
// DECLARE_SESSION 보다 풍부한 단일-shot 핸드셰이크로, 다음을 한 번에 처리한다:
//  1. 세션 등록 (DECLARE_SESSION 과 동일한 메타)
//  2. exchange 선언 (옵션)
//  3. queue 선언 (필수 — unsolicited 수신용 큐)
//  4. queue export (도메인 단위, 옵션)
//  5. unsolicited 수신 등록 (qu_flag 의 QF_UNSOL_MSG 등)
//
// 비즈니스적으로:
//   - mci-api  → DECLARE_SESSION 으로 충분 (request/reply 만)
//   - mci-push, mci-price, mci-admin → CONNECT 필요 (unsolicited 구독)
//
// wire 레이아웃:
//
//	instance 영역 (192 bytes) — 클라이언트가 채움
//	  mq_host[24] + mq_port[4] + mq_user[16] + my_pid[4] +
//	  my_name[16] + ch_name[16] + ch_ipad[20] + ch_port[4] +
//	  ex_name[16] + ex_type[4] + qu_name[16] + qu_flag[4] +
//	  qu_attr[4] + qu_size[4] + qu_expt[40]
//
//	connection 응답 영역 (56 bytes) — broker 가 채움
//	  socket_id[4] + connection_id[4] + queue_name[16] + queue_key[4] +
//	  queue_id[4] + queue_msg_id[4] + queue_size[4] + how_to_routing[2] +
//	  how_to_broadcast[1] + log_suffix[1] + heartbeat[4] +
//	  compress_method[4] + compress_size[4]
//
//	합계: 248 bytes
const (
	instanceSize   = 24 + 4 + 16 + 4 + 16 + 16 + 20 + 4 + 16 + 4 + 16 + 4 + 4 + 4 + 40
	connectionSize = instanceSize + 4 + 4 + 16 + 4 + 4 + 4 + 4 + 2 + 1 + 1 + 4 + 4 + 4
)

// QueueOptions 는 CONNECT 시 broker 에 등록할 큐/exchange 선언 정보.
//
// Options.Queue 가 nil 이 아니면 CONNECT 핸드셰이크를 사용한다.
// nil 이면 DECLARE_SESSION 만 보낸다.
type QueueOptions struct {
	// Name 은 큐 이름. broker 가 이 이름의 큐를 자동 생성/매핑한다.
	// 빈 값이면 broker 가 자동 작명.
	Name string

	// Attr 는 큐 속성 (QtClient/QtPublic/QtShared/QtPrivate).
	// QtClient: 클라이언트 전용 큐 (mci-api 가 reply 받을 때).
	// QtPublic|QtShared: 공유 구독 큐 (mci-push / mci-price).
	Attr QueueType

	// Size 는 큐 크기 (KB). 0 이면 broker 기본값.
	Size uint32

	// Flags 는 unsolicited 수신 옵션 (QfUnsolMsg | QfUnsolHdr | QfUnsolRep).
	// mci-push/price 는 보통 (QfUnsolMsg | QfUnsolHdr) 로 설정.
	Flags uint32

	// ExchangeName 은 함께 선언할 exchange (옵션).
	// 비어있으면 exchange 선언 생략 (이미 broker 측에 정의되어 있다고 가정).
	ExchangeName string

	// ExchangeType 은 ExchangeName 비어있지 않을 때만 사용
	// (ExchangeDirect / ExchangeTopic / ExchangeFanout).
	ExchangeType ExchangeType

	// ExportDomain 은 큐를 다른 도메인으로 export 할 때만 (옵션, 최대 40바이트).
	ExportDomain string
}

// connectRequest 는 CONNECT 의 instance 영역에 들어가는 클라이언트 측 입력.
type connectRequest struct {
	MqHost string
	MqPort uint32
	MqUser string
	Pid    uint32
	MyName string
	ChName string
	ChIpad string
	ChPort uint32
	ExName string
	ExType uint32
	QuName string
	QuFlag uint32
	QuAttr uint32
	QuSize uint32
	QuExpt string
}

// ConnectResponse 는 CONNECT 응답 안의 broker 결과 영역.
type ConnectResponse struct {
	SocketID       uint32
	ConnectionID   uint32 // = scid (whoami)
	QueueName      string // broker 가 부여한 최종 큐 이름
	QueueKey       uint32
	QueueID        uint32
	QueueMsgID     uint32
	QueueSize      uint32
	HowToRouting   [2]byte // [0]=ap2ap, [1]=ap2br
	HowToBroadcast uint8
	LogSuffix      uint8
	Heartbeat      uint32
	CompressMethod uint32
	CompressSize   uint32
}

// encodeConnectRequest 는 connection 구조 (248 bytes) 를 직렬화한다.
// 응답 영역(56 bytes) 은 0 으로 두며, broker 가 채워서 회신한다.
func encodeConnectRequest(req *connectRequest) []byte {
	buf := make([]byte, connectionSize)
	off := 0

	padCopyString(buf[off:off+24], req.MqHost)
	off += 24
	putU32(buf[off:off+4], req.MqPort)
	off += 4
	padCopyString(buf[off:off+16], req.MqUser)
	off += 16
	putU32(buf[off:off+4], req.Pid)
	off += 4
	padCopyString(buf[off:off+16], req.MyName)
	off += 16
	padCopyString(buf[off:off+16], req.ChName)
	off += 16
	padCopyString(buf[off:off+20], req.ChIpad)
	off += 20
	putU32(buf[off:off+4], req.ChPort)
	off += 4
	padCopyString(buf[off:off+16], req.ExName)
	off += 16
	putU32(buf[off:off+4], req.ExType)
	off += 4
	padCopyString(buf[off:off+16], req.QuName)
	off += 16
	putU32(buf[off:off+4], req.QuFlag)
	off += 4
	putU32(buf[off:off+4], req.QuAttr)
	off += 4
	putU32(buf[off:off+4], req.QuSize)
	off += 4
	padCopyString(buf[off:off+40], req.QuExpt)
	// off += 40 → instanceSize (192)

	// 나머지 56바이트는 0 그대로 — broker 가 채움.
	return buf
}

// decodeConnectResponse 는 broker 응답에서 결과 영역을 추출한다.
// 입력 buf 는 248 바이트 이상이어야 한다 (instance echo + 응답).
func decodeConnectResponse(buf []byte) (ConnectResponse, error) {
	var r ConnectResponse
	if len(buf) < connectionSize {
		return r, ErrFrameTooShort
	}
	off := instanceSize // 클라이언트가 채운 영역은 건너뛴다

	r.SocketID = getU32(buf[off : off+4])
	off += 4
	r.ConnectionID = getU32(buf[off : off+4])
	off += 4
	r.QueueName = trimNulString(buf[off : off+16])
	off += 16
	r.QueueKey = getU32(buf[off : off+4])
	off += 4
	r.QueueID = getU32(buf[off : off+4])
	off += 4
	r.QueueMsgID = getU32(buf[off : off+4])
	off += 4
	r.QueueSize = getU32(buf[off : off+4])
	off += 4
	r.HowToRouting[0] = buf[off]
	r.HowToRouting[1] = buf[off+1]
	off += 2
	r.HowToBroadcast = buf[off]
	off++
	r.LogSuffix = buf[off]
	off++
	r.Heartbeat = getU32(buf[off : off+4])
	off += 4
	r.CompressMethod = getU32(buf[off : off+4])
	off += 4
	r.CompressSize = getU32(buf[off : off+4])
	return r, nil
}

// bindServiceBody 는 FC_CNTL+BIND_SERVICE 의 본문 (mqio.h struct bind_service —
// exchange_name[16] + routing_key[16], NUL 패딩 고정폭 32B) 을 만든다.
func bindServiceBody(exchange, rkey string) []byte {
	body := make([]byte, 32)
	copy(body[0:16], exchange)
	copy(body[16:32], rkey)
	return body
}
