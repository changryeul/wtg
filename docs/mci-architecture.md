# MCI 아키텍처

> WTG (Winway Trading Gateway) — 외환 매매 엔진 (MyMQ broker + AP) 앞단의 web 게이트웨이
> 최종 업데이트: 2026-05-03
> 짝 문서: [mci-test-runbook.md](./mci-test-runbook.md)

---

## 1. 개요

WTG 는 외환 매매 엔진(MyMQ broker + AP) 앞에 두는 web 게이트웨이다. 핵심 원칙
세 가지:

- **비즈니스 권한 검증은 매매 엔진에 위임** — WTG 는 web 보안만 처리한다 (JWT
  검증, IP 화이트리스트, 정책 cut-in, rate-limit 등). 거래 자체의 적법성은
  broker 뒤편의 매매 엔진이 결정한다.
- **transaction 별 핸들러 X** — `POST /v1/tx` 한 개로 모든 매매 transaction 을
  broker 에 generic passthrough 한다.
- **Client.applyDefaults 가 navi(origin/destination) 를 자동 채움** — 빠지면
  broker 가 "no navigation" 으로 거부하므로 client 라이브러리가 보장한다.

mci-* 컴포넌트는 모두 broker (mymqd) 에 mymq client 로 connect 해서 메시지를
흘린다 — broker 가 모든 라우팅의 중심이다.

---

## 2. 컴포넌트 카탈로그

```
                                  ┌─ DMZ (외부 노출) ──────────┐  ┌─ Internal (사내) ──────────────┐
                                  │                            │  │                                 │
사용자 단말 (브라우저/모바일) ──▶        │  mci-edge-api    (8090) ──▶│  │  mci-api      (8080) ─┐        │
                                  │  mci-edge-push   (8084) ──▶│  │  mci-push     (8081) ─┼─→ mymqd │
                                  │  mci-edge-price  (8083) ──▶│  │  mci-price    (시세) ─┤  (11217)│
                                  └────────────────────────────┘  │                       │         │
                                                                  │  mci-admin    (9090) ─┤         │
                                                                  │  quote-forwarder ─UDP→┘         │
                                                                  └─────────────────────────────────┘
                                                                                      ↓
                                                                              AP / 매매 엔진
                                                                       (TSTSVC / W*/BW* 같은 svc)
```

| 카테고리             | 바이너리            | 포트                    | mymq channel                | 책임                                                                  |
| ---------------- | --------------- | --------------------- | --------------------------- | ------------------------------------------------------------------- |
| **DMZ (edge)**   | mci-edge-api    | 8090                  | (HTTP only)                 | 외부 → 사내 reverse proxy. JWT 검증 + IP 화이트리스트 + Rate limit              |
|                  | mci-edge-push   | 8084                  | (gRPC client)               | mci-push 의 PushService 에 sub → 외부 ws fan-out                        |
|                  | mci-edge-price  | 8083                  | (gRPC client)               | 시세 외부 ws fan-out                                                    |
| **Internal API** | mci-api         | 8080                  | ChannelWeb                  | `POST /v1/tx`, `/v1/login`, `/v1/logout`, `/v1/refresh`, `/v1/ping` |
|                  | mci-admin       | 9090                  | ChannelAdmin                | 운영자 콘솔 + admin 명령 + 정책/룰 관리 + tx-test/push-test proxy               |
|                  | mci-push        | 8081                  | ChannelWeb (representative) | broker 의 unsolicited 받아 user 별 ws fan-out                           |
|                  | mci-price       | (TBD)                 | ChannelWeb                  | 시세 fan-out 전용                                                       |
| **추가**           | quote-forwarder | UDP 30044~30051       | ChannelAdmin                | UDP FX 시세 → broker broadcast publish                                |
| **Broker**       | mymqd           | 11217 (cluster 11218) | —                           | mymq message broker (docker 컨테이너)                                   |
| **Broker내**      | test_service    | (broker 안 entrypoint) | —                           | TSTSVC/PING echo 서비스 (single rkey, round-trip 검증용)                  |
| **Broker내**      | WECHO           | (broker 안 entrypoint, `win/src/trn/WECHO`) | —          | 운영 svcmain 패턴 prototype — multi rkey (PING/ECHO/UPPER/TIME/INFO) |

---

## 2.1 내부 클라이언트도 mci 경유 — 라우팅 권고

내부 도구 (CS 콘솔 / 백오피스 / 운영 batch) 가 매매 svc 를 호출할 때, 두 가지
선택지가 있다:

A. **mymq client 로 broker 직접 connect** — 외부 W3100 들이 쓰던 전통 패턴
B. **mci-api `POST /v1/tx` 경유** (alias 또는 `exchange/routing_key` envelope)

**권고: B (mci 경유) 를 default 로 채택**한다. 이유 3가지:

1. **정책/감사의 비대칭이 사고로 직결** — 직접 broker 경로는 kill switch /
   maintenance / blocked symbols / IP allowlist / rate limit / audit ring 을
   모두 우회한다. CS 가 사고 시점에 거래를 칠 수 있다는 건 컴플라이언스 측면
   에서 받아들이기 어렵고, 사고 사후 분석에서 *누가 무엇을 호출했는지* 의
   단일 source of truth 가 깨진다.
2. **alias 추상화의 가치 자체가 일관성** — `wtg-routes.json` + sync 정책으로
   만든 라우팅 룰 layer 는 *서비스 변경 시 호출자 코드 무수정* 이 핵심 가치.
   내부만 raw `exchange/routing_key` 를 박아두면 svc rkey 가 바뀔 때 외부는
   무영향, 내부는 다 깨지는 비대칭이 생긴다.
3. **HTTP 오버헤드는 LAN 환경에서 무시 가능** — 측정 결과 mci → broker
   round-trip 25~35ms 중 broker call 자체가 대부분이고 mci layer 는 1ms 미만.
   CS 인터랙티브 작업의 인지 임계 (~100ms) 와는 두 자릿수 차이.

