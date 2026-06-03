// Package mymq 는 MyMQ 메시지 브로커용 Go 네이티브 클라이언트를 제공한다.
//
// Wire protocol 은 MyMQ C 소스의 include/mymq.h, src/inc/mq.h 를 그대로
// 옮긴 것이다. 모든 정수 필드는 big-endian (network byte order) 이며,
// TCP 위에서 length-prefixed framing 을 사용한다.
//
// WTG (Winway Trading Gateway) 의 모든 Go 서비스(mci-api / push / price /
// admin)가 이 패키지를 통해 mymqd 와 통신한다. cgo 의존성 없음.
package mymq

// 필드 길이 상수 (mymq.h 의 매크로).
const (
	LSymb = 20 // L_SYMB  - 심볼 이름 길이
	LName = 16 // L_NAME  - 멤버/큐 이름 길이
	LRkey = 16 // L_RKEY  - routing key 길이
	LXchg = 8  // L_XCHG  - exchange 이름 길이
	LSkey = 80 // L_SKEY  - 저장 키 (pkey/nkey) 최대 길이

	CookieSize = 256 // MyMQ_COOKIE - 서버측 쿠키 페이로드 크기
)

// Wire 프레임 크기 제한 (mq.h).
const (
	MaxMsgSize  = 1024*2048 + 256 // 단일 프레임 최대 크기 (~2MB)
	MaxUsolSize = 32*1024 + 256   // unsolicited 메시지 최대 크기 (32KB)
	CompSize    = 1024 * 2        // 압축 임계값 (2KB 이상이면 압축 시도)
	MaxVia      = 16              // navigation hop 최대 개수
	MaxPushLen  = 1512            // 실시간 push 메시지 본문 최대
)

// 구조체 wire 크기 (struct 레이아웃에서 계산).
const (
	HdrSize    = 100                              // mqhdr_t 고정 헤더 (84 + trace_id 16)
	NaviSize   = 32                               // navi_t 1개
	CookieWire = 16 + 12 + 24 + 20 + 20 + 4 + 256 // cookie_t (압축 안된 상태)

	// TraceIDSize — mqhdr.trcid[16] (W3C tracecontext 의 trace-id 16 byte).
	TraceIDSize = 16
)

// Func 는 메시지의 function code (mqhdr.func).
// MyMQ 의 가장 상위 라우팅 분류 — 트랜잭션인지, 브로드캐스트인지, 제어 명령인지 구분한다.
type Func uint8

const (
	FCCntl   Func = 1   // FC_CNTL   - 제어 명령 (CONNECT, DECLARE_*, BIND_SERVICE)
	FCDomain Func = 2   // FC_DOMAIN - 도메인 네트워크 (클러스터 간) 명령
	FCAdmin  Func = 3   // FC_ADMIN  - 관리 명령 (GET_STATUS, GET_CLIENT 등)
	FCCast   Func = 4   // FC_CAST   - 브로드캐스트 (다수 수신자)
	FCNotify Func = 5   // FC_NOTIFY - 응답 없는 단방향 통지
	FCTran   Func = 10  // FC_TRAN   - 트랜잭션 (request/reply 페어)
	FCFanout Func = 11  // FC_FANOUT - fanout (모든 바인드된 큐로)
	FCUnso   Func = 12  // FC_UNSO   - unsolicited (서버 측 능동 푸시)
	FCPush   Func = 13  // FC_PUSH   - 특정 사용자 푸시
	FCSignal Func = 14  // FC_SIGNAL - 시그널 (제어성 통지)
	FCBulk   Func = 15  // FC_BULK   - 대용량 파일 (FTP)
	FCRaw    Func = 100 // FC_RAW    - 자유 포맷
)

// Subc 는 sub-function code (mqhdr.subc).
// Func 와 결합해 메시지의 정확한 의도를 표현한다 (예: FC_CNTL+CONNECT, FC_TRAN+LOGON).
type Subc uint8

