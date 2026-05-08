# MCI Phase 0 — MyMQ 분석 보고서

분석 대상: `/Users/winwaysystems/mywork/mymq` (MyMQ — C 기반 분산 메시지 브로커)
분석 일자: 2026-05-02
작성자: Phase 0 (libmymq-go 설계 기반자료)

---

## 1. 핵심 결론 (TL;DR)

| 결정 사항               | 결론                                                                     |
| ------------------- | ---------------------------------------------------------------------- |
| 통합 방식               | **Go-native MyMQ 클라이언트 (libmymq-go) 신규 구현** (cgo 회피)                   |
| Wire protocol       | **명확하게 문서화 가능** — mqhdr_t 84바이트 + navi[] + 가변영역                        |
| Endianness          | **Big-endian (network byte order)**                                    |
| Framing             | **Length-prefixed (4 bytes BE)** 단일 TCP 스트림                            |
| 인증                  | mymq_logon은 stub. 실제는 cookie 등록 + DECLARE_SESSION 핸드셰이크                |
| Pub/Sub             | 별도 subscribe API 없음. **qu_flag = QF_UNSOL_MSG로 전부 수신**                 |
| Request/Reply 멀티플렉싱 | **`mqhdr.ckey` (4바이트)를 correlation_id로 활용** — C 엔진 무수정 + 단일 연결 멀티플렉싱   |
| 실시간 시세 (RQ)         | **옵션 A-1 확정** — Cooker가 myrqd + mymqd 양쪽 publish, mci-price는 broker 경유 |

## 2. MyMQ 시스템 개관

### 2.1 데몬 구성

| 데몬                    | 역할                                             |
| --------------------- | ---------------------------------------------- |
| `mymqd`               | TCP 메시지 브로커 (포트: `mymqd/tcp` 또는 기본값)           |
| `myrqd`               | 실시간 시세 push 데몬 (shared memory + sysv mq + tcp) |
| `mymqappd`            | 프로세스 관리/모니터링                                   |
| `mymqinit`/`mymqboot` | mymq.tab 기반 부트스트랩                              |
| `mymqpubd`            | UDP 기반 클러스터 broadcast                          |
| `mymqslbd`            | 로드 밸런서                                         |
| `mymqftpd`            | bulk file transfer                             |
| `mymqlink`            | 도메인 클러스터링                                      |

### 2.2 채널 어댑터 (MCA, src/mca/)

| 어댑터         | 외부 프로토콜                     |
| ----------- | --------------------------- |
| `presto`    | HTS 트레이딩 시스템 프로토콜           |
| `vivace`    | 향상된 트레이딩 인터페이스              |
| `znet`      | 레거시 Z-network               |
| `anyone`    | HTTP REST 스타일 (mci-api 템플릿) |
| `websocket` | WebSocket + JSON            |
| `oneshot`   | 단일 요청-응답                    |
| `template`  | 신규 어댑터 베이스                  |

→ **mci-api는 anyone, mci-push는 websocket 어댑터를 Go로 재구현하는 셈**.
   단, MCA framework 자체는 새로 만들지 않고 Go의 net/http + WebSocket 라이브러리로 대체.

## 3. Wire Protocol 명세 (Go 구현용)

### 3.1 TCP Framing

```
[length:4 BE][frame body...]
```
- 4바이트 길이 (전체 프레임 크기, 자기 포함)
- length=4 → heartbeat (본문 없음)
- 최대 프레임: 2MB+256 (`MAX_MSG_SIZE`)

### 3.2 Frame 구조 — mqhdr_t (84 bytes)