**예외 인정 기준** — 다음 경우만 raw broker 가 정당화된다:
- broadcast/sub 패턴이 잦은 시세성 도구 (이미 quote-forwarder 가 그렇게 동작)
- 분당 수만 건 처리 batch 도구 (HTTP envelope 오버헤드 누적이 의미 있을 때)

위 예외도 *정책 우회 책임을 운영팀이 명시 수용* 하는 등록 절차를 거쳐야 한다.

**호출 모양 — 내부 CS 도구 예시**:

```bash
# alias 기반 (라우팅 룰 변경에 면역)
curl -H 'X-WTG-User: cs01' -X POST http://mci-api.internal:8080/v1/tx \
    -d '{"alias":"WECHO_PING","data":""}'

# 또는 raw envelope (alias 미등록 svc 의 즉석 검증 용도만)
curl -H 'X-WTG-User: cs01' -X POST http://mci-api.internal:8080/v1/tx \
    -d '{"exchange":"ECHOSVC","routing_key":"PING","data":""}'
```

내부망 직결이라 mci-edge-api 는 거치지 않아도 된다 (DMZ 는 외부용). 단,
인증 모드는 운영시 JWT (mci-admin 이 발급) 또는 SSO 를 권장.

---

## 3. 핵심 데이터 흐름 — 3 가지 모델

### 3.1 Transaction (sync RPC) — `POST /v1/tx`

```
[클라이언트]                              [WTG]                                 [매매 엔진]

브라우저  ─POST /v1/tx────────────▶  mci-api
       JSON envelope:               (인증/정책/audit)
       {exchange,routing_key,data}       │
                                          │ libmymq-go (ChannelWeb)
                                          │ FrameInput{Func=Call, Xchg, Rkey, Body, Ckey}
                                          │
                                          ▼
                                       mymqd
                                  (routing table 매칭)
                                          │
                                          ▼
                                  service AP (test_service / W* / BW*)
                                          │
                                          │ reply (Body="ECHO:...")
                                          ▼
                                       mymqd
                                          │
                                          ▼
       JSON envelope ◀───────────  mci-api
       {data:"ECHO:..."}
```

핵심 포인트:
- envelope 의 `alias` 를 쓰면 mci-admin 의 라우팅 룰 store 에서
  `alias → exchange/routing_key` 로 resolve 된다.
- 정책 엔진이 `kill_switch` / `maintenance` / `blocked_symbols` /
  `blocked_routing_keys` 로 cut-in 가능 (모두 broker 호출 전 단계).
- **broker 가 ckey 를 응답에 echo back** (mci-test 가 검증한 option C
  멀티플렉싱) — 단일 connection 으로 동시 호출 가능. mci-api 는 ckey 별
  대기 채널만 분기한다.

### 3.2 Push (server → client unsolicited) — broker 가 user 에게 능동 전송

```
[발사자]                              [broker]                         [구독자]

mci-admin
POST /v1/admin/push-test ──FCPush──▶ mymqd
{user, data}                            │
                                        │ "Published N/M to <user>"
                                        │
                                        ├─▶ user 별 LOGON 한 client (있으면)
                                        │
                                        └─▶ representative receiver (mci-push)
                                                  │
                                                  │ Subscribe() 채널
                                                  ▼
                                              dispatcher
                                              prefix.LogonID 로 ws Registry 조회
                                                  ▼
                                              ws.Send
                                                  ▼
                                            브라우저 ws ←───────
```

핵심 포인트:
- **`QF_UNSOL_REP` flag 를 켠 client (mci-push)** 가 broker 의 representative
  receiver 로 등록되어 *user 매칭 없이 모든 publish 를 받음*
  (broker `publish.c` 의 `_REPRESENTATIVE_UNSOL_RECVER_` 분기).
- **큐 이름이 빈값** 이어야 broker 가 client 를 `_CLIENT_` type 으로 등록하고
  publish 후보로 인정한다 (`dispatch.c:91` 이 큐 이름 있으면 `_SERVER_`,
  `publish.c:185-189` 가 `_CLIENT_` 만 후보로 인정).
- broadcast prefix 80B 의 `LogonID` 로 target user 결정. 빈값이면 전체
  broadcast (시세 채널이 사용).

### 3.3 시세 (UDP → broadcast → ws fan-out)

```
[시세 source]                       [forwarder]                  [broker]              [ws 사용자]

replay_smb2 / nc / wtgctl quote
       │ UDP 30044/30045/30046/30051
       ↓
   ┌─────────────┐
   │ quote-fwd   │ FIX 4.4 파싱 (35=W/X)
   │             │ → JSON envelope
   └──────┬──────┘
          ↓ FCCast/SubBroadcast
       mymqd ─ broadcast (LogonID="")
          │
          └─▶ representative receiver (mci-push)
                   │
                   │ dispatcher: LogonID 빈값
                   │     → 전체 user fan-out
                   ▼
              모든 ws 사용자 ◀
```

forwarder 출력 envelope:
```json
{
  "ts": "2026-05-03T01:50:04.338422Z",
  "feed": "SMB",
  "seq": 1,
  "msgtype": "snapshot",
  "symbol": "USDKRW",
  "sender": "SMB",
  "target": "SUB",
  "entries": [
    {"type":"bid","px":1380.5,"qty":1000000},
    {"type":"ask","px":1380.7,"qty":1500000}
  ]
}
```

**옵션 1 (현재)**: forwarder 가 cooker 우회 — UDP 직접 listen.
**옵션 2 (정통)**: forwarder 가 cooker 의 multicast 224.0.0.1:30022 에 join —
가공된 시세 받음.
**옵션 3 (PAL 통합)**: cooker 의 `mds_mq` PAL 백엔드를 broker push 로 교체 —
forwarder 불필요.