const (
	// 트랜잭션 sub-code (FC_TRAN 과 결합)
	SubTranMsg Subc = 0 // TRANMSG - 일반 트랜잭션
	SubTranErr Subc = 1 // TRANERR - 에러 응답
	SubSysErr  Subc = 2 // SYSERR  - 시스템 에러
	SubLogon   Subc = 3 // LOGON   - 로그인
	SubLogoff  Subc = 4 // LOGOFF  - 로그아웃

	// 대용량 메시지 sub-code (FC_BULK)
	SubBulkMsg Subc = 6 // BULKMSG - 대용량 페이로드
	SubBulkChn Subc = 7 // BULKCHN - 대용량 채널 메시지
	SubBulkFtp Subc = 8 // BULKFTP - FTP 전송

	// 제어 명령 sub-code (FC_CNTL)
	SubConnect         Subc = 10 // CONNECT          - mymq 오픈
	SubDeclareSession  Subc = 11 // DECLARE_SESSION  - 세션 선언 (핸드셰이크)
	SubDeclareExchange Subc = 12 // DECLARE_EXCHANGE - exchange 생성
	SubDeclareQueue    Subc = 13 // DECLARE_QUEUE    - queue 생성
	SubExportQueue     Subc = 14 // EXPORT_QUEUE     - 도메인에 export
	SubBindService     Subc = 15 // BIND_SERVICE     - 서비스 바인드
	SubGetClid         Subc = 16 // GET_CLID         - client id 조회

	// 캐스팅 sub-code (FC_CAST / FC_PUSH / FC_SIGNAL)
	SubBroadcast Subc = 50 // BROADCAST       - 모든 구독자에게
	SubUnicast   Subc = 51 // UNICAST         - 단일 수신자
	SubUnsolMsg  Subc = 52 // UNSOLICITED_MSG - 비요청 메시지
	SubSignal    Subc = 53 // SIGNAL          - 시그널
	SubPush      Subc = 54 // PUSH            - 실시간 푸시
	SubExit      Subc = 55 // EXIT            - 중복 종료
	SubKill      Subc = 56 // KILL            - 강제 종료

	// 도메인 sub-code (FC_DOMAIN)
	SubConnectDomain   Subc = 100 // CONNECT_DOMAIN
	SubAcceptDomain    Subc = 101 // ACCEPT_DOMAIN
	SubExportDomainSvc Subc = 102 // EXPORT_DOMAIN_SERVICE
	SubReportCpustat   Subc = 103 // REPORT_CPUSTAT

	// 관리 명령 sub-code (FC_ADMIN) — mci-admin 에서 주로 사용
	SubGetStatus      Subc = 150 // GET_STATUS         - broker 상태
	SubGetAppl        Subc = 151 // GET_APPL           - 애플리케이션 목록
	SubGetDomain      Subc = 152 // GET_DOMAIN         - 도메인 정보
	SubGetExchange    Subc = 153 // GET_EXCHANGE       - exchange 목록
	SubGetClient      Subc = 154 // GET_CLIENT         - 클라이언트 목록
	SubGetClientByPid Subc = 155 // GET_CLIENT_BY_PID  - PID 별 클라이언트
	SubGetWhois       Subc = 156 // GET_WHOIS          - 사용자 ID 검색
	SubGetLink        Subc = 157 // GET_LINK           - 클러스터 링크 세션
	SubGetNetSvc      Subc = 158 // GET_NETSVC         - 네트워크 서비스
	SubCtlNetSvc      Subc = 159 // CTL_NETSVC         - 네트워크 서비스 제어
	SubGetUsers       Subc = 160 // GET_USERS          - 채널 사용자

	// 기타 sub-code
	SubNotifyUpload Subc = 200 // NOTIFY_UPLOAD - 업로드 통지
	SubNotifyDnload Subc = 201 // NOTIFY_DNLOAD - 다운로드 통지
)

// Dirf 는 메시지 방향 플래그 (mqhdr.dirf).
// broker 가 multi-hop 라우팅 시 어느 단계인지를 표시한다.
type Dirf uint8

const (
	DirIoctl    Dirf = 0 // IOCTL    - 제어 명령
	DirForward  Dirf = 1 // FORWARD  - 정방향 (request)
	DirRelay    Dirf = 2 // RELAY    - 다음 hop 으로 중계
	DirBackward Dirf = 3 // BACKWARD - 역방향 (reply 진행 중)
	DirOrigin   Dirf = 4 // ORIGIN   - 발신자에게 reply 도달
	DirPublish  Dirf = 5 // PUBLISH  - 발행 (broadcast)
)

// Keyc 는 key action code (mqhdr.keyc).
// 페이지네이션이나 키 기반 탐색에 사용된다.
type Keyc uint8

const (
	KeySend Keyc = 'S' // K_SEND - 일반 송신 (대부분의 케이스)
	KeyPrev Keyc = 'P' // K_PREV - 이전 페이지 요청
	KeyNext Keyc = 'N' // K_NEXT - 다음 페이지 요청
)