```
Offset  Size  Field           설명
  0      4    msgl            전체 프레임 길이 (BE uint32)
  4      1    func            FC_TRAN(10)/FC_PUSH(13)/FC_CAST(4)/...
  5      1    subc            sub-function code
  6      1    nvia            navigation entry 개수 (max 16)
  7      1    dirf            FORWARD(1)/RELAY(2)/BACKWARD(3)/ORIGIN(4)/PUBLISH(5)
  8      1    msgf            ERR/FID/HDR/NWC/ENC/CON/END/CER 비트
  9      1    ctlf            NOC=0x01 (no compress)
 10      1    xxxx            reserved
 11      1    keyc            'S'=Send / 'P'=Prev / 'N'=Next
 12      8    xchg[8]         exchange name (null-padded)
 20     16    rkey[16]        routing key (null-padded)
 36      4    ckey            content key-id (BE uint32)
 40      4    clid            client id (BE uint32) — type:3 + ncid:5 + scid:24
 44      8    wkey[8]         window key-id (raw bytes)
 52      4    chan[4]         origin channel type
 56      4    errn            error number (BE uint32)
 60      4    coff            cookie WHERE: zipf:1 + doff:3 (24-bit byte offset)
 64      4    soff            symbol WHERE
 68      4    errm            SZOFF: len:2 + off:2 (16-bit)
 72      4    pkey            previous key SZOFF
 76      4    nkey            next key SZOFF
 80      4    body            body WHERE (zipf:1 + doff:3)
```

### 3.3 가변 영역 (mqhdr 이후)

```
Offset 84  navi_t[nvia]    32 bytes × nvia    네비게이션 엔트리
       ?   ERRM            errm.len bytes      에러 메시지
       ?   PKEY            pkey.len bytes      이전 키 (max 80)
       ?   NKEY            nkey.len bytes      다음 키 (max 80)
       ?   COOKIE          cookie_t (압축가능) coff.zipf 참조
       ?   SYMB            count:4 + symbol_t[]  실시간 심볼
       ?   BODY            body.doff부터 끝까지  메시지 본문 (압축가능)
```

### 3.4 navi_t (32 bytes)

```c
struct navigate {
    char    xchg[8];
    char    rkey[16];
    uint8_t scid[4];   // BE uint32 — session connection id
    uint8_t iama;      // role (DMS=1/NET=2/AGENT=0x10)
    uint8_t eatt;      // exchange type
    uint8_t zipf;      // receivable compress method
    uint8_t ncid;      // network id (cluster host)
};
```

### 3.5 cookie_t (~352 bytes, 압축 가능)

```c
struct cookie {
    char usid[16];       // user id
    char name[12];       // user name
    char maca[24];       // mac address
    char pcip[20];       // client ip
    char svip[20];       // server ip
    int  clid;           // BE uint32 (직렬화 시)
    char coki[256];      // server-side cookie blob
};
```

### 3.6 핸드셰이크 (DECLARE_SESSION) — 88 bytes

```c
struct declare_session {
    char    appl_name[16];     // 클라이언트가 채움
    char    user[16];          // 클라이언트가 채움
    uint8_t pid[4];            // BE uint32
    char    ipad[20];          // external client IP
    uint8_t port[4];           // external port
    uint8_t socket_id[4];      // ← 서버가 응답으로 채움
    uint8_t session_id[4];     // ← 서버
    uint8_t connection_id[4];  // ← 서버 (= scid)
    uint8_t how_to_routing[2]; // ← 서버
    uint8_t how_to_broadcast[1];
    uint8_t log_suffix[1];
    uint8_t heartbeat[4];      // ← 서버가 결정
    uint8_t compress_method[4];
    uint8_t compress_size[4];
};
```

요청은 `FC_CNTL` + `DECLARE_SESSION(11)` 으로 보내고, 응답은 동일 함수코드로 옴 (msgf에 ERR 비트 없음 = 성공).

### 3.7 broadcast prefix (FC_CAST/FC_PUSH 본문 앞 80 bytes)

```c
struct broadcast {
    char ipaddr[24];     // target host (or empty = all)
    char exchange[16];
    char chan[16];       // channel name
    char user[16];
    char logon_id[16];   // 특정 사용자 (개별 push 시)
    char function;       // FC_CAST/PUSH/SIGNAL
    char sub_function;   // BROADCAST(50)/UNICAST/PUSH/KILL/EXIT
    char via_net;        // 1 = remote 출처
    char debug;
};
```
이 80바이트가 본문 앞에 붙음 (QF_UNSOL_HDR=true 시에만 클라이언트에 노출됨).