---

## 4. 포트 / 채널 / 인증 매트릭스

| 컴포넌트 | listen | broker channel | 주 인증 모델 |
|---|---|---|---|
| mymqd | 11217 (TCP), 11218 (cluster) | (broker self) | mymq native |
| mci-api | 8080 | ChannelWeb | DevMode `X-WTG-User`, 운영 JWT |
| mci-push | 8081 (HTTP/WS) | ChannelWeb + `QfUnsolMsg \| QfUnsolHdr \| QfUnsolRep` | 동상 (ws connect 시) |
| mci-admin | 9090 | ChannelAdmin | DevMode `X-WTG-User`, 운영 JWT (운영자 SSO/MFA 추후) + IP CIDR |
| quote-forwarder | UDP 30044~30051 | ChannelAdmin | (broker 발사 권한) |
| mci-edge-* | 8083~8090 | (HTTP/gRPC client) | 외부 JWT 검증 |

브라우저는 사용자 정의 헤더를 ws upgrade 에 못 보내므로 query 우회를 둔다:
- `?access_token=<jwt>` → `BearerFromQuery` 가 `Authorization` 헤더로 변환
- `?x_wtg_user=<id>` → `UserFromQuery` 가 `X-WTG-User` 헤더로 변환 (DevMode 만)

또한 mci-push 는 **DevMode 시 CheckOrigin 모두 허용** 하도록 설정된다 (gorilla
의 same-origin 기본 정책이 mci-admin UI(9090) → mci-push(8081) cross-origin
ws 를 막기 때문).

---

## 5. broker (mymq) 인터페이스 정리

### 5.1 Frame 구조 (84B 고정 헤더 + 가변 영역)

```
Func (FCCntl/FCAdmin/FCNotify/FCCast/FCPush/FCSignal/FCService/...)
Subc (DECLARE_SESSION/CONNECT/GET_STATUS/BROADCAST/PUSH/...)
Dirf (메시지 방향), Keyc (키 동작)
Xchg, Rkey  (목적지 exchange/routing key)
Ckey        (correlation id — broker 가 echo back)
Wkey, Chan  (window key, origin channel)
가변 영역: Cookie, Errm, Pkey, Nkey, Body
```

### 5.2 Channel 종류

| 상수              | 용도                                               |
| --------------- | ------------------------------------------------ |
| `ChannelWeb`    | 일반 web/mobile client (mci-api, mci-push)         |
| `ChannelMobile` | 모바일 전용                                           |
| `ChannelAdmin`  | 직원 콘솔 (admin 명령 권한, mci-admin / quote-forwarder) |

### 5.3 Queue 옵션 (mci-push 가 사용)

| 상수 | 의미 |
|---|---|
| `QtClient` | 일반 client 큐 |
| `QfUnsolMsg` (0x01) | unsolicited 수신 가능 |
| `QfUnsolHdr` (0x02) | broadcast prefix 80B 수신 |
| `QfUnsolRep` (0x04) | **representative receiver** — 모든 publish wiretap |

### 5.4 Broadcast prefix (80B) — Push/Cast 본문 앞에 붙는 헤더

```
IPAddr[24] + Exchange[16] + Chan[16] + User[16] + LogonID[16]
            + Func[1] + SubFunc[1] + ViaNet[1] + Debug[1]
```

- `LogonID` 가 target user 식별. 빈값이면 broadcast 모드 (전체 user).
- mci-push dispatcher 가 이 LogonID 로 ws Registry 를 조회해 fan-out 한다.

### 5.5 자주 쓰는 Func / Subc

| Func | 값 | 용도 |
|---|---|---|
| `FCCntl` | 0 | session 관리 (DECLARE_SESSION/CONNECT/LOGON) |
| `FCAdmin` | 3 | admin 명령 (GET_STATUS, LIST_CLIENTS, WHOIS) |
| `FCCast` | 4 | broadcast (모든 representative receiver) |
| `FCNotify` | 5 | 단방향 통지 |
| `FCPush` | 13 | user-targeted push |
| `FCSignal` | 14 | 시그널 (제어성 통지) |
| `FCService` | 7 | service RPC |

| Subc | 값 | 용도 |
|---|---|---|
| `SubBroadcast` | 50 | 모든 구독자 |
| `SubUnicast` | 51 | 단일 수신자 |
| `SubPush` | 54 | 실시간 push |
| `SubKill` | 56 | 강제 종료 |
| `SubGetStatus` | 150 | broker 상태 조회 |

---

## 6. mci-admin 의 layer 구조

mci-admin 은 web UI + admin REST API + in-memory store 를 한 binary 에 담는다.
정적 파일 (UI HTML) 은 `embed.FS` 로 바이너리에 박혀 있어 별도 프론트 빌드/배포
없이 단독 실행 가능. 응답 헤더에 `Cache-Control: no-store` 강제로 markup/JS
부정합 사고를 방지한다.

```
브라우저 SPA (단일 HTML, embed.FS)
   ↓ HTTP /v1/* (auth + IP CIDR 화이트리스트 통과)
   │
   ├─ admin 명령 (broker 호출)
   │    GET  /v1/admin/status         broker GET_STATUS
   │    GET  /v1/admin/clients        broker LIST_CLIENTS (errn=1014 정상 — 인자 부족)
   │    GET  /v1/admin/exchanges      broker LIST_EXCHANGES
   │    GET  /v1/admin/whois          broker WHOIS
   │    POST /v1/admin/cmd            generic admin frame 발사
   │    POST /v1/admin/push-test      ★ user-targeted FCPush 발사 (broker 발사기)
   │
   ├─ 라우팅 룰 (in-memory store, etcd 옵션)
   │    GET/PUT/DELETE /v1/admin/routes/{alias}
   │    POST /v1/admin/routes/{alias}/active
   │
   ├─ 정책 엔진 (in-memory)
   │    GET  /v1/admin/policy
   │    POST /v1/admin/policy/kill-switch
   │    POST /v1/admin/policy/maintenance
   │    POST /v1/admin/policy/blocked-{symbols,routing-keys}
   │
   ├─ 감사 로그 (200 entry ring buffer)
   │    GET  /v1/admin/audit
   │
   ├─ 실시간 stream (ws)
   │    GET  /v1/admin/stream         정책/룰/감사 변경 push
   │
   └─ tx 테스터 reverse proxy (DevMode 검증용)
        POST /v1/admin/tx-test  → mci-api 의 /v1/tx
```