// MsgF 플래그 (mqhdr.msgf 비트필드).
const (
	MsgfErr uint8 = 0x01 // 에러 메시지 포함
	MsgfFid uint8 = 0x02 // FID 형식 메시지
	MsgfHdr uint8 = 0x04 // common header 포함
	MsgfNwc uint8 = 0x08 // 신규 윈도우 생성 요청
	MsgfEnc uint8 = 0x10 // 암호화된 본문
	MsgfCon uint8 = 0x20 // continuous 프레임 (분할 송신)
	MsgfEnd uint8 = 0x40 // continuous 의 마지막 조각
	MsgfCer uint8 = 0x80 // 인증 정보 포함
)

// CtlF 플래그 (mqhdr.ctlf 비트필드).
const (
	CtlfNoc uint8 = 0x01 // CTLF_NOC - 압축하지 말 것
)

// WITH_* 콘텐츠 플래그 (content.with 비트필드, uint32).
// content 송수신 시 부가 동작을 지정한다.
const (
	WithCookie        uint32 = 0x00001 // cookie 첨부
	WithSymbol        uint32 = 0x00002 // 실시간 심볼 갱신 요청
	WithCompressed    uint32 = 0x00004 // 본문이 압축됨
	WithError         uint32 = 0x00008 // 에러 응답
	WithDirect        uint32 = 0x00010 // 직접 라우팅 (internal)
	WithFid           uint32 = 0x00020 // FID 형식
	WithReset         uint32 = 0x00040 // content 리셋 요청
	WithNewWin        uint32 = 0x00080 // 신규 window 생성
	WithEncrypt       uint32 = 0x00100 // 암호화 송신
	WithContinuous    uint32 = 0x00200 // 분할 송신 진행 중
	WithEnd           uint32 = 0x00400 // 분할 송신 마지막
	WithCertification uint32 = 0x00800 // 인증 정보 동봉
	WithCommonHdr     uint32 = 0x01000 // common header 동봉
	WithPrevKey       uint32 = 0x02000 // pkey 동봉
	WithNextKey       uint32 = 0x04000 // nkey 동봉
	WithNoCompress    uint32 = 0x08000 // 압축 비활성
	WithFirst         uint32 = 0x10000 // 첫 메시지
	WithBulkMsg       uint32 = 0x20000 // bulk 응답
	WithBulkStored    uint32 = 0x40000 // bulk 저장 완료
)

// Zipf 는 본문 압축 방식 (content.zipf).
// MyMQ 는 압축 임계값(2KB) 이상일 때만 자동 압축한다.
type Zipf uint8

const (
	ZipfNone Zipf = 0 // 압축 없음
	ZipfMlzo Zipf = 1 // mini-LZO
	ZipfZlib Zipf = 2 // zlib (가장 호환성 좋음)
	ZipfLziv Zipf = 3 // Lempel-Ziv
)

// ExchangeType 은 exchange 라우팅 모델 (mymq_declare_exchange).
type ExchangeType uint8

const (
	ExchangeDirect ExchangeType = 0 // ET_DIRECT - routing key 정확 매칭
	ExchangeTopic  ExchangeType = 1 // ET_TOPIC  - 패턴 매칭
	ExchangeFanout ExchangeType = 2 // ET_FANOUT - 모든 바인드된 큐로
)

// QueueType 은 큐 속성 (mymq_declare_queue).
type QueueType uint8

const (
	QtMask    QueueType = 0x0f
	QtPrivate QueueType = 1    // QT_PRIVATE - 단일 소유자
	QtShared  QueueType = 2    // QT_SHARED  - 공유 (여러 소비자)
	QtPublic  QueueType = 3    // QT_PUBLIC  - 공개 (외부 발견 가능)
	QtClient  QueueType = 4    // QT_CLIENT  - 클라이언트 전용
	QtExport  QueueType = 0x10 // QT_EXPORT  - 도메인 export 모드
)

// Queue 플래그 (instance.qu_flag).
// unsolicited (broker 가 능동 push) 수신 여부 결정.
const (
	QfUnsolMsg uint32 = 0x01 // 비요청 메시지 수신 가능
	QfUnsolHdr uint32 = 0x02 // 본문 앞에 broadcast prefix 동봉
	QfUnsolRep uint32 = 0x04 // 그룹 대표 (한 노드만 수신)
)

