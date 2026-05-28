# WTG (Winway Trading Gateway) 코드베이스 구조 리뷰

> 검토일: 2026-05-23 · 검토 대상: `/Users/winwaysystems/mywork/wtg` · 검토 방식: 정적 소스 리뷰 (read-only)
> 검토 범위: 구조/아키텍처 개요, 핵심 메시지 흐름 추적, 코드 품질 및 개선점

---

## 1. 검토 개요

WTG 는 기존 C 기반 MyMQ 매매 엔진 앞단에 놓이는 **Go 기반 web 게이트웨이**다. 웹/FIX/CS 채널을 단일 broker(`mymqd`)로 정규화하며, C 엔진을 무수정으로 두고 순수 Go 로 wire protocol 을 재구현(no cgo)한 것이 핵심 설계다.

### 규모

| 영역 | 소스 파일 | LOC | 테스트 파일 |
|------|----------|-----|------------|
| `pkg/` (공유 라이브러리) | 42 | 8,465 | 34 |
| `internal/` (서비스 로직) | 56 | 9,825 | 34 |
| `cmd/` (entrypoint) | 9 | 1,137 | 0 |
| `test/` | 1 | 81 | 1 |
| **합계** | **~108** | **~19,500** | **69** |

전체 Go 파일 177개. 의존성은 표준 라이브러리 중심이며 외부 의존은 `gorilla/websocket`, `prometheus/client_golang`, `etcd v3`, `grpc` 정도로 절제되어 있다. JWT 도 외부 라이브러리 없이 `crypto/rsa` 로 직접 구현했다.

> **참고:** 작업 환경에 Go 툴체인이 없어 `go build` / `go test` / `go vet` 컴파일 검증은 수행하지 못했다. 본 리뷰는 소스 정독 기반의 정적 분석이다.

---

## 2. 아키텍처 개요

### 2.1 레이어링

디렉토리 구조가 C 엔진(`mymq`)의 레이아웃을 의도적으로 미러링한다.

```
pkg/        공유 라이브러리 (= mymq/src/lib/)     — 재사용 가능, 서비스 비의존
internal/   서비스별 비즈니스 로직                 — api / admin / push / price / edge
cmd/        서비스 entrypoint (= mymq/src/{mqd,...}) — 얇은 main()
api/proto/  .proto 원본 → pkg/wtgpb/v1 로 생성
test/       실 broker 통합 테스트 + embedded etcd helper
docs/       11개 명세 문서
```

레이어 의존 방향이 단방향(`cmd → internal → pkg`)으로 깔끔하게 유지된다. `pkg` 는 서비스 패키지를 import 하지 않고, `internal` 끼리의 교차 참조도 최소화되어 있다(`internal/admin` 이 `internal/api/handlers` 의 login 핸들러를 재사용하는 정도).

### 2.2 컴포넌트 맵

```
                  ┌─────────── DMZ ───────────┐   ┌──────────── Internal ────────────┐
  외부 클라이언트 → │ mci-edge-api  (8090)       │ → │ mci-api    (8080) ChannelWeb     │ ┐
                  │ mci-edge-push (8084)       │   │ mci-admin  (9090) ChannelAdmin   │ │
                  │ mci-edge-price(8083)       │   │ mci-push   (8081) ChannelWeb(rep)│ ├→ mymqd
                  └────────────────────────────┘   │ mci-price  (TBD)  ChannelWeb     │ │  (11217)
                                                    │ quote-forwarder   ChannelAdmin   │ ┘
                                                    └───────────────────────────────────┘
```

- **DMZ 계층** (`mci-edge-*`): TLS 종단, JWT 검증(public key 만 보유), IP 화이트리스트, rate-limit, 헤더 sanitization. broker 를 직접 호출하지 않고 `httputil.ReverseProxy` 또는 gRPC 로 Internal 계층에 위임.
- **Internal 계층**: 실제 broker 호출. cookie_t/세션 같은 비밀은 이 계층에만 둔다.
- **공통 채널**: `etcd` 가 라우팅 룰·정책을 공유(watch 동기화), `wtgpb` gRPC 가 edge↔internal 내부 채널.

### 2.3 핵심 설계 원칙 (코드에서 일관되게 관철됨)