### 3.8 압축

- 알고리즘: NONE(0) / MLZO(1) / ZLIB(2) / LZIV(3)
- 본문 ≥ COMP_SIZE(2KB) && zipf > 0 시 적용
- 압축 본문 prefix: `[orig_size:4 BE][compressed_data]`
- 수신측 zipf 모르는 경우 raw passthrough (AGENT 역할)
- ⚠️ **mq_send.c:220 의 `size >> 27` 코드는 `>> 24` 가 맞아 보임** — 압축 시 정밀 검증 필요

## 4. 함수코드 / 서브코드 카탈로그

### 4.1 FUNC (mqhdr.func, content.func)

| 값 | 이름 | 설명 |
|----|------|-----|
| 1 | FC_CNTL | 제어 명령 (CONNECT/DECLARE_*/BIND_SERVICE) |
| 2 | FC_DOMAIN | 도메인 네트워크 명령 |
| 3 | FC_ADMIN | 관리 명령 (GET_STATUS/CLIENT/...) |
| 4 | FC_CAST | broadcast |
| 5 | FC_NOTIFY | notify (응답 없는 트랜) |
| 10 | FC_TRAN | 트랜잭션 (request/reply) |
| 11 | FC_FANOUT | fanout |
| 12 | FC_UNSO | unsolicited |
| 13 | FC_PUSH | push |
| 14 | FC_SIGNAL | signal |
| 15 | FC_BULK | ftp/bulk |
| 100 | FC_RAW | free format |

### 4.2 SUBC (자주 쓰는 것만)

| 값 | 이름 | 설명 |
|----|------|-----|
| 0 | TRANMSG | 일반 트랜잭션 |
| 1 | TRANERR | 에러 응답 |
| 2 | SYSERR | 시스템 에러 |
| 3 | LOGON | 로그인 |
| 4 | LOGOFF | 로그아웃 |
| 10 | CONNECT | mymq 오픈 |
| 11 | DECLARE_SESSION | 세션 선언 |
| 12 | DECLARE_EXCHANGE | exchange 생성 |
| 13 | DECLARE_QUEUE | queue 생성 |
| 15 | BIND_SERVICE | 서비스 바인드 |
| 50 | BROADCAST | 브로드캐스트 |
| 51 | UNICAST | 유니캐스트 |
| 53 | SIGNAL | 시그널 |
| 54 | PUSH | push 메시지 |
| 55 | EXIT | 중복 종료 |
| 56 | KILL | 강제 종료 |
| 150 | GET_STATUS | 상태 조회 |
| 151 | GET_APPL | 앱 정보 |

### 4.3 에러 코드 (1000~1030)

```
1000 MESYSTEM        시스템 에러
1001 MEBROKER        broker 끊김
1002 MENORCVER       receiver 없음
1003 MENOORGN        origin 없음
1004 MENODSTN        destination 없음
1010 MEBADARG        잘못된 인자
1011 MEBADFUNC       잘못된 함수 호출
1012 MEBADPATH       잘못된 라우팅 경로
1013 METOOBIG        메시지 너무 큼
1014 METOOSHORT      메시지 너무 짧음
1019 MEEXIST         이미 존재
1020 MEAUTH          인증 실패
1021 METIMEOUT       타임아웃
1022 MEMSGIO         I/O 에러
1023 MERESOURCE      리소스 부족
1025 MECONNREFUSED   연결 거부
1027 MENOSVC         서비스 없음
1029 MESVCTIMEOUT    서비스 타임아웃
1030 MESVCABORTED    서비스 중단
```

## 5. Request/Reply 멀티플렉싱 — 결정 완료

### 5.1 결정 (2026-05-02)

**옵션 C: `mqhdr.ckey` 필드를 correlation_id로 활용한 단일 연결 멀티플렉싱**.