// 라우팅 방식 (s_parms.ap2ap, ap2br).
// WTG Go 클라이언트는 BROKER 만 사용 (네트워크 분리 환경).
const (
	RoutingBroker  = 0 // BROKER  - TCP 통한 broker (Go 클라이언트 기본)
	RoutingSysMsgQ = 1 // SYSMSGQ - SysV 메시지 큐 (로컬 IPC)
	RoutingTrxQ    = 2 // MYMQTXQ - 자체 트랜잭션 큐
	RoutingShmQ    = 3 // MYMQSHM - shared memory 큐
	RoutingIpcSock = 4 // IPCSOCK - Unix domain socket
)

// 브로드캐스트 라우팅 방식 (s_parms.bcast).
const (
	BcastInternal   = 0 // BCAST_INTERNAL   - broker 자체 라우팅
	BcastMulticast  = 1 // BCAST_MULTICAST  - UDP 멀티캐스트
	BcastIndividual = 2 // BCAST_INDIVIDUAL - whois 조회 후 개별 unicast
)

// Member 타입 — clid (client id) 의 비트 인코딩.
// 레이아웃: type:2 bits + ncid:5 bits + scid:24 bits = 31 bits 사용.
//
// 원본 C 매크로 (mq.h) 는 ncid 를 0x3f(6 bits) 로 마스크하지만, bit 29 가
// type 의 하위 비트와 겹쳐서 type 이 1(DMS) 이거나 ncid >= 32 일 때 round-trip
// 손실이 발생한다. WTG 에서는 ncid 를 5 bits 로 제한해서 손실 없이 인코딩/
// 디코딩하도록 했다. 실무 영향은 없음 — DMS/NET 클러스터는 32호스트 미만이
// 일반적이다.
const (
	MemberLoc = 0 // _LOC_ - 로컬
	MemberDms = 1 // _DMS_ - 도메인 서비스 서버
	MemberNet = 2 // _NET_ - 네트워크 서버

	clidTypeShift = 29
	clidNcidShift = 24
	clidTypeMask  = 0x03
	clidNcidMask  = 0x1f       // 5 bits (타입과 겹침 방지)
	clidScidMask  = 0x00FFFFFF // 24 bits (session connection id)
)

// MakeClid 는 (type 2bit, ncid 5bit, scid 24bit) 를 32비트 client id 로 묶는다.
// C 의 MEMBER_CCID 매크로와 동일한 wire 레이아웃을 만든다 (입력값이 비트 폭을
// 초과하면 자동 마스크).
func MakeClid(memberType, ncid, scid uint32) uint32 {
	return ((memberType & clidTypeMask) << clidTypeShift) |
		((ncid & clidNcidMask) << clidNcidShift) |
		(scid & clidScidMask)
}

// SplitClid 는 packed client id 로부터 (type, ncid, scid) 를 추출한다.
func SplitClid(clid uint32) (memberType, ncid, scid uint32) {
	memberType = (clid >> clidTypeShift) & clidTypeMask
	ncid = (clid >> clidNcidShift) & clidNcidMask
	scid = clid & clidScidMask
	return
}

// MyMQ 에러 코드 (MEBASE..MESVCABORTED).
// content.errn 또는 mqhdr.errn 으로 전달된다.
const (
	ErrSystem        = 1000 // MESYSTEM      - 시스템 에러
	ErrBroker        = 1001 // MEBROKER      - broker 끊김
	ErrNoReceiver    = 1002 // MENORCVER     - 수신자 없음
	ErrNoOrigin      = 1003 // MENOORGN      - 발신자 정보 없음
	ErrNoDestination = 1004 // MENODSTN      - 목적지 없음
	ErrQueueName     = 1005 // MEQUENAME     - 잘못된 큐 이름
	ErrQueueAttr     = 1006 // MEQUEATTR     - 잘못된 큐 속성
	ErrNoQueue       = 1007 // MENOQUEUE     - 큐 미선언
	ErrQueueBusy     = 1008 // MEQUEBUSY     - 큐 사용 중
	ErrNoBound       = 1009 // MENOBOUND     - 바인드된 서비스 없음
	ErrBadArg        = 1010 // MEBADARG      - 잘못된 인자
	ErrBadFunc       = 1011 // MEBADFUNC     - 잘못된 함수 호출
	ErrBadPath       = 1012 // MEBADPATH     - 잘못된 라우팅 경로
	ErrTooBig        = 1013 // METOOBIG      - 메시지 너무 큼
	ErrTooShort      = 1014 // METOOSHORT    - 메시지 너무 짧음
	ErrNoMsg         = 1015 // MENOMSG       - 가용 메시지 없음
	ErrMsgForm       = 1016 // MEMSGFORM     - 잘못된 메시지 포맷
	ErrBadAddr       = 1017 // MEBADADDR     - 잘못된 호스트 주소
	ErrBusy          = 1018 // MEBUSY        - 사용 중
	ErrExist         = 1019 // MEEXIST       - 이미 존재
	ErrAuth          = 1020 // MEAUTH        - 인증 실패
	ErrTimeout       = 1021 // METIMEOUT     - 타임아웃
	ErrMsgIo         = 1022 // MEMSGIO       - I/O 에러
	ErrResource      = 1023 // MERESOURCE    - 리소스 부족
	ErrFunc          = 1024 // MEFUNC        - 함수 호출 순서 오류
	ErrConnRefused   = 1025 // MECONNREFUSED - 연결 거부
	ErrSvcBusy       = 1026 // MESVCBUSY     - 서비스 사용 중
	ErrNoSvc         = 1027 // MENOSVC       - 알 수 없는 트랜잭션 코드
	ErrForkPro       = 1028 // MEFORKPRO     - 서비스 fork 실패
	ErrSvcTimeout    = 1029 // MESVCTIMEOUT  - 서비스 타임아웃
	ErrSvcAborted    = 1030 // MESVCABORTED  - 서비스 중단됨
)

