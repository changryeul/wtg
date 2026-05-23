# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 프로젝트 개요

**WTG (Winway Trading Gateway)** — 기존 C 기반 MyMQ 매매 엔진(`/Users/winwaysystems/mywork/mymq`) 앞단의 Go 기반 web 게이트웨이. 웹/FIX/CS 채널을 단일 broker(`mymqd`) 로 정규화한다.

핵심 설계 원칙:
- **C 엔진 무수정** — broker(`mymqd`) 와 매매 AP 는 그대로 두고, WTG 는 mymq client 로만 붙는다.
- **Go-native, no cgo** — `pkg/mymq` 가 wire protocol(`mymq.h` / `mq.h`) 을 순수 Go 로 재구현 (84B mqhdr + navi[] + 가변영역, BE network byte order, 4B length-prefix framing, 4B 빈 프레임 heartbeat).
- **단일 connection 멀티플렉싱** — `mqhdr.ckey` 를 correlation_id 로 사용해 동시 RPC 처리.
- **DMZ ↔ Internal 분리** — 외부 트래픽은 `mci-edge-*` (DMZ) 만 거치고, 내부 도구도 `mci-api` 경유 권장 (정책/감사 일관성).

## 빌드 / 테스트 명령

```bash
make build              # cmd/<svc>/*.go 자동 발견 → build/bin/<svc>
make test               # 단위 테스트 (broker 불필요)
make test-race          # -race + coverage.out
make test-integration   # build tag=integration, embedded etcd 사용 (~30s)
make coverage           # coverage.html 생성
make lint               # fmt-check + vet + staticcheck
make vulncheck          # govulncheck
make ci                 # CI 와 동일한 전체 검증 (commit/PR 전 권장)
make ckey-echo          # Phase 1 GO/NO-GO — mymqd 가 ckey echo back 하는지
make proto              # api/proto/*.proto → pkg/wtgpb/v1/*.pb.go
make install            # build/bin/* → $(BINDIR), etc/* → $(ETCDIR) (PREFIX=/opt/wtg 기본)
```

`cmd/<service>/main.go` 추가 시 자동으로 빌드 대상에 포함된다 (Makefile 가 `cmd/*/*.go` glob).

### 단일 테스트 실행

```bash
go test ./pkg/mymq/ -run TestCkeyEcho -v
go test ./internal/api/handlers/ -run TestLogin -race
```

### 실 broker 통합 테스트

`MYMQD_HOST` 미설정이면 자동 skip — CI 는 broker 없이도 green.

```bash
MYMQD_HOST=10.0.0.10 MYMQD_PORT=11217 go test -v ./test/integration/...
```

### UI / DevMode (broker 없이 5분 안에 시각 확인)

```bash
./build/bin/mci-admin --dev --no-broker --listen :9090
# 로그인 화면 → "개발 모드로 진입" → ID 입력 → "ID 만으로 입장 (DevMode)"
# DevMode 는 X-WTG-User 헤더만 신뢰 (broker 호출 우회)
```

## 컴포넌트 구조

| 영역 | 바이너리 | 포트 | mymq channel | 책임 |
|-----|---------|-----|--------------|-----|
| DMZ | `mci-edge-api` | 8090 | (HTTP only) | TLS termination + JWT 검증 + IP 화이트리스트 + rate-limit |
| DMZ | `mci-edge-push` | 8084 | (gRPC client) | mci-push PushService → 외부 ws fan-out |
| DMZ | `mci-edge-price` | 8083 | (gRPC client) | raw tick broadcast + Profile 별 quote stream (SubscribeQuote) |
| Internal | `mci-api` | 8080 | `ChannelWeb` | `/v1/tx` `/v1/login` (Site/Tier → Session.Profile + JWT) `/v1/refresh` |
| Internal | `mci-admin` | 9090 | `ChannelAdmin` | UI + 라우팅/정책 관리 + symbols/pricing/profiles CRUD + audit ring |
| Internal | `mci-push` | 8081 | `ChannelWeb` (rep) | broker unsolicited 수신 → 사용자별 ws fan-out |
| Internal | `mci-price` | 8082 (HTTP) / 50051 (gRPC) | `ChannelWeb` | 시세 fan-out + Aggregator (OHLC 봉) + PricingConsumer (Profile 별 마진) + SubscribeBar gRPC stream |
| Internal | `mci-chart` | 8086 | (gRPC client) | TimescaleDB historical 봉 REST + ws 라이브 봉 (SubscribeBar) |
| Internal | `quote-forwarder` | UDP 30044~30051 | `ChannelAdmin` | UDP FIX 4.4 시세 → broker broadcast publish |
| 검증 | `mci-test` | — | — | Phase 1 ckey echo 검증 CLI |