mci-admin 은 라우팅 룰과 정책을 자체 메모리에 보관 + 변경 시 ws stream 으로
동일 원본 다른 admin 인스턴스에 push. 운영 환경에선 etcd 동기화로 다중
인스턴스를 지원한다.

UI 페이지 (사이드바):
- 대시보드 — KPI 카드 + sparkline + Chart.js 시계열 (2초 polling)
- 라우팅 룰 — alias CRUD
- 정책 엔진 — kill switch / maintenance / blocked symbols/rkeys 토글
- 브로커 명령 — broker admin endpoint 호출 (Status/Clients/Exchanges)
- API 테스터 — preset (헬스체크/Routes/Whois/Tx echo/Push 발사 등) + 임의
  endpoint 호출. 요청 패널 + 응답 패널 (헤더/바디 dump)
- WS 모니터 — ws://...:8081/v1/subscribe 같은 ws connect, 메시지 stream
- **시세** — mci-push ws 자동 connect, 통화쌍별 호가창(BID/LAST/ASK 변동), 최근 체결
- 감사 로그 — 정책/룰 변경 timeline

---

## 6.1 운영 svcmain 패턴 vs WECHO prototype

운영 매매 서비스 (W3100, W3200 등) 는 `win/src/lib/main/biz_main.c` 의 `svcmain()`
wrapper + `callme[]` 디스패치 모델을 쓴다. svcmain 은 KB 운영환경의 다음에
의존한다:
- broker cfg 의 `<service><active exchange="dom" rkey="*" domain="lgr"/></service>` (외부 LGR cluster forward)
- `<queue sharing="yes"/>` + KB SHM 키
- mymq lib 내부의 `mymq_openx()` 가 `mymq_open()` 과 다른 path 사용

**우리 dev cfg 에서 svcmain 이 ERROR 1000 (MS-1000 SYS ERROR) 으로 fail** 하는
이슈를 확인 — mymq lib 의 `mq_error.c` 가 GNU `strerror_r` 의 결과를
`stack-uninitialized buffer` 에 의존하는 cosmetic + 운영 cfg 의존성 결합 때문.
운영 패턴 그대로 동작시키려면 NH 사내 라이브러리(libwfaapi/axapi) + DB2 + KB SHM
환경 셋업이 필요해서 Phase 7 (운영기능 통합) 단계로 미뤘다.

**대신 동일한 callme[] 모델을 svcmain 우회로 동작시키는 prototype** 으로
`win/src/trn/WECHO/` 를 두었다:
- `mymq_open` → `mymq_declare_exchange` → `mymq_declare_queue` → `mymq_bind_services`
  (복수, 한 frame 에 array 로 N entry) → 자체 recv loop + dispatch
- 운영 svcmain 정상화 후엔 main wrapper 만 `biz_main_standalone.c` 로 갈아끼움
- `Makefile.standalone` — mymq lib 만 의존, 사내 lib stub 불필요

기록된 함정:
1. `mymq_bind_service` 단수를 N번 호출하면 broker routing 테이블에 누적 안 됨.
   반드시 `mymq_bind_services` 복수 array 를 한 번 호출.
2. `content_t.sndb` 는 svcmain 프레임워크 외부에서 NULL — 별도 reply buffer
   사용 (`char rep[64*1024]` + `mymq_reply(mq, ct, rep, len)`).
3. `mymq_recv` 가 len=0 인 경우는 *빈 body 의 정상 메시지* (PING 등) — `len < 0`
   만 continue, `len == 0` 도 dispatch + reply 해야 한다.

---

## 7. 라우팅 룰 (alias) 모델

mci-admin 에 등록한 라우팅 룰:
```json
{
  "alias": "ORDER_NEW",
  "exchange": "ORDER",
  "routing_key": "NEW",
  "active": true,
  "comment": "신규 주문 routing"
}
```

mci-api 의 `Transaction` 핸들러가 `transform.Envelope.BuildFrame` 에서 alias 가
있으면 in-memory `routing.Registry` 를 조회해 exchange/routing_key 로 resolve.
inactive 또는 미등록 alias 는 `ErrUnknownAlias` 로 거부.

운영에서는 **alias 만으로 호출** 하는 형태가 권장 — 매매 엔진의 exchange/rkey
가 변경되어도 클라이언트 코드 영향 없음.

**DevMode 자동 시드** — `pkg/routing/dev_seed.go` 의 `SeedDevRoutesEx()` 가
mci-admin / mci-api 부팅 시 호출되어 broker entrypoint 가 자동 띄우는 service
들의 alias 를 미리 등록한다. 동일 alias 가 이미 있으면 skip (사용자 편집을
덮어쓰지 않음). 운영 (`--dev=false`) 에선 호출되지 않으며 etcd 가 source of
truth.

시드 source 선택:
- **외부 cfg 파일** (권장, `~/mymq/etc/wtg-routes.json`) — `--dev-routes-file=...`
  flag 또는 `WTG_{ADMIN,API}_DEV_ROUTES_FILE` env. 새 서비스 추가 시 cfg 한 줄
  추가만으로 시드 변경 가능
- **hardcode default** — cfg path 가 비거나 파일 부재 시 fallback (회귀 안전)