1. **C 엔진 무수정** — broker/AP 를 그대로 두고 WTG 는 mymq client 로만 붙는다.
2. **Go-native, no cgo** — `pkg/mymq` 가 wire protocol 전체를 순수 Go 로 재구현.
3. **단일 connection 멀티플렉싱** — `mqhdr.ckey` 를 correlation_id 로 써서 한 TCP 연결로 동시 RPC.
4. **passthrough 패턴** — transaction별 핸들러를 만들지 않고 generic envelope 1개로 모든 매매 transaction 을 통과시킨다.
5. **인증/권한 분리** — Authentication(누구인가)만 WTG, Authorization(무엇을 할 수 있나)은 매매 엔진에 위임.

이 원칙들이 주석·패키지 설계·핸들러 구현에 빠짐없이 반영되어 있다. 특히 5번(권한 위임)은 `policy` 패키지가 "비즈니스 거부"가 아닌 "운영 차단"만 다루도록 책임 범위를 명확히 그어놓은 점이 인상적이다.

---

## 3. 핵심 메시지 흐름 추적

### 3.1 Transaction (동기 RPC) — `POST /v1/tx`

```
클라이언트 → [mci-edge-api] → mci-api
  └ 미들웨어 체인 (server.go:216) : Recover → RequestID → AccessLog → metrics → Auth
  └ handlers.Transaction (transaction.go:45)
       1. principalRequired()           — Auth 미들웨어가 심은 Principal 추출
       2. JSON Envelope 디코딩 + ValidateRequest()  — transport 수준 검증만
       3. deps.Policy.Check()           — kill switch / 정비창 / 차단 심볼·rkey
       4. env.BuildFrame()              — alias → routing.Resolve → exchange/rkey 치환
       5. frame.Cookie = p.Cookie       — SessionMode 시 cookie_t 첨부
       6. deps.MQ.Call(ctx, frame)      — mymq.Client 동기 호출
       7. transform.FromReply()         — 응답 envelope 으로 raw passthrough
```

`mymq.Client.Call` (client.go:651) 내부:
- `nextCorrelation()` 으로 0 이 아닌 ckey 발급 → `pending` sync.Map 에 reply 채널 등록
- `applyDefaults()` 가 channel 코드와 navi(origin/destination)를 자동 채움
- `writeFrame()` 송신 후 reply 채널 / ctx.Done / connection-done 에서 select 대기
- broker 가 ckey 를 echo back → `readLoop` → `dispatch` (client.go:580) 가 `pending.LoadAndDelete(ckey)` 로 정확히 매칭

이 ckey 멀티플렉싱 덕분에 다수의 동시 `Call()` 이 단일 TCP 연결을 안전하게 공유한다. 설계의 핵심이며 구현도 견고하다.

### 3.2 Push (unsolicited fan-out)

```
broker publish → mci-push (representative receiver) → ws 사용자
```

- `mci-push` 가 `mymq.Open` 시 `Queue{Flags: QfUnsolMsg|QfUnsolHdr|QfUnsolRep}` 로 connect (push/server.go:59) → broker 가 "대표 수신자"로 등록해 user 매칭 없이 모든 publish 를 흘려준다.
- `Dispatcher.Run` (dispatcher.go:73) 이 `mymq.Client.Subscribe()` 채널을 **단일 goroutine** 으로 소비.
- `handle()`: `FCCast/FCPush/FCSignal` 만 필터 → `buildEnvelope()` (JSON) → broadcast prefix 의 `LogonID` 가 있으면 `FanoutToUser`, 없으면 `FanoutBroadcast`.
- `Registry` (push/registry.go) 가 `logon_id → []*Connection` 매핑 → `Connection.Send()` → send 채널 → `writeLoop` → `ws.WriteMessage`.
- **slow consumer 격리**: send 큐(기본 256)가 가득 차면 해당 Connection 만 Close 해서 다른 사용자에게 영향이 번지지 않게 한다. 좋은 격리 설계.

### 3.3 시세 (UDP → broker broadcast → ws)

```
replay_smb2/kmb2/ebs2 ─UDP→ quote-forwarder ─FCCast/SubBroadcast→ broker ─→ mci-push ─→ ws
```

- `quote-forwarder/main.go` 가 feed별 UDP listener goroutine 으로 패킷 수신.
- `parseQuote()` 가 FIX 4.4 의 Market Data Snapshot(35=W) / Incremental Refresh(35=X) 를 파싱 — repeating group(269/279)을 flat 하게 처리.
- `quoteEnvelope` JSON 으로 변환 후 `publishBroadcast()`: 80바이트 broadcast prefix(`LogonID` 빈값) + payload 를 `FCCast/SubBroadcast` 로 발행.
- `LogonID` 가 비어 있으므로 `mci-push` 의 Dispatcher 가 `FanoutBroadcast` 로 전체 ws 사용자에게 fan-out.
- `mci-price` 는 별도로 `ChannelWeb` 큐로 구독해 conflation(시세 합성) + gRPC stream 을 제공.