mymqd (broker) 는 11217, cluster 11218. `ApplName` 컨벤션과 채널/exchange/routing-key 카탈로그는 `docs/conventions.md` + `pkg/mymq/conventions.go` 가 단일 출처 — 변경 시 양쪽 동기화.

## 디렉토리 매핑

```
pkg/                     # 공유 라이브러리 (단방향 DAG, 도메인 layer)
  session/               # Channel/Site/Tier/Profile/Pair/LogonID 도메인 enum (leaf)
  quote/                 # Quote, RingBuffer, SymbolMap, Bar/Timeframe + JSON envelope
                         # + etcd_symbols.go (EtcdSymbolWatcher)
  pricing/               # PricingTable (atomic snapshot) + Apply + JSON codec
                         # + etcd.go (EtcdTableWatcher)
  mymq/                  # libmymq-go: wire protocol + Client + ckey 멀티플렉싱
                         # codec / frame / handshake / reconnect / pubsub / conventions
                         # Quote/Bar 관련 exchange + routing-key 상수
  auth/                  # JWT (Claims.Site/Tier) + Session.Profile + Memory/RedisStore
  policy/                # kill switch / 정비창 / 차단 심볼 (etcd watch, TLS)
  routing/               # transaction alias → exchange/routing_key (etcd watch, TLS)
  config/ log/ metrics/ netutil/ ratelimit/ svcio/ tlsutil/
  proto/ wtgpb/          # gRPC (admin↔mci-api, edge↔internal, chart↔price)
cmd/                     # 서비스 entrypoint
  mci-{api,push,price,chart,admin,test} mci-edge-{api,push,price} quote-forwarder
internal/                # 서비스별 비즈니스 로직
  price/                 # Server + Aggregator + Archiver (pgx → TimescaleDB)
                         # + PricingConsumer + JSONCookerDecoder + GRPCServer
                         # + EtcdProfileSource (etcd watch)
  chart/                 # REST (Repository.QueryBars) + WS Hub + SubscribeBar 수신
  admin/                 # 운영 콘솔 — 라우팅/정책/symbols/pricing/profiles CRUD
  api/ push/ edge/{api,price,push}
api/proto/               # .proto 원본 (price.proto: SubscribeQuote / SubscribeBar)
test/integration/        # 실 mymqd 통합 테스트
test/etcdtest/           # embedded etcd helper (integration build tag)
docs/                    # 명세 (auth/conventions/architecture/roadmap/chart/cooker-quote/...)
etc/                     # 운영 설정 (symbols.json, profiles.json, pricing.json, sql/, ...)
```

## 메시지 흐름 — 알아야 할 핵심 5가지

### 1. Transaction (sync RPC) — `POST /v1/tx`
**transaction별 핸들러를 만들지 말 것.** generic envelope 1개로 모든 매매 transaction 을 broker 에 통과시킨다:
```json
{"alias":"WECHO_PING","data":""}                          // alias 기반 (권장)
{"exchange":"ECHOSVC","routing_key":"PING","data":""}     // raw envelope
```
`alias` 는 `mci-admin` 의 라우팅 룰 store(etcd, watch 동기화)에서 `exchange/routing_key` 로 resolve. broker 가 ckey 를 응답에 echo back 하므로 단일 connection 으로 동시 호출 가능.