이유:
- C 엔진 코드 수정 없음 (ckey는 이미 wire protocol에 존재)
- `mq_frame.c`에서 ckey는 단순히 `INT2CHAR/CHAR2INT`로 envelope 메타데이터로만 다뤄짐
- broker가 라우팅 시 ckey를 변형하지 않을 가능성 매우 높음 (envelope 패턴)
- 단일(또는 소수) 연결로 동시 요청 무제한

### 5.2 동작 모델

```
┌──────────────────────────────────────┐
│ mci-api / libmymq-go (Go)            │
│                                      │
│  요청 1 ─┐                           │
│  요청 2 ─┼─→ ckey 발급 (atomic++)    │
│  요청 N ─┘   pending[ckey]=replyChan │
│              │                       │
│              ▼                       │
│        writer goroutine              │
│        (단일 writer, mutex)          │
│              │                       │
└──────────────┼───────────────────────┘
               │  TCP (영구 연결)
               ▼
            [mymqd]
               │
               │  reply 시 ckey echo
               ▼
┌──────────────┴───────────────────────┐
│ reader goroutine                     │
│  ckey 추출 → pending[ckey] dispatch  │
└──────────────────────────────────────┘
```

핵심 코드 패턴:
```go
type Client struct {
    conn      net.Conn
    writeMu   sync.Mutex
    pending   sync.Map              // ckey(uint32) → chan *Reply
    nextCkey  atomic.Uint32
    sub       chan *UnsolicitedMessage
}

func (c *Client) Call(ctx, exchange, rkey string, body []byte) ([]byte, error) {
    ckey := c.nextCkey.Add(1)
    replyCh := make(chan *Reply, 1)
    c.pending.Store(ckey, replyCh)
    defer c.pending.Delete(ckey)

    if err := c.send(ckey, exchange, rkey, body); err != nil {
        return nil, err
    }
    select {
    case r := <-replyCh: return r.Body, r.Err
    case <-ctx.Done():   return nil, ctx.Err()
    }
}

// reader goroutine
for frame := range c.frames {
    ckey := frame.Header.Ckey
    if ckey == 0 || frame.IsUnsolicited() {
        c.sub <- frame.toUnsolicited()
        continue
    }
    if ch, ok := c.pending.LoadAndDelete(ckey); ok {
        ch.(chan *Reply) <- frame.toReply()
    }
}
```

### 5.3 Phase 1 시작 시 1발 검증

**최소 검증 시퀀스** (libmymq-go의 첫 번째 통합 테스트):

1. mymqd 로컬 가동
2. Go 클라이언트로 connect + DECLARE_SESSION
3. 알려진 admin 서비스 호출 (예: `FC_ADMIN/GET_STATUS`) — `ckey = 0xDEADBEEF` 으로 송신
4. reply 받으면 `mqhdr.ckey == 0xDEADBEEF` 인지 확인

**Pass**: 옵션 C 그대로 진행 (이게 99% 시나리오)
**Fail**: broker가 ckey를 변형하면 옵션 B (connection pool)로 폴백 — 다만 wire에 변형 흔적이 있을테니 즉시 발견 가능

### 5.4 ckey 발급 정책

- Range: 1 ~ 0xFFFFFFFE (0과 0xFFFFFFFF은 reserved)
- 0: unsolicited 메시지 (broker가 0으로 보낸다고 가정)
- atomic counter, 32-bit wrap 시 collision 가능성 있으므로 **timeout pending entry GC** 함께 운영
- 동시 in-flight 한계: 4B (사실상 무제한)

### 5.5 wkey[8] 활용

`wkey`는 8바이트로 더 크지만 client 측 window 식별자 용도로 보임 (UI에서 어떤 윈도우/탭에서 온 요청인지). 멀티플렉싱에는 ckey만 사용하고 wkey는 클라이언트 → 클라이언트 자체 컨텍스트 전달 용도로 보존.

## 6. Pub/Sub 모델

### 6.1 구독 메커니즘

**놀랍게도 별도 subscribe API 없음**. 대신:

