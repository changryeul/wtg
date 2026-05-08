package mymq

// declareSessionWire 는 mqio.h 의 struct declare_session 와 동일한 wire 레이아웃.
// 모든 다바이트 정수는 big-endian 으로 직렬화된다.
//
//	appl_name         [16] - 클라이언트가 채움
//	user              [16] - 클라이언트가 채움 (보통 getpwuid 결과)
//	pid               [4]  - 클라이언트가 채움 (BE)
//	ipad              [20] - 외부 클라이언트 IP, 옵션
//	port              [4]  - 외부 클라이언트 포트, 옵션 (BE)
//	socket_id         [4]  - 서버가 채움
//	session_id        [4]  - 서버가 채움
//	connection_id     [4]  - 서버가 채움 (= scid)
//	how_to_routing    [2]  - 서버: ap2ap[0], ap2br[1]
//	how_to_broadcast  [1]  - 서버: 브로드캐스트 방식
//	log_suffix        [1]  - 서버
//	heartbeat         [4]  - 서버 (BE, 초)
//	compress_method   [4]  - 서버 (BE)
//	compress_size     [4]  - 서버 (BE)
//
// 합계 88 bytes.
const declareSessionSize = 16 + 16 + 4 + 20 + 4 + 4 + 4 + 4 + 2 + 1 + 1 + 4 + 4 + 4

// DeclareSessionRequest 는 클라이언트가 채워서 보내는 핸드셰이크 요청.
type DeclareSessionRequest struct {
	ApplName     string // 최대 16바이트 — broker 에 등록되는 애플리케이션 이름
	User         string // 최대 16바이트 — 사용자 ID (getpwuid 결과 권장)
	Pid          uint32
	ExternalIP   string // 최대 20바이트 — 외부 클라이언트 IP (게이트웨이 사용 시)
	ExternalPort uint32
}

// DeclareSessionResponse 는 broker 가 응답으로 채워주는 세션 파라미터.
type DeclareSessionResponse struct {
	SocketID       uint32
	SessionID      uint32
	ConnectionID   uint32  // = scid (whoami.scid 채울 때 사용)
	HowToRouting   [2]byte // [0]=ap2ap, [1]=ap2br
	HowToBroadcast uint8
	LogSuffix      uint8
	Heartbeat      uint32 // 초 단위. 자동 heartbeat 루프의 주기로 사용됨.
	CompressMethod uint32
	CompressSize   uint32
}

// EncodeDeclareSessionRequest 는 요청 부분을 직렬화한다.
// 응답 영역(서버가 채울 자리)은 0 으로 남긴다.
func EncodeDeclareSessionRequest(req *DeclareSessionRequest) []byte {
	buf := make([]byte, declareSessionSize)
	off := 0
	padCopyString(buf[off:off+16], req.ApplName)
	off += 16
	padCopyString(buf[off:off+16], req.User)
	off += 16
	putU32(buf[off:off+4], req.Pid)
	off += 4
	padCopyString(buf[off:off+20], req.ExternalIP)
	off += 20
	putU32(buf[off:off+4], req.ExternalPort)
	// 나머지 응답 영역은 서버가 채워서 회신한다 — 0 그대로 둠.
	return buf
}

// DecodeDeclareSessionResponse 는 서버 응답에서 파라미터를 추출한다.
// broker 는 클라이언트가 보낸 88바이트 구조체를 그대로 echo back 하면서
// 자기 영역만 채워서 회신하므로, 입력은 88바이트 또는 그 이상이어야 한다.
func DecodeDeclareSessionResponse(buf []byte) (DeclareSessionResponse, error) {
	var r DeclareSessionResponse
	if len(buf) < declareSessionSize {
		return r, ErrFrameTooShort
	}
	off := 16 + 16 + 4 + 20 + 4 // 클라이언트가 보낸 영역은 건너뛴다
	r.SocketID = getU32(buf[off : off+4])
	off += 4
	r.SessionID = getU32(buf[off : off+4])
	off += 4
	r.ConnectionID = getU32(buf[off : off+4])
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