### 2. Push (unsolicited fan-out)
`mci-push` 는 `QF_UNSOL_REP` flag 로 broker 의 representative receiver 에 등록 → user 매칭 없이 모든 publish 수신. 큐 이름이 **빈값** 이어야 broker 가 `_CLIENT_` type 으로 등록한다 (`publish.c:185-189` 참조). broadcast prefix 80B 의 `LogonID` 로 fan-out target 을 결정 — 빈값이면 전체 broadcast.

### 3. 시세 raw (UDP → broker broadcast → mci-price)
`quote-forwarder` 가 UDP FIX 4.4 (35=W/X) 파싱 → JSON envelope publish (`FCCast/SubBroadcast`, LogonID="") → `mci-price` 가 unsolicited 수신.
또는 cooker 가 직접 `pushdata.msgb` 에 v1 평면 envelope (docs/cooker-quote-schema.md) 으로 publish — 동일 path 로 흡수.

### 4. 마진 적용 + 라이브 quote (mci-price → mci-edge-price → 고객 ws)
- `Aggregator` 가 tick 을 6 timeframe (1s/1m/5m/15m/1h/1d) OHLC 봉 누적 (UTC bucket)
- `PricingConsumer` 가 동일 tick 에 `PricingTable.Apply` (Profile별 마진) → `MultiQuotePublisher` 로 fan-out:
  - broker `ExchangeQuote` (TOPIC, routing-key=Profile.Key())
  - **gRPC `PriceService.SubscribeQuote`** stream (mci-edge-price 가 소비)
- `mci-edge-price` 의 ws 클라이언트는 로그인 시 결정된 `Principal.ProfileKey()` (JWT claim 출처) 로 자기 Profile 의 quote 만 수신

### 5. 봉 영속 + 라이브 챠트 (mci-price → TimescaleDB + mci-chart → ws)
- `Aggregator.onClose` 가 봉 close 시 fan-out:
  - `Archiver` → pgx batch INSERT → `TimescaleDB.quote_bars` (1m+ 만 영속, 1s 는 메모리)
  - **gRPC `PriceService.SubscribeBar`** stream → `mci-chart`
- `mci-chart` REST `GET /v1/chart` 는 historical, WS `/v1/chart/stream` 는 라이브 (pair, tf 필터)
- **운영 카탈로그 hot reload**: `mci-admin` 이 etcd 에 PricingTable/SymbolMap/Profile/SymbolEntry write → 모든 `mci-price` 인스턴스가 watch 로 즉시 반영 (재배포 X)

## 인증/권한 분담

> **Authentication (사용자가 누구인가)** — WTG 가 처리.
> **Authorization (사용자가 무엇을 할 수 있는가)** — 매매 엔진이 처리.

WTG 코드에 **거래 한도/통화쌍 활성/거래시간/slippage 같은 비즈니스 권한 체크를 추가하지 말 것.** WTG 가 책임지는 것은 JWT, MFA, rate-limit, IP 화이트리스트, 봇 탐지뿐. login 시 매매 엔진이 발급한 `cookie_t` 를 Redis 에 저장하고 이후 호출에 그대로 첨부 (passthrough). 비즈니스 거부는 항상 엔진 응답을 기다린 후 그대로 전달. 자세히는 `docs/auth.md`.

## `pkg/mymq` 사용 시 주의