1. `mymq_openx` 시 `instance.qu_flag = QF_UNSOL_MSG | QF_UNSOL_HDR` 설정
2. broker가 자동으로 모든 push/broadcast를 이 클라이언트에 unsolicited 전달
3. 클라이언트는 receiver thread에서 모두 수신
4. 본문 앞 80바이트 `broadcast` struct 보고 자기 메시지 인지 판단

### 6.2 Bind/Service 모델 (RPC용)

서비스 제공자는 reverse 방향:
- `mymq_declare_exchange(name, ET_DIRECT/TOPIC/FANOUT)`
- `mymq_declare_queue(name, size, attr)`
- `mymq_bind_service(exchange, routing_key)`
- 들어온 요청: `mymq_recv` → 처리 → `mymq_reply`

→ **mci-api는 클라이언트 측이므로 bind 불필요. mymq_call만 사용**.

### 6.3 Broadcast 라우팅 방법

`s_parms.bcast` 값에 따라:
- `BCAST_INTERNAL`: broker 내부 라우팅 (`mymq_ioctl(FC_CAST)`)
- `BCAST_MULTICAST`: UDP 멀티캐스트
- `BCAST_INDIVIDUAL`: whois map 조회 후 개별 unicast

→ Go 클라이언트는 **수신만 하면 됨**. broadcast 송신 책임은 mymqd/엔진에 있음.

## 7. 실시간 시세 (RQ) — 통합 전략

### 7.1 RQ 아키텍처

`myrqd`는 **shared memory + sysv mq + tcp** 복합 채널:
- 메시지 본문은 SHM의 ring buffer (`rq_fifo`)에 저장
- TCP는 8바이트 alarm (`{sidx, indx, seqn}`)만 송신
- 클라이언트는 alarm 받고 SHM에서 직접 읽음
- 구독은 SysV msgsnd로 등록

**제약**: SHM 의존 → **로컬 호스트 전용**, 네트워크 너머 사용 불가.

### 7.2 mci-price 통합 — 결정 완료 (2026-05-02)

**옵션 A-1 채택**: Cooker가 양쪽(myrqd + mymqd)으로 동시 publish.

**채택 사유**:
- 순수 Go, cgo 불필요
- mci-price 호스트 자유 (mymqd 닿기만 하면 됨)
- 추가 데몬 없음 (브릿지 데몬 미사용)
- 기존 myrqd 트래픽 보존 → presto/vivace 등 기존 클라이언트 무영향
- 점진적 마이그레이션 가능

**기각 사유**:
- 옵션 B (cgo+myrqd 직접): 1ms 미만 지연 절감을 위해 빌드 복잡도, cross-compile 제약, cgo 디버깅 부담을 떠안기에 비효율적. FX MCI는 ms 단위 응답으로 충분.
- 옵션 A-2 (브릿지 데몬): 추가 데몬 운영 비용. 발행자가 어차피 cooker 측이라 5줄 추가가 더 단순.
- 옵션 A-3 (mymqd 수정): 기존 broker 코드 변경은 위험.

### 7.3 A-1 구현 명세

**변경 대상**: 시세를 `myrq_push`로 발행하는 모든 cooker / market adapter.

**변경 내용**:
```c
// 기존 (유지)
myrq_push(&pushdata);

// 추가
mymq_broadcast(mq, BROADCAST, NULL, "PRICE", NULL, NULL,
               &pushdata, sizeof(struct pushdata));
```

권장 캡슐화:
```c
int publish_price(MyMQ *mq, struct pushdata *pd) {
    int rc1 = myrq_push(pd);
    int rc2 = mymq_broadcast(mq, BROADCAST, NULL, "PRICE", NULL, NULL,
                              pd, sizeof(*pd));
    return (rc1 || rc2) ? -1 : 0;
}
```

**mymqd 설정 추가** (etc/mymqd.cfg):
```
exchange PRICE FANOUT
queue    mci_price PUBLIC SHARED
bind     PRICE * mci_price
```