### 3.4 인증 흐름

```
로그인:  POST /v1/login (인증 우회 경로) → broker LOGON 트랜잭션
        → reply.Cookie 추출 → auth.Session 저장 → JWT(RS256) Sign + RefreshToken 발급
후속요청: Auth 미들웨어 → authenticateJWT → Verifier.Verify → SessionStore.Get(SID)
        → cookie_t 복원 → Principal (context 주입)
edge:   stripIngressHeaders() 로 외부 X-WTG-* 제거 → 검증된 Principal 만 헤더 재주입
        → mci-api 가 TrustEdgeHeaders 모드로 헤더 신뢰
```

인증 모드가 우선순위로 명확히 계층화되어 있다(`auth.go:71` AuthConfig 주석): DevMode → TrustEdgeHeaders → JWT → raw session_id → 401. edge 서버의 `stripIngressHeaders` (edge/api/server.go:345)가 외부에서 들어온 신뢰 헤더(`X-WTG-User/SID/Channel`)와 `Authorization`/`Cookie` 를 무조건 제거한 뒤 edge 가 검증한 값만 재주입하는 점은 헤더 위조 우회를 막는 핵심 방어선이며 올바르게 구현되어 있다.

---

## 4. 패키지별 관찰

### `pkg/mymq` — wire protocol + Client
프로젝트의 심장. 84B 고정 헤더(`mqhdr_t`) + navi[] + 가변 영역(ERRM/PKEY/NKEY/COOKIE/BODY)을 BE network byte order 로 인코딩/디코딩한다. 오프셋 상수가 frame.go 상단에 wire 레이아웃 주석과 함께 명시되어 있어 C 헤더와 대조하기 쉽다. `MakeClid`/`SplitClid` 에서 원본 C 매크로의 비트 오버랩 버그(ncid 6bit→5bit)를 의식적으로 고치고 그 결정을 주석으로 남긴 점이 돋보인다. reconnect supervisor, heartbeat watchdog(2×interval 무수신 시 사망 판정), 압축 자동 해제 등 운영 기능이 충실하다.

### `pkg/auth` — JWT / 세션 / refresh
RS256 JWT 를 표준 crypto 로 직접 구현. `alg` 를 RS256 으로 강제 검증해 `alg=none`·HS/RS confusion 공격을 차단한다. refresh token 은 single-use(Consume = 조회+삭제) 로 replay 를 방어. 다만 **저장소가 in-memory 구현(`MemoryStore`/`MemoryRefreshStore`)뿐**이고 Redis 는 미구현 — 4.2의 High 항목 참조.

### `pkg/routing`, `pkg/policy`
둘 다 인터페이스(`Registry`) + in-memory 구현 + etcd 동기화 조합. `policy.Engine` 의 `notifyPrepLocked` 패턴 — 락 안에서 콜백 목록·스냅샷만 준비하고 락 해제 후 콜백을 fire — 은 콜백 재진입 deadlock 을 막는 모범적 처리다. kill switch 를 채널별 scope 로 적용할 수 있게 한 점(`SetKillSwitchScoped`)도 실무적이다.

### `internal/*` 서비스
5개 서비스가 모두 `NewServer → Start(ctx) → Shutdown(ctx)` 동일 라이프사이클 패턴을 따른다. TLS reloader(SIGHUP + mtime polling), graceful shutdown, Prometheus metrics 가 일관되게 적용된다. 핸들러는 struct 대신 `함수형 + Deps 주입` 패턴으로 wire-up 이 단순하다.

---

## 5. 코드 품질 평가

### 5.1 강점