**Hot reload** — `routing.WatchRoutesFilePolicy()` 이 cfg 파일의 mtime 을 2초마다
polling 해서 변경 감지 시 자동 재시드. fsnotify 의존성 0 (`tlsutil/reloader.go`
와 같은 패턴).

**시드 정책** — `--dev-routes-policy` flag (또는
`WTG_{ADMIN,API}_DEV_ROUTES_POLICY` env). `pkg/routing.SeedPolicy` 참조.

| 정책 | cfg → in-memory 동작 | 사용 장면 |
|---|---|---|
| `additive` (기본) | 새 alias 만 추가. 기존 alias 의 exchange/rkey/active/comment 변경은 무시. cfg 에서 빠진 alias 도 in-memory 유지 | UI 에서 즉석으로 추가/수정한 룰을 보존하고 싶을 때 (개발 탐색 모드) |
| `sync` | cfg 가 진실의 원천. 모든 cfg alias 를 upsert (변경 필드 덮어쓰기) + cfg 에 없는 in-memory alias 는 Delete | `wtgctl routes del/set` 가 즉시 in-memory 에 반영되길 원할 때 (운영 ops 모드) |

`sync` 모드 주의 — UI 만으로 추가한 룰은 hot reload 시 cfg 에 없으면 사라진다.
운영자가 의식적으로 선택해야 한다. cfg 파싱 실패 시엔 sync 약속을 지킬 수 없으므로
강제로 additive 로 fallback (안전 fallback).

```
새 서비스 추가 흐름 (Phase 5, additive):
1. 서비스 코드 작성
2. ~/mymq/etc/wtg-routes.json 에 한 줄 추가 (재기동 없음)
3. ≤ 2초 후 자동 재시드
4. 라우팅 룰 / API 테스터 화면에 자동 출현

기존 alias 변경/삭제 흐름 (sync 정책):
1. WTG_ROUTES_POLICY=sync wtgctl start  ──┐
2. wtgctl routes del NAME                  ├─ ≤ 2초 후 in-memory 에서도 자동 제거
3. wtgctl routes set NAME field=value      ┘   (변경 필드 덮어쓰기)
```

기본 시드 (총 6 alias):

| alias | exchange | routing_key | 매핑 service |
|---|---|---|---|
| `TSTSVC_PING` | `TSTSVC` | `PING` | test_service (single rkey) |
| `WECHO_PING` | `ECHOSVC` | `PING` | WECHO → "PONG" |
| `WECHO_ECHO` | `ECHOSVC` | `ECHO` | WECHO → "ECHO:" + payload |
| `WECHO_UPPER` | `ECHOSVC` | `UPPER` | WECHO → 대문자 변환 |
| `WECHO_TIME` | `ECHOSVC` | `TIME` | WECHO → 현재 시각 ISO8601 |
| `WECHO_INFO` | `ECHOSVC` | `INFO` | WECHO → pid + uptime |

호출 예:
```json
POST /v1/tx
{"alias": "WECHO_PING", "data": ""}
→ {"data": "PONG"}
```

mci-admin UI 의 "라우팅 룰" 화면에 6개 entry 가 자동 표시. UI 효율화 두 단계:

- **API 테스터의 동적 alias preset** — 화면 진입 시 `/v1/admin/routes` fetch →
  active alias 마다 button 자동 생성. 새 서비스 추가 시 코드 수정 0 (cfg + UI 등록만)