**합의 필요한 운영 결정**:
- Exchange 이름: "PRICE" (확정 시)
- Exchange type: ET_FANOUT (모든 구독자 자동 전파)
- Routing key: 빈 값 (FANOUT 모드에선 무시) 또는 통화쌍 (TOPIC 모드 시)
- Compression: 시세 메시지는 1.5KB 이하라 압축 불필요 → `WITH_NO_COMPRESS`
- Cookie: 미첨부

**mci-price (Go) 동작**:
```go
mq, _ := mymq.Open(host, port, "mci-price",
    mymq.WithUnsolicited(true),
)
msgs, _ := mq.Subscribe()
for m := range msgs {
    if m.Func != FC_CAST || m.Subc != BROADCAST { continue }
    if m.Exchange != "PRICE" { continue }
    var pd PushData
    pd.Unmarshal(m.Body)
    ringBuffer.Update(&pd)
    fanout(&pd)
}
```

**운영 영향**:
- cooker 재배포 1회 필요
- 기존 myrqd 채널 무변경 → presto/vivace/HTS 클라이언트 무영향
- 신규 mci-price만 broker 통해 수신 → 안전한 단계적 도입

## 8. 어댑터 패턴 (MCA → Go)

### 8.1 MCA 채널 라이프사이클

```
client connect
  ↓
channel_init        ─→ buffers, cookie 셋업
  ↓
channel_recv (thread)  ─→ loop: read frame → parse → mca_mq_send
                              │
                              ▼
                         (mymqd로 전송, reply 대기)
                              │
                              ▼
                         channel_reply ─→ 외부 프로토콜로 변환 → write
  ↓
channel_push (callback) ─→ myrqd push → 외부 프로토콜 → write
  ↓
channel_exit
```

### 8.2 Go 매핑

```go
type Channel struct {
    conn        net.Conn          // WebSocket/HTTP
    cookie      Cookie
    mq          *mymq.Client      // mymqd 클라이언트 (per-channel 또는 pooled)
    pushEnabled bool
    logon       bool
    heartbeat   HeartbeatState
    sendMu      sync.Mutex        // socket write lock
    ctx         context.Context
}

func (c *Channel) recvLoop()  // goroutine: WebSocket → mq.Call
func (c *Channel) pushLoop()  // goroutine: subscribe events → WebSocket write
func (c *Channel) close()
```

### 8.3 mci-api / mci-push / mci-price 분담

| 서비스 | 외부 프로토콜 | 내부 동작 |
|--------|-------------|----------|
| **mci-api** | REST (HTTP) | 매 요청마다 mymq_call (또는 connection pool) |
| **mci-push** | WebSocket | unsolicited 모드 mymqd 연결 1개 + 사용자별 fan-out 자체 관리 |
| **mci-price** | WebSocket | (옵션 A) mymqd unsolicited 또는 (옵션 B) myrqd 직접 |
| **mci-admin** | HTTPS + Next.js | mymq_call로 admin (FC_ADMIN) 호출 |

## 9. libmymq-go 설계 가이드라인

### 9.1 패키지 구조

```
pkg/mymq/
├── client.go        // Client struct, Open, Close
├── handshake.go     // DECLARE_SESSION 송수신
├── frame.go         // mqhdr 인코딩/디코딩
├── frame_test.go    // C 라이브러리와의 호환성 단위 테스트
├── codec.go         // big-endian helpers, content_t 매핑
├── compress.go      // ZLIB/MLZO 디코딩 (인코딩은 일단 NONE만)
├── content.go       // Content struct (= content_t)
├── cookie.go        // Cookie struct
├── nav.go           // navi_t
├── send.go          // Send/Call/Publish/Reply
├── recv.go          // 수신 loop, dispatch
├── pool.go          // connection pool (mci-api용)
└── pubsub.go        // unsolicited 메시지 dispatch
```

### 9.2 핵심 API (1차 목표)