- **문서화 수준이 매우 높다.** 거의 모든 패키지·타입·비자명 함수에 *왜* 그렇게 했는지를 설명하는 한글 주석이 붙어 있다. wire protocol 처럼 검증이 어려운 영역일수록 C 원본과의 대응 관계를 명시해 두어 유지보수 위험을 크게 낮춘다.
- **동시성 처리가 견고하다.** ckey 멀티플렉싱, atomic 카운터, `connMu`/`writeMu` 분리, 락 밖 콜백 fire 패턴, slow-consumer 격리 등 동시성 함정을 의식한 흔적이 일관적이다.
- **테스트가 따라온다.** `pkg`·`internal` 모두 소스 대비 테스트 파일 비율이 높고(각각 34개), fake broker·embedded etcd helper 까지 갖춰 broker 없이도 CI 가 green 이 되도록 설계됐다.
- **설계 원칙이 코드에 일관 반영.** passthrough·권한 위임·DMZ 분리 같은 원칙이 문서뿐 아니라 실제 핸들러·패키지 경계에서 지켜진다.
- **보안 의식.** edge 헤더 sanitization, JWT alg 강제, public/private key 분리 배치, TLS hot-reload, IP allowlist.

### 5.2 개선점 (우선순위별)

#### 🔴 높음

**(H-1) 세션 저장소가 in-memory 전용 — 수평 확장 블로커.**
`auth.Store`/`auth.RefreshStore` 의 운영 구현(Redis)이 아직 없다. 현재는 `mci-api` 인스턴스마다 독립된 `MemoryStore` 를 갖는다(`internal/api/server.go:122`). `etcd` 동기화는 라우팅 룰·정책만 공유하고 **세션은 공유하지 않는다.** 따라서 인스턴스 A 에서 로그인한 사용자의 후속 요청이 인스턴스 B 로 라우팅되면 세션 조회에 실패한다. CLAUDE.md 와 `ApplName` 다중 인스턴스 컨벤션(`mci-api-01`...)이 전제하는 수평 확장이 사실상 불가능한 상태다. 코드상 인터페이스는 이미 분리되어 있으므로(`auth.md §7` 차환 계획), Redis 구현 추가가 운영 진입의 선결 과제다.

**(H-2) `quote-forwarder` 의 FIX 파서가 테스트되지 않음.**
`cmd/` 디렉토리 전체에 테스트 파일이 0개다. 대부분의 `main.go` 는 얇지만, `quote-forwarder/main.go` 는 `parseQuote`/`fixFields`/repeating-group 처리 등 **실수하기 쉬운 FIX 4.4 파싱 로직 ~160줄을 `main` 패키지 안에 담고 있고 단위 테스트가 없다.** 35=W 와 35=X 의 entry 시작 태그가 다르고(269 vs 279), 270/271 누적 시점이 미묘하다. 이 로직을 `internal/` 패키지로 분리해 테스트 가능하게 만들거나, 최소한 `main_test.go` 로 대표 FIX 메시지 픽스처를 검증할 것을 권장한다.

#### 🟡 중간

**(M-1) `mymq.Client.dispatch` 의 unsolicited drop 이 무계측.**
`subCh` 버퍼(256)가 가득 차면 unsolicited 메시지를 조용히 버린다(`client.go:622` `default:` 분기, `// TODO 향후 metric 카운터`). `mci-push` 의 Dispatcher 는 `subCh` 를 단일 goroutine 으로 소비하므로, Registry 락 경합이나 fan-out 지연으로 소비가 느려지면 시세·체결 메시지가 **소실되어도 가시화되지 않는다.** 매매 게이트웨이에서 데이터 손실 무계측은 위험하다. 최소한 drop 카운터(Prometheus)를 추가할 것.

**(M-2) reconnect 소진 경로의 `subCh` double-close 잠재 버그.**
`reconnect.go:98`·`:107` 에서 supervisor 가 `c.closed.Store(true)` 후 `close(c.subCh)` 를 **무조건** 호출하는데, 같은 채널을 `Client.Close()` (`client.go:336`)도 닫는다. `Close()` 는 `closed.Swap` 가드가 있지만 supervisor 분기는 가드가 없다. 사용자가 `Close()` 로 먼저 `subCh` 를 닫은 직후 supervisor 가 소진 분기에 도달하면 `close of closed channel` panic 이 발생한다. **현재는 모든 서비스가 `ReconnectOptions.MaxAttempts` 를 설정하지 않아(0=무제한) 소진 분기가 도달 불가능하므로 잠재적 버그**지만, 누군가 `MaxAttempts>0` 을 설정하는 순간 실제 패닉이 된다. `subCh` 종료를 `sync.Once` 로 감싸는 것이 안전하다.