- **라우팅 룰 → ▶ 테스트 deep link** — 각 entry 의 `▶ 테스트` 버튼 클릭 시
  API 테스터 화면으로 navigate + `{"alias":...,"data":""}` 자동 채움. 화면 한 곳에서
  alias lifecycle (등록/활성/수정/삭제/**테스트**) 완결

---

## 8. 정책 엔진 (policy)

mci-admin 의 in-memory `policy.Engine` 이 mci-api 의 Transaction 흐름에
cut-in 한다 (broker 호출 전).

| 항목 | 동작 |
|---|---|
| `kill_switch` | ON + `kill_switch_channels` 비어있음 → 모든 transaction 503 |
| `kill_switch_channels` | ON + 비어있지 않음 → 그 채널만 503. 예: `["WEB","MOB","HTS"]` 면 고객 차단, ADM/EMP 통과 |
| `maintenance` | datetime 범위 안이면 503 |
| `blocked_symbols` | envelope.data.symbol 매칭 시 403 |
| `blocked_routing_keys` | envelope.routing_key 매칭 시 403 |

**채널 spoof — DevMode 검증 도구**: mci-api 의 auth 미들웨어가 DevMode 에서
`X-WTG-Channel` 헤더를 Principal.Channel 로 사용. UI 의 API 테스터에 채널
select drop-down 추가 — 정책 분기 검증 시 한 클릭으로 채널 바꿔 호출 가능.
운영 모드에선 SSO/JWT claim 의 채널이 우선이라 헤더 무시.

**전파 경로** — mci-admin 과 mci-api 는 각자 in-memory `policy.Engine` 을 가지
므로 admin 토글이 api 까지 전파될 sync 채널이 필수다 (없으면 split-brain →
admin UI 만 ON 이고 실제 거래는 차단되지 않음).

| 환경 | sync 메커니즘 | 패키지 |
|---|---|---|
| 운영 | etcd 단일 key (`wtg/policy`) Put + watch | `policy.StartEtcdSync` |
| DevMode | mci-api 가 `GET /v1/admin/policy` 를 2 초 주기로 poll → `ApplyRemote` | `policy.StartHTTPPoll` |

DevMode poll 은 `mci-api --dev-policy-url=...` flag 또는 `WTG_API_DEV_POLICY_URL`
env 로 활성화. wtgctl 의 `cmd_start` 가 자동 주입한다. 단방향 (admin → api),
api 의 변경은 의미 없음 (admin 만 mutator).

UI 의 정책 엔진 화면 토글은 `POST /v1/admin/policy/kill-switch` 등으로 admin 의
Engine 을 갱신하고, ws stream 으로 다른 admin 탭에 broadcast — 동시에 etcd Put
또는 admin endpoint 를 통해 api 인스턴스로 전파된다.

---

## 9. 인증 / Principal 모델 (auth.md 발췌)

DevMode (현재 셋업):
- 운영자: mci-admin UI 의 "ID 만으로 입장" → `localStorage` 의 `wtg_user`,
  `wtg_dev_mode` 저장 → 모든 API 호출에 `X-WTG-User` 헤더 자동 주입.
- 사용자: mci-api 의 `/v1/tx` 도 `X-WTG-User` 헤더 필요 (운영에서는 JWT 의
  claim 으로 대체).

운영 모델 (Phase 2~3):
- mci-edge-api 가 외부 JWT 검증 → `X-WTG-SID` 같은 internal 헤더로 변환 →
  internal mci-* 가 그 헤더 신뢰 (mTLS 사내망 한정).
- 운영자 콘솔 (mci-admin) 은 SSO + MFA 통합 (Phase 7).

**edge 보안 layer 정리** — 3 edge 모두 같은 방식:

| layer | flag | 설정 시점 | 목적 |
|---|---|---|---|
| IP allowlist | `--allow-cidrs` (`WTG_E*_ALLOW_CIDRS`) | chain 의 *가장 바깥* | 비-허용 출발지 즉시 403. auth/ratelimit 자원 절약 |
| Rate limit | `--ip-rate` / `--ip-burst` | allowlist 통과 후 | IP 단위 sustained TPS / burst |
| JWT auth | (auto) | rate-limit 통과 후 | DevMode 면 X-WTG-User, 운영이면 RS256 JWT |
| Server TLS | `--tls-cert` / `--tls-key` (옵션 mTLS `--tls-client-ca`) | 외부 종단점 | 일반적으로 ingress/LB 가 처리, edge 자체도 가능 |
| Upstream TLS | `--upstream-tls-*` / `--grpc-tls-*` | edge → internal 호출 | mTLS 사내망 한정 |

`pkg/netutil.IPAllowList` 미들웨어는 admin 의 `internal/admin/ipallow.go` 와
같은 의도지만 edge 가 admin 패키지를 import 할 수 없어 (의존 방향 금지) 별도
패키지로 추출. `ParseCIDRs` 도 같이 제공.

`Principal` 구조:
- `Usid` — 사용자 ID (logonID 와 동일)
- `Channel` — Web/Mobile/Admin
- `SessionID` — 운영 모드의 broker session
- `Cookie` — 운영 모드의 broker cookie_t

---

## 10. 추가 도구 — quote-forwarder

UDP FX 시세를 broker 로 broadcast 하는 단일 binary. 한 인스턴스가 여러 거래소
를 동시에 받을 수 있다.

```
quote-forwarder \
    --multi=SMB:30044,KMB:30045,EBS:30046,REUT:30051 \
    --broker-host=127.0.0.1 --broker-port=11217
```

기능:
- UDP listen (각 거래소별 별도 goroutine)
- FIX 4.4 Market Data 파싱 (35=W Snapshot, 35=X Incremental)
- JSON envelope (symbol/bid/ask/trade entries) 으로 변환
- broker 로 FCCast/SubBroadcast 발사 (LogonID 빈값 — 전체 user fan-out)

원본 FIX 가 필요하면 `--include-fix=true` 로 envelope 에 같이 박는다 (SOH 는
가독성을 위해 `|` 로 치환).

---

## 11. 운영 control — wtgctl

전체 stack 의 기동/종료/상태/시세 burst 를 한 도구로:

```
wtgctl start        # 전부 기동 (필요한 것만)
wtgctl stop         # mci-* + forwarder 종료. broker 컨테이너는 유지
wtgctl stop --all   # broker 컨테이너까지 종료
wtgctl status       # 한 줄 dump
wtgctl logs <name>  # api|push|admin|fwd|broker tail -f
wtgctl test tx      # /v1/tx echo 한 번
wtgctl test push    # /v1/admin/push-test 한 번
wtgctl quote SMB USDKRW 1380.5   # 단발 시세 발사
wtgctl burst start [pat]   # 시세 시뮬레이션 (walk|trend|downtrend|volatile|spike|calm)
wtgctl burst stop
wtgctl burst status

wtgctl alias              # 등록 alias 목록
wtgctl alias NAME [data]  # alias 호출
wtgctl routes add|del|set # cfg 파일 룰 관리 (hot reload)
```

자세한 사용법은 [mci-test-runbook.md](./mci-test-runbook.md) 참조.

---

## 12. 코드 위치 빠른 참조

| 위치                                             | 내용                                                                                                        |
| ---------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `pkg/mymq/`                                    | broker wire protocol Go 구현 (FrameInput, BroadcastHeader, Client.Open/Call/Send/Subscribe)                 |
| `pkg/metrics/`                                 | 공용 Prometheus metrics (HTTPMiddleware)                                                                    |
| `pkg/policy/`, `pkg/routing/`, `pkg/auth/`     | 정책/라우팅/인증 도메인                                                                                             |
| `internal/api/`                                | mci-api (server, config, middleware, handlers, transform)                                                 |
| `internal/admin/`                              | mci-admin (server, ui/index.html, handlers, routes, policy, audit_ring, stream, tx_proxy.go, pushtest.go) |
| `internal/push/`                               | mci-push (server, connection, dispatcher, registry, handlers, grpc)                                       |
| `internal/edge/api`, `edge/push`, `edge/price` | DMZ proxy 들                                                                                               |
| `pkg/svcio/`                                   | 매매 svc 헤더 파싱 (`win/src/inc/trn/*.h` → SvcSpec). I/O 명세 화면의 metadata source                                |
| `pkg/netutil/`                                 | CIDR allowlist 미들웨어 (edge 공통)                                                                              |
| `cmd/mci-*`                                    | 각 서비스 main                                                                                                |
| `cmd/quote-forwarder/main.go`                  | 시세 forwarder                                                                                              |
| `cmd/mci-test/main.go`                         | broker ckey echo 검증 CLI                                                                                   |

---

## 13. 한계 / 미해결 항목

- **echo_svc (svcmain 프레임워크)** 는 우리 dev cfg 에서 register 못 함 —
  운영환경 (NH 사내 라이브러리 + DB2 + KB SHM cfg) 의존. 대신 `WECHO`
  (svcmain 우회) 를 쓴다. svcmain 정상화는 Phase 7 운영기능 통합 단계 (B 옵션,
  `win/src/lib/com` 의 standalone 모드 작업) 에서.
- **broker errm 인코딩** 이 CP949/EUC-KR 인 채로 UTF-8 응답에 박혀 깨져 보일
  때가 있다 (cosmetic). errn 코드는 정확하므로 동작에는 영향 없음.
- **mymq lib 의 mq_error.c** 가 GNU `strerror_r` 의 결과를 stack-uninitialized
  buffer 에 의존하는 패턴. errm 에 stack 잔재 (system command 등) 가 leak 되어
  보일 수 있음. 진짜 errn 코드는 정확.
- mci-edge-* 와 mci-price 는 Phase 2~3 작업 진행 중. 현재 setup 에서는 옵션.

## 14. 진척 — 마지막 변경

### Phase 1~3 (이전 turns)
- WECHO (`win/src/trn/WECHO`) prototype 동작 완료, broker entrypoint 가 자동
  기동. 운영 svcmain 정상화 시 main wrapper 만 갈아끼우면 W3100 같은 형태.
- alias 자동 시드 (DevMode) — mci-admin + mci-api 양쪽이 같은 alias 를
  in-memory registry 에 시드.
- forwarder Reconnect + Prometheus metrics (`/metrics`, `/stats`).
- mci-push 의 dispatcher 카운터 `/v1/push-stats` + Prometheus 게이지.
- middleware 의 statusRecorder 에 `Hijack()` — ws upgrade 통과.
- `UserFromQuery` 미들웨어 — DevMode ws 인증 (`?x_wtg_user=...`).
- `Cache-Control: no-store` — UI 자산 캐시 부정합 방지.
- mci-push CheckOrigin DevMode 시 모두 허용.

### B 옵션 (win 환경 정상화)
- `win/src/lib/com/Makefile.standalone` → `libcom_min.a` (8 sources, 88KB).
  외부 사내 lib 의존성 0.
- `win/src/lib/main/biz_main_standalone.c` — 운영 biz_main 의 atcall(Oracle/DB2)
  부분만 noop, 그 외 동일. 신규 trn 서비스가 link 가능.
- `win/src/trn/WECHO` 가 com_min 의 `GetDateTime` / `APLog` 사용으로 운영
  helper 통합.
- `win/src/trn/WECHOSTD` (운영 svcmain 패턴) + `win/src/bat/logutl/logbackup`
  (실전 batch) 추가 빌드. svcmain runtime 동작은 broker cfg 의존 — Phase 7 으로
  미룸.
- broker `mq_error.c:50` 의 `METOOSHORT` 콤마 누락 수정 — errm 깨짐 해결.

### UI 효율화 Phase 2~5
- **Phase 2 (동적 alias preset)** — API 테스터 진입 시 `/v1/admin/routes` fetch
  + button 자동 생성. 정적 hardcode preset 에서 alias 항목 제거.
- **Phase 3 (라우팅 룰 deep link)** — entry 마다 `▶ 테스트` 버튼 → API 테스터로
  navigate + body 자동 채움.
- **Phase 4 (외부 cfg 파일 시드)** — `~/mymq/etc/wtg-routes.json` 에서 alias 시드
  로드. `--dev-routes-file=...` flag. 파일 부재 시 hardcode default fallback.
- **Phase 5 (cfg hot reload)** — `routing.WatchRoutesFilePolicy()` 이 mtime 2초 polling.
  cfg 변경 시 재기동 없이 자동 재시드. 정책 선택 가능: `additive` (UI 편집 보존,
  기본) / `sync` (cfg 가 진실의 원천 — `wtgctl routes del/set` 이 즉시 반영).
- **API 테스터 body localStorage 기억** — alias/preset 별 마지막 body 가
  `localStorage["wtg_tester_bodies"]` 에 보관됨. 같은 preset 을 다시 열면 자동
  복원되고, body 라벨 옆 "기억됨" 배지로 표시. 반복 호출 시 매번 입력 불필요.
- 시세 카드에 sparkline (Chart.js 없이 자체 canvas 직접 그리기).

### DMZ edge 통합
- 3 edge (api/push/price) 모두 `--allow-cidrs` IP 화이트리스트 지원
  (`pkg/netutil.IPAllowList`). chain 의 *가장 바깥* 에 wire 되어 auth/ratelimit
  이전에 즉시 거부 — 자원 절약.
- wtgctl `WTG_EDGE=1 wtgctl start` / `WTG_EDGE_ALLOW_CIDRS=...` / `wtgctl edge
  {start|stop|status}`. dev-stack 에서 DMZ 시뮬 한 줄 기동.

### 정책 split-brain 해결
- `pkg/policy/httpsync.go` — DevMode 에서 mci-api 가 mci-admin 의 `GET
  /v1/admin/policy` 를 2 초 주기로 poll → `Engine.ApplyRemote`. wtgctl 의
  `cmd_start` 에서 `--dev-policy-url=http://127.0.0.1:9090/v1/admin/policy` 자동
  주입. 운영은 etcd. kill switch / blocked symbols 가 admin↔api 양쪽에
  실시간 일치.

### svc I/O 명세 화면 — Phase 3-A/B/C/D 완료
- **3-A (파서)**: `pkg/svcio/spec.go` — `win/src/inc/trn/*.h` 를 `SvcSpec{Code,
  Name, Input[]Field, Output[]Field, Records}` 로 파싱. CP949 자동 변환,
  Program Name 한글 주석 추출, 인라인 nested struct + named record (`_R`) 자동
  resolve. 단위 테스트 3 건 + 실측 830 헤더 일괄 **98.7% 파싱 성공** (819 OK,
  11 fail — typedef 부재 placeholder 또는 syntactic typo).
- **3-B (registry + endpoint)**: `pkg/svcio/registry.go` — in-memory 인덱스
  (code → SvcSpec). 부팅 시 `LoadDir()`. mci-admin 의 `--svc-inc-dir` flag /
  `WTG_ADMIN_SVC_INC_DIR` env 로 디렉터리 주입. 두 endpoint:
  - `GET /v1/admin/svc-io[?q=KEY&max=N]` — code/한글이름 prefix 검색
  - `GET /v1/admin/svc-io/{code}` — 단건 SvcSpec JSON
- **3-C (UI 화면)**: 사이드바 "서비스 명세". 좌측 검색 + 목록, 우측 Code/Name
  헤더 + Input/Output 트리 (depth indent + 한글 comment + size). 라우팅 룰
  store cross-ref 로 *이 svc 를 가리키는 alias* 자동 표시.
- **3-D (호출 형식 + 채널별 gen)**: 두 axis 가 직교한다.

  *형식 axis* (호출 형태 결정):
  | 형식 | envelope | 실행 endpoint | 직렬화 위치 |
  |---|---|---|---|
  | `API` (기본) | `POST /v1/tx` + JSON | mci-api `/v1/tx` | 클라이언트 (JSON 그대로) |
  | `WIRE` (전문) | broker wire frame | mci-admin `/v1/admin/svc-io/{code}/test-wire` | **mci 서버 측** (Input layout 으로 byte 직렬화) |

  WIRE 형식의 핵심: *클라이언트는 REST 호출* 하지만 mci 가 *서버 측에서* wire
  frame 직렬화 → broker 송신. broker 는 legacy native client 와 동일한 byte
  수신, 클라이언트는 mci 의 정책/감사 layer 를 그대로 통과. 검증 가치 (실제
  legacy 동작 재현) 와 운영 안정성 (정책/감사 우회 X) 양쪽 다 챙김.

  `pkg/svcio.SerializeWithHeader/DeserializeWithHeader` — `[COMHDR][TX_BODY]`
  복합 직렬화. char[N] + CP949 + 우측 공백 fill + 가변 grid (직전 *_cnt 필드
  ASCII int 로 횟수 결정). 단위 테스트 + W1104S01 실 헤더 round-trip 검증.
  1차 prototype 은 char 외 타입 미지원 (실측 830 헤더의 압도적 다수).

  **공통 헤더 (header + body 구조)** — 운영 svc 는 `[COMHDR(256)][TX_BODY]`,
  dev svc 는 raw body. mci-admin 부팅 시 `--svc-common-header=comhdr.h` 로
  COMHDR/BROADCAST_H/CHGHDR 등 named header 등록. 디렉터리별 default
  HeaderType + spec 별 `@wtg-header: NAME` 주석 override 지원.

  W1104S01 검증 결과: `sent=394 byte` (256+138), `header_type=COMHDR` —
  legacy native client 가 보낼 wire 와 정확히 동일.

  *채널 axis* (API 형식에서만 의미):
  채널 5종 (pkg/mymq.ChannelCode 와 정렬). 두 axis 가 직교한다 — *사용자
  권한* 과 *기술 framework*:

  |  | 외부 (고객권) | 내부 (직원권) |
  |---|---|---|
  | web/native | `WEB` (브라우저) / `MOB` (모바일) | `ADM` (운영 콘솔) |
  | cs-native | `HTS` (고객 desktop) | `EMP` (딜러 desktop) |

  HTS / EMP 는 같은 cs framework (전통 native desktop) 위에 있지만 사용자가
  정반대 — HTS=고객, EMP=딜러. 노출 위치도 다르다 (HTS=DMZ edge, EMP=Internal
  직결). 권한 등급/정책 적용 범위도 다르다 (예: kill switch ON 시 HTS 차단,
  EMP 비상 거래 허용).

  | 채널 | 사용자 | URL prefix | 인증 | 표준 client |
  |---|---|---|---|---|
  | `WEB` | 고객 | mci-edge-api (DMZ) | JWT (Authorization) | 브라우저 fetch |
  | `MOB` | 고객 | mci-edge-api (DMZ) | JWT (앱 keychain) | Swift / Kotlin |
  | `HTS` | 고객 | mci-edge-api (DMZ) | 고객 SSO / JWT | cs native desktop |
  | `ADM` | 직원 | mci-admin :9090 | 직원 SSO / X-WTG-User | UI fetch / curl |
  | `EMP` | 딜러/직원 | mci-api :8080 직결 | 직원 SSO / X-WTG-User | cs native desktop |

  그룹 헬퍼:
  - `ChannelCode.IsCustomer()` — WEB/MOB/HTS (고객 정책 적용 범위)
  - `ChannelCode.IsEmployee()` — ADM/EMP (운영/딜러 권한)
  - `ChannelCode.IsCSFramework()` — HTS/EMP (같은 cs 기술 — wire 정책/rate
    limit 등 *기술 차원* 룰을 같이 적용. 권한 그룹 아님)

  새 채널 추가 시 `SVCIO_GEN` 에 key 한 줄 + mymq 상수 한 줄 — data layer 와
  완전 분리되어 open/closed.

wtgctl 의 `cmd_start` 가 `--svc-inc-dir=$HOME/mywork/win/src/inc/trn` 자동
주입. `WTG_SVC_INC_DIR` env 로 override.