```go
package mymq

type Client struct { /* ... */ }

func Open(host string, port int, name string, opts ...Option) (*Client, error)
func (c *Client) Logon(cookie *Cookie) error
func (c *Client) Call(ctx context.Context, exchange, routingKey string, body []byte) ([]byte, error)
func (c *Client) Send(ctx context.Context, content *Content, body []byte) error
func (c *Client) Subscribe() (<-chan *UnsolicitedMessage, error)
func (c *Client) Close() error

type UnsolicitedMessage struct {
    Func, Subc uint8
    Exchange   string
    RoutingKey string
    LogonID    string  // broadcast prefix에서 추출
    Channel    string
    Body       []byte
}
```

### 9.3 Phase 1 구현 우선순위

1. **frame.go**: mqhdr 인코딩/디코딩 + 단위 테스트
   - C 라이브러리로 만든 프레임을 Go에서 디코딩 → 일치 검증
   - Go에서 만든 프레임을 C 라이브러리로 디코딩 → 일치 검증
2. **codec.go**: big-endian helpers, navi_t, cookie_t 직렬화
3. **client.go + handshake.go**: TCP 연결 + DECLARE_SESSION
4. **ckey echo 검증 테스트**: 단일 round-trip으로 broker가 ckey 보존하는지 확인 (Phase 1 GO/NO-GO)
5. **send.go + recv.go**: 단일 writer mutex + reader goroutine + ckey dispatcher
6. **Call**: ckey 기반 multiplex (옵션 C 직행)
7. **pending GC**: 만료된 ckey 엔트리 정리, timeout 처리
8. **Subscribe**: unsolicited 메시지 채널 (ckey=0 또는 broadcast prefix 검출)
9. **integration test**: 로컬 mymqd에 실제 connect해서 admin 명령 호출
10. **압축**: 일단 raw passthrough, 추후 ZLIB만 추가
11. **heartbeat**: 4바이트 빈 프레임 송수신, 2*interval 미수신 시 재연결
12. **재연결**: 끊김 감지 + 재시도 + pending에 에러 통보

### 9.4 검증 전략

- **단위 테스트**: 알려진 C 라이브러리 출력과 byte-by-byte 일치
- **호환성 테스트**: 로컬 mymqd 띄우고 Go 클라이언트로 connect → DECLARE_SESSION → admin 명령 (`GET_STATUS`) → 응답 파싱
- **부하 테스트**: 다수 동시 연결 + sustained 요청
- **카오스 테스트**: TCP drop, slow consumer, large message

## 10. 미해결 / Phase 1에서 결정할 사항

| 항목 | 영향 | 결정 방법 |
|------|------|----------|
| ~~ckey echo 여부~~ | ~~멀티플렉싱 가능성~~ | **결정 (2026-05-02): ckey 활용으로 확정. Phase 1 첫 통합테스트로 실측 검증** |
| ~~시세 발행 흐름~~ | ~~mci-price 옵션 A/B~~ | **결정 (2026-05-02): 옵션 A-1 확정. Cooker 측 5줄 변경으로 양쪽 publish** |
| `size >> 27` vs `>> 24` | 압축 호환성 | 실제 mymqd 동작 확인 |
| 압축 알고리즘 우선순위 | 구현 우선순위 | 프로덕션 트래픽 분석 |
| `wkey`의 사용처 | UI window 매핑 인지 | presto/vivace 어댑터 확인 |
| 인증 메커니즘 상세 | LOGON/COOKIE 보안 | engine 측 검증 로직 검토 |
| heartbeat interval 기본값 | 클라이언트 설정 | DECLARE_SESSION 응답 |
| 클러스터링 활용 여부 | NCID 처리 | 운영 환경 (단일/멀티 호스트) |

## 11. 다음 단계 (Phase 1 진입 조건)

**Phase 1 시작 전 확인할 것**:
1. ✅ wire protocol 명세 확보 (본 문서)
2. ✅ 핵심 API 목록 확보
3. ✅ 멀티플렉싱 전략 — ckey 활용 옵션 C 확정 (실측 검증은 Phase 1 첫 작업)
4. ✅ 시세 통합 — 옵션 A-1 확정 (Cooker 양쪽 publish)
5. ⏳ Go 모듈 구조 합의 (monorepo? 단일 모듈?)
6. ⏳ Exchange 이름/타입/routing key 컨벤션 (운영팀 합의)