**(M-3) wire 레벨 채널 코드가 서비스별 고정 — MOB/HTS 가 반영 안 됨.**
`conventions.go` 는 WEB/MOB/HTS/ADM/EMP 등 풍부한 채널 분류를 정의하지만, 각 서비스는 `mymq.Open` 시 채널을 하나로 고정한다(`mci-api` 는 `ChannelWeb` — `internal/api/server.go:103`). `Options.Channel` 은 모든 송신 프레임의 `mqhdr.chan[4]` 에 자동 첨부되므로, 모바일/HTS 사용자가 `mci-api` 를 거치면 broker 측 감사·라우팅이 보는 채널은 항상 `WEB` 가 된다. 사용자별 실제 채널(`Principal.Channel`)은 WTG 세션/JWT 안에만 존재한다. broker 측 채널 귀속이 중요하다면 프레임별로 `FrameInput.Chan` 을 Principal 채널로 채우는 경로가 필요하다. 현재는 채널 taxonomy 의 표현력과 실제 wire 사용 사이에 간극이 있다.

#### 🟢 낮음

- **(L-1) `extractSymbol` 의 이중 JSON 파싱.** `/v1/tx` 매 요청마다 `data` 페이로드를 `json.Unmarshal` 해 `symbol` 만 뽑는다(`transaction.go:20`). 매매 엔진이 같은 페이로드를 또 파싱한다. `BlockedSymbols` 가 비어 있을 때는 건너뛰거나 토큰 스캐닝으로 바꾸면 고빈도 주문 경로의 할당을 줄일 수 있다.
- **(L-2) 데드코드 import 유지 핵.** `internal/edge/api/server.go:387` 의 `var _ = strings.Repeat` 는 미사용 import 를 억지로 살리는 흔적이다. `strings` import 를 제거하는 게 맞다.
- **(L-3) stale 주석.** `internal/api/middleware/auth.go:93` 의 "Phase 2 단계 ... RS256/Redis 통합은 미구현" 주석은 현재와 어긋난다 — RS256 JWT 검증은 `jwt.go` 에 완전히 구현·배선되어 있다(Redis 만 미구현). 주석 갱신 필요.
- **(L-4) 단일 git 커밋 + 대량 미커밋 변경.** 전체 코드베이스가 `초기 import` 커밋 1개이며, working tree 에는 다수의 modified 파일과 미추적 파일(`internal/admin/admin_*.go` 일체, `pkg/pricing/` 등)이 쌓여 있다. 변경 이력 추적·리뷰가 불가능한 상태이므로 의미 단위 커밋 분할을 권장한다.
- **(L-5) `quote-forwarder` 의 msgtype substring 프로브.** `buildEnvelopeWithStatus` 가 JSON 을 직렬화한 뒤 `bytes.Contains` 로 `"msgtype":"snapshot"` 를 찾아 파싱 성공 여부를 판정한다(`main.go:295`). `parseQuote` 가 이미 구조체를 반환하므로 `buildEnvelope` 가 `(bytes, msgtype)` 를 직접 돌려주면 더 깔끔하다.

---

## 6. 종합 의견

WTG 는 **설계 의도가 코드 전반에 일관되게 관철된, 완성도 높은 코드베이스**다. C wire protocol 의 순수 Go 재구현, ckey 멀티플렉싱, DMZ/Internal 분리, 권한 위임 원칙 같은 어려운 결정들이 문서·주석·패키지 경계·핸들러 구현에서 모두 같은 방향을 가리킨다. 동시성 처리와 보안 의식, 테스트 인프라(fake broker / embedded etcd)도 프로토타입 수준을 넘어선다.

운영 진입 관점에서 가장 중요한 한 가지는 **세션 저장소의 Redis 구현(H-1)** 이다. 이것이 없으면 문서가 전제하는 다중 인스턴스 운영이 성립하지 않는다. 그다음으로 **FIX 파서 테스트(H-2)** 와 **unsolicited drop 계측(M-1)** 이 데이터 무결성·가시성 측면에서 우선순위가 높다. 나머지 중·낮음 항목들은 점진적으로 정리하면 된다.

전반적으로 구조적 결함이라기보다 "프로토타입에서 운영으로 넘어가며 채워야 할 칸"들이 명확하게 남아 있는 상태로 보이며, 그 칸들이 인터페이스로 미리 분리되어 있어 차환 비용도 낮다.

---

*이 문서는 정적 소스 리뷰 결과이며 컴파일·테스트 실행 검증은 포함하지 않는다. 인용한 파일:라인 번호는 검토 시점(2026-05-23) 기준이다.*