// Cookie 는 단말 사용자 식별 정보 (cookie_t in mymq.h).
// 로그인 후 모든 트랜잭션에 첨부되어 인증/감사용으로 사용된다.
type Cookie struct {
	Usid [16]byte         // 사용자 ID
	Name [12]byte         // 사용자 이름
	Maca [24]byte         // MAC 주소
	Pcip [20]byte         // 클라이언트 IP
	Svip [20]byte         // 서버 IP
	Clid uint32           // 클라이언트 ID
	Coki [CookieSize]byte // 서버측 쿠키 페이로드 (불투명)
}

// Navi 는 라우팅 엔트리 (navi_t in mq.h, wire 상 32 bytes).
// 메시지가 broker 를 통해 multi-hop 라우팅 시 거치는 노드들의 정보.
type Navi struct {
	Xchg [LXchg]byte // 8 bytes - exchange 이름
	Rkey [LRkey]byte // 16 bytes - routing key
	Scid uint32      // 4 bytes BE - session connection id
	Iama uint8       // 1 byte - 노드 역할 (DMS/NET/AGENT)
	Eatt uint8       // 1 byte - exchange 타입
	Zipf uint8       // 1 byte - 수신 가능한 압축 방식
	Ncid uint8       // 1 byte - 클러스터 호스트 ID
}

// Header 는 MyMQ 프레임 고정 헤더 (mqhdr_t, wire 상 84 bytes).
// 모든 정수 필드는 big-endian 으로 직렬화된다.
type Header struct {
	Length uint32      // 전체 프레임 길이
	Func   Func        // function code
	Subc   Subc        // sub-function code
	Nvia   uint8       // navi 엔트리 개수
	Dirf   Dirf        // 메시지 방향
	Msgf   uint8       // 메시지 플래그
	Ctlf   uint8       // 제어 플래그
	Keyc   Keyc        // key action
	Xchg   [LXchg]byte // 목적지 exchange
	Rkey   [LRkey]byte // 목적지 routing key
	Ckey   uint32      // content key-id (correlation_id 로 활용)
	Clid   uint32      // client id (type+ncid+scid 인코딩)
	Wkey   [8]byte     // window key-id (UI window 식별자)
	Chan   [4]byte     // origin channel type
	Errn   uint32      // 에러 코드

	// WHERE/SZOFF — 가변 영역 안의 데이터 위치 인덱스
	CoffZipf uint8  // cookie 압축 방식
	CoffOff  uint32 // cookie 오프셋 (24-bit 바이트 오프셋)
	SoffZipf uint8  // symbol 압축 방식
	SoffOff  uint32 // symbol 오프셋
	ErrmLen  uint16 // 에러 메시지 길이
	ErrmOff  uint16 // 에러 메시지 오프셋
	PkeyLen  uint16 // pkey 길이
	PkeyOff  uint16 // pkey 오프셋
	NkeyLen  uint16 // nkey 길이
	NkeyOff  uint16 // nkey 오프셋
	BodyZipf uint8  // 본문 압축 방식
	BodyOff  uint32 // 본문 오프셋

	// TraceID — mqhdr.trcid[16]. W3C tracecontext 의 trace-id (16 byte).
	// broker (mymqd) 가 echo/passthrough — 라우팅에 사용 X. WTG 측이 set/get.
	// 모두 0 = 미설정 (DecodeHeader 는 그래도 zero array 채움).
	TraceID [TraceIDSize]byte
}