**Phase 1 deliverable**:
- `libmymq-go` 패키지 (frame, codec, client, handshake, send, recv, call, subscribe)
- 단위 테스트 + 통합 테스트 (로컬 mymqd 연동)
- mci-api의 가장 단순한 endpoint 1개 prototype
- 운영자가 보고 검토할 기술 문서

---

## 부록: 빠른 참조

### Go 코드 시작점 예시

```go
// pkg/mymq/frame.go
package mymq

import (
    "encoding/binary"
    "errors"
)

const (
    HdrSize  = 84
    NaviSize = 32
    LXchg    = 8
    LRkey    = 16
    LName    = 16
    LSymb    = 20
    LSkey    = 80
)

type Header struct {
    Length     uint32
    Func       uint8
    Subc       uint8
    Nvia       uint8
    Dirf       uint8
    Msgf       uint8
    Ctlf       uint8
    Keyc       uint8
    Xchg       [LXchg]byte
    Rkey       [LRkey]byte
    Ckey       uint32
    Clid       uint32
    Wkey       [8]byte
    Chan       [4]byte
    Errn       uint32
    CoffZipf   uint8
    CoffOff    uint32  // 24-bit
    SoffZipf   uint8
    SoffOff    uint32  // 24-bit
    ErrmLen    uint16
    ErrmOff    uint16
    PkeyLen    uint16
    PkeyOff    uint16
    NkeyLen    uint16
    NkeyOff    uint16
    BodyZipf   uint8
    BodyOff    uint32  // 24-bit
}

func encodeWHERE(zipf uint8, off uint32) [4]byte {
    var b [4]byte
    b[0] = zipf
    b[1] = byte(off >> 16)
    b[2] = byte(off >> 8)
    b[3] = byte(off)
    return b
}

func decodeWHERE(b [4]byte) (zipf uint8, off uint32) {
    zipf = b[0]
    off = uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
    return
}

// EncodeHeader writes 84-byte mqhdr to dst (must be at least 84 bytes).
func (h *Header) EncodeHeader(dst []byte) error {
    if len(dst) < HdrSize {
        return errors.New("buffer too short")
    }
    binary.BigEndian.PutUint32(dst[0:4], h.Length)
    dst[4] = h.Func
    dst[5] = h.Subc
    dst[6] = h.Nvia
    dst[7] = h.Dirf
    dst[8] = h.Msgf
    dst[9] = h.Ctlf
    dst[10] = 0
    dst[11] = h.Keyc
    copy(dst[12:20], h.Xchg[:])
    copy(dst[20:36], h.Rkey[:])
    binary.BigEndian.PutUint32(dst[36:40], h.Ckey)
    binary.BigEndian.PutUint32(dst[40:44], h.Clid)
    copy(dst[44:52], h.Wkey[:])
    copy(dst[52:56], h.Chan[:])
    binary.BigEndian.PutUint32(dst[56:60], h.Errn)
    coff := encodeWHERE(h.CoffZipf, h.CoffOff)
    copy(dst[60:64], coff[:])
    soff := encodeWHERE(h.SoffZipf, h.SoffOff)
    copy(dst[64:68], soff[:])
    binary.BigEndian.PutUint16(dst[68:70], h.ErrmLen)
    binary.BigEndian.PutUint16(dst[70:72], h.ErrmOff)
    binary.BigEndian.PutUint16(dst[72:74], h.PkeyLen)
    binary.BigEndian.PutUint16(dst[74:76], h.PkeyOff)
    binary.BigEndian.PutUint16(dst[76:78], h.NkeyLen)
    binary.BigEndian.PutUint16(dst[78:80], h.NkeyOff)
    body := encodeWHERE(h.BodyZipf, h.BodyOff)
    copy(dst[80:84], body[:])
    return nil
}
```

— END of Phase 0 보고서 —