- **Channel 자동 첨부** — `Client.Call/Send` 가 `FrameInput.Chan` 빈값이면 `Options.Channel.Bytes()` 자동 주입.
- **Navi 자동 채움** — `Client.applyDefaults` 가 origin/destination navi 자동 채움. **수동으로 navi 다루다 빠뜨리면 broker 가 "no navigation" 으로 거부한다.** 일부러 override 하는 게 아니라면 default 로 두기.
- **ApplName / Channel 상수** — `pkg/mymq/conventions.go` 의 `ApplMci*` / `Channel*` / `Exchange*` / `RKey*` / `Queue*` 사용. 매직 스트링 박지 말 것.
- **Reconnect** — `Options.Reconnect` 채우면 supervisor goroutine 이 자동 재연결. nil 이면 1회용.

## 코드 스타일

- **주석 / 문서 / 커밋 메시지는 한글**, **식별자(타입/함수/변수/패키지)는 영문**.
- Go 1.25 (`go.mod`) 기준이지만 CI 는 1.23 으로도 통과해야 한다 — 1.24+ 전용 API 도입 시 주의.
- 핸들러는 struct 보다 함수형 + `Deps` 주입 패턴 (`internal/api/handlers/handlers.go` 참조).
- `mymq.Error` 의 errn 은 핸들러에서 그대로 응답 본문에 동봉 (auth 위임 원칙).

## 외부 의존성

- **MyMQ 매매 엔진** (`/Users/winwaysystems/mywork/mymq`) — broker (mymqd) + AP (test_service / WECHO / W*/BW*). WTG 코드에서 직접 수정하지 않는다.
- **etcd** — 라우팅 룰 / 정책 + 시세 카탈로그 (symbols / pricing / profiles). `mci-admin` write, `mci-api` / `mci-price` watch. 모든 service 가 동일 인증서로 TLS / mTLS 옵션 (`--etcd-tls-cert/-key/-ca/-sni`). 테스트는 `test/etcdtest` embedded etcd.
- **Redis** (운영) — `pkg/auth.RedisStore` — 세션 + cookie_t (멀티 인스턴스 공유, 재시작 복구). 단위 테스트는 `miniredis`. dev 는 `MemoryStore`.
- **TimescaleDB / PostgreSQL** — `quote_bars` hypertable. `mci-price.Archiver` 가 `pgx/v5` 로 INSERT, `mci-chart.Repository` 가 SELECT. `etc/sql/quote_bars.sql` 로 부트스트랩.

## 참고 문서

### 핵심 (이 5개로 day-1 가능)
- `docs/mci-architecture.md` — 컴포넌트 흐름도 + 내부 도구 라우팅 권고
- `docs/conventions.md` — ApplName / Channel / Exchange / RoutingKey / Queue 카탈로그
- `docs/routing.md` — alias→exchange/rkey 변환, Registry, SeedPolicy 상세
- `docs/operations.md` — 서비스별 flag/env 카탈로그 + mci-admin 운영 작업 + 부트스트랩 순서
- `docs/auth.md` — 인증·권한 위임 명세 (Site/Tier 추가 후)

### 시세 도메인 (FX 챠트 기능)
- `docs/cooker-quote-schema.md` — cooker → broker → mci-price wire v1 평면 envelope
- `docs/chart-schema.md` — TimescaleDB quote_bars hypertable (압축/retention 정책)
- `etc/sql/quote_bars.sql` — DDL (`create_hypertable` + 압축/retention)
- `etc/{symbols,profiles,pricing}.json` — 운영 카탈로그 샘플 (정적 파일 모드)

### 심층 자료 / 특수 주제
- `docs/phase0-analysis.md` — wire protocol 분석 + 설계 결정
- `docs/roadmap.md` — 9-Phase 구현 계획 (~22주)
- `docs/testing.md` — 단위 → 통합 → e2e 단계별 시나리오
- `docs/mci-test-runbook.md` — `mci-test` CLI 운용 절차 (ckey echo 등 GO/NO-GO 검증)
- `docs/cooker-patch.md` — 옵션 A-1 (Cooker 가 myrqd + mymqd 양쪽 publish) C 패치 명세
- `docs/broker-tls.md` — broker TLS 합의안
