# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 프로젝트 개요

**WTG (Winway Trading Gateway)** — 기존 C 기반 MyMQ 매매 엔진(`/Users/winwaysystems/mywork/mymq`) 앞단의 Go 기반 web 게이트웨이. 웹/FIX/CS 채널을 단일 broker(`mymqd`) 로 정규화한다.

핵심 설계 원칙:
- **C 엔진 최소 수정** — broker(`mymqd`) 와 매매 AP 는 비즈니스 로직 변경 X. 단 wire schema 확장 (예: mqhdr 끝의 `trcid[16]` trace_id) 같은 cross-cutting 인프라 추가는 양측 합의 후 big-bang deploy 로 진행. 상세는 `docs/broker-tracing.md`.
- **Go-native, no cgo** — `pkg/mymq` 가 wire protocol(`mymq.h` / `mq.h`) 을 순수 Go 로 재구현 (100B mqhdr + navi[] + 가변영역, BE network byte order, 4B length-prefix framing, 4B 빈 프레임 heartbeat).
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
make ci                 # lint + vulncheck + test-race + build (CI 동일, commit/PR 전 권장)
make ckey-echo          # Phase 1 GO/NO-GO — mymqd 가 ckey echo back 하는지
make proto              # api/proto/wtg/v1/*.proto → pkg/wtgpb/v1/*.pb.go
make install            # build/bin/* → $(BINDIR), etc/* → $(ETCDIR) (PREFIX=/opt/wtg 기본)
make cside              # Phase 2.6 — cside/wtgpush C SDK 빌드 (POSIX socket, 외부 의존 0)
make cside-clean        # cside/wtgpush 산출물 정리
make test-cside         # C SDK ↔ mci-push HTTP push 핸들러 wire 호환성 (build tag=cside, cside 선결)
make wtgprice           # Phase S3-d — cside/wtgprice C SDK 빌드 (매칭 엔진 swap/lock 호출용)
make wtgprice-clean     # cside/wtgprice 산출물 정리
make test-wtgprice      # C SDK ↔ SwapLockHandler wire 호환성 (build tag=wtgprice)
make wtgquery           # PoC — cside/wtgquery C SDK 빌드 (mds W9501S01 종가 조회의 mci-chart REST wrapper)
make wtgquery-clean     # cside/wtgquery 산출물 정리
make test-wtgquery      # C SDK ↔ mci-chart /v1/chart + mci-price /v1/best-stats wire 호환성 (build tag=wtgquery)
```

`cmd/<service>/main.go` 추가 시 자동으로 빌드 대상에 포함된다 (Makefile 가 `cmd/*/*.go` glob).

### 단일 테스트 실행

```bash
go test ./pkg/mymq/ -run TestCkeyEcho -v
go test ./internal/api/handlers/ -run TestLogin -race
go test -tags=integration -run TestEtcdRouting ./internal/api/...   # build tag 필요 테스트
```

### 로컬 스택 부팅 (개발용)

```bash
./scripts/dev-up.sh                    # TimescaleDB Docker 컨테이너 + DDL + 데모 봉 seed
./scripts/wtg-stack-up.sh              # WTG 바이너리 (mci-admin + mci-price + mci-edge-price + tickloop)
./scripts/wtg-stack-up.sh --with-broker --with-api   # docker mymqd + mci-api 까지
./scripts/wtg-stack-up.sh --with-all                 # broker/forwarder/prom 포함 (chart 는 DB 의존이라 별도)
./scripts/wtg-status.sh                # 프로세스 / docker broker / 핵심 카운터 1회 스냅샷
./scripts/wtg-stack-down.sh            # 일괄 종료
```

### 부하 테스트 (시세 파이프라인)

```bash
./scripts/load-test.sh low      # baseline 640 tick/s
./scripts/load-test.sh mid      # typical 6.4k tick/s
./scripts/load-test.sh high     # extreme 64k tick/s
./scripts/load-test.sh custom 500 USDKRW,EURUSD 30s   # 임의 rate × pairs

# 결과 CSV: logs/load-<scenario>-<ts>.csv
# 측정 카운터: forwarder /stats /metrics + mci-price /v1/price-stats /v1/best-stats
# pprof: http://127.0.0.1:8082/debug/pprof/  (DevMode) — http://127.0.0.1:9091/debug/pprof/
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

### standalone (broker 없이 single binary)

`--no-broker` 는 `mci-admin` 뿐 아니라 `mci-push` / `mci-price` 도 지원 — broker 부재 시
broker subscribe path skip, HTTP/gRPC 만으로 단독 부팅. PoC / 회귀 / 로컬 개발용.

```bash
./build/bin/mci-push  --no-broker --listen :8081    # HTTP push only
./build/bin/mci-price --no-broker --listen :8082    # gRPC + REST only (broker fan-in 없음)
```

## 컴포넌트 구조

| 영역 | 바이너리 | 포트 | mymq channel | 책임 |
|-----|---------|-----|--------------|-----|
| DMZ | `mci-edge-api` | 8090 | (HTTP only) | TLS termination + JWT 검증 + IP 화이트리스트 + rate-limit |
| DMZ | `mci-edge-push` | 8084 | (gRPC client) | mci-push PushService → 외부 ws fan-out |
| DMZ | `mci-edge-price` | 8083 | (gRPC client) | raw tick broadcast + Profile 별 quote stream (SubscribeQuote) + **다중 인스턴스 fan-out** (N6, mci-admin 에서 관리) |
| DMZ | `mci-edge-chart` | 8087 | (HTTP/WS proxy) | TLS + 인증 + IP CIDR + rate-limit → mci-chart reverse-proxy |
| DMZ | `mci-edge-fix` | 5001 (FIX) / 5002 (stats) | (HTTP forward) | FIX 4.4 session 종단 + Logon 검증 + NewOrderSingle (35=D) → `/v1/tx` alias. 정적 counterparty seed (Phase A) — etcd watch 는 Phase B |
| DMZ | `mci-edge-md` | 5011 (FIX MD) / 5012 (stats) | (gRPC client) | FIX 4.4 시세 gateway — 외부 카운터파티 MarketDataRequest (35=V) → MarketDataSnapshotFullRefresh (35=W) 응답 |
| DMZ | `mci-edge-tcp` | 5021 (raw TCP) | (HTTP forward) | 레거시 cs (HTS/EMP) 용 raw TCP 전문 gateway — 지속 연결 + 4B length-prefix 전문 + 빈 프레임 heartbeat (mymq 컨벤션) → `/v1/tx` raw 모드. 검증은 `cmd/tcp-tester` |
| Internal | `mci-api` | 8080 | `ChannelWeb` | `/v1/tx` `/v1/login` (Site/Tier → Session.Profile + JWT) `/v1/refresh`. `data` 가 JSON 이면 svc I/O 명세(`pkg/svcio`) 기반 고정폭 전문 자동 조립 |
| Internal | `mci-admin` | 9090 | `ChannelAdmin` | UI + 라우팅/정책 관리 + symbols/pricing/profiles CRUD + audit ring |
| Internal | `mci-push` | 8081 | `ChannelWeb` (rep) | broker unsolicited 수신 → 사용자별 ws fan-out. **HTTP push endpoint** (Phase 2.x — broker 우회, secret-only 또는 mTLS, MultiClient + consistent hash ring sticky) 병행. `--no-broker` 로 HTTP-only standalone 부팅 가능 |
| Internal | `mci-price` | 8082 (HTTP) / 50051 (gRPC) | `ChannelWeb` | 시세 fan-out + **BestConsumer** (다중시장 best 산정, mds 모델) + Aggregator (OHLC 봉) + PricingConsumer + SubscribeBar gRPC + **AlgoStream** (`SubscribeAlgo` — algo 봇 전용 stream, per-client isolation + slow client evict). `--no-broker` 로 standalone 부팅 가능 |
| Internal | `mci-chart` | 8086 | (gRPC client) | TimescaleDB historical 봉 REST + ws 라이브 봉 (SubscribeBar) |
| Internal | `quote-forwarder` | UDP 30044~30051 | `ChannelAdmin` | UDP FIX 4.4 시세 → reader/worker 분리 + **batch publish** (default 14 envelope/msg) → broker broadcast |
| 검증 | `mci-test` | — | — | Phase 1 ckey echo 검증 CLI |
| 검증 | `load-gen` | — | — | UDP 시세 부하 생성기 (`scripts/load-test.sh` 의 low/mid/high 시나리오). delivery/drop/sub_drops 측정 |
| 검증 | `fix-tester` / `fix-md-tester` | — | — | mci-edge-fix / mci-edge-md smoke test CLI (quickfix initiator — Logon + 주문/MDR 1건) |
| 검증 | `algo-tester` | — | — | `PriceService.SubscribeAlgo` smoke tester — latest tick stdout dump |
| 검증 | `spot-load-gen` / `ws-load-gen` | — | — | `/v1/quote/spot` REST latency / ws fan-out burst PoC 부하 생성기 |
| 검증 | `tcp-tester` | — | — | mci-edge-tcp 클라이언트 시뮬레이터 — 지속 연결 + 주기 heartbeat RTT + 전문 송수신 (`--send-file` / `--reconnect`) |
| 검증 | `dev-bar-faker` | — | — | gRPC `PriceService.SubscribeBar` mock — mci-chart 단독 테스트용 |
| 검증 | `quote-diff` | — | — | 두 ws source (legacy/best) envelope 자동 비교 — cs P4-1 dual run confidence, 변환 정확도 검증 |
| 운영 도구 | `fx-sync` | — | — | 외환 운영 DB (Oracle / file mock) 마스터 데이터 (currency 등) → WTG etcd 미러링 CLI |

mymqd (broker) 는 11217, cluster 11218. `ApplName` 컨벤션과 채널/exchange/routing-key 카탈로그는 `docs/conventions.md` + `pkg/mymq/conventions.go` 가 단일 출처 — 변경 시 양쪽 동기화.

## 디렉토리 매핑

```
pkg/                     # 공유 라이브러리 (단방향 DAG, 도메인 layer)
  session/               # Channel/Site/Tier/Profile/Pair/LogonID 도메인 enum (leaf)
  quote/                 # Quote, RingBuffer, SymbolMap, Bar/Timeframe + JSON envelope (v1)
                         # + pushdata.go (EncodePushdata / EncodePushdataV1 / EncodePushdataBatch)
                         # + etcd_symbols.go (EtcdSymbolWatcher)
  pricing/               # PricingTable (atomic snapshot) + Apply + JSON codec
                         # + etcd.go (EtcdTableWatcher)
  mymq/                  # libmymq-go: wire protocol + Client + ckey 멀티플렉싱
                         # codec / frame / handshake / reconnect / pubsub / conventions
                         # Quote/Bar 관련 exchange + routing-key 상수
                         # + LZO1X (MLZO) 압축 응답 본문 자동 해제
  auth/                  # JWT (Claims.Site/Tier) + Session.Profile + Memory/RedisStore
  policy/                # kill switch / 정비창 / 차단 심볼 (etcd watch, TLS)
  routing/               # transaction alias → exchange/routing_key (etcd watch, TLS)
  quoteid/               # quote ID 검증 (sync/async, Memory/Redis store, etcd allowlist)
                         # — RFC: docs/quoteid-validation-rfc.md
  idempotency/           # 멱등키 store (Memory / Redis) — 동일 요청 중복 차단
  otelinit/              # OpenTelemetry tracer/meter 초기화 (서비스 공통)
  svcio/                 # 매매 svc I/O 헤더 (win/src/inc/trn/WnnnnSnn.h) 파서 + Registry
                         # — JSON ↔ 고정폭 전문 자동 조립 (/v1/tx·/v1/login) + admin UI 메타
  push/                  # HTTP push 클라이언트측 — Client + MultiClient + consistent hash ring
  metrics/ netutil/ ratelimit/ tlsutil/
  wtgpb/                 # gRPC 생성 코드 (admin↔mci-api, edge↔internal, chart↔price)
cmd/                     # 서비스 entrypoint
  mci-{api,push,price,chart,admin,test} mci-edge-{api,push,price,chart,fix,md}
  quote-forwarder
  load-gen               # 부하 생성기 (scripts/load-test.sh 가 wrap)
  dev-bar-faker          # gRPC PriceService.SubscribeBar mock (chart 단독 테스트)
  quote-diff             # 두 ws source envelope 자동 비교 (legacy/best dual run)
  fx-sync                # 외환 운영 DB (Oracle / file mock) → WTG etcd 미러 CLI
  fix-tester             # mci-edge-fix smoke test (Logon + NewOrderSingle)
  fix-md-tester          # mci-edge-md smoke test (Logon + MDR → 스냅샷 표 출력)
  algo-tester            # SubscribeAlgo smoke tester
  spot-load-gen          # /v1/quote/spot REST latency 측정 (docs/spot-latency-poc.md)
  ws-load-gen            # ws fan-out burst 측정 (docs/ws-fanout-burst-poc.md)
internal/                # 서비스별 비즈니스 로직
  price/                 # Server + BestConsumer (다중시장 best, mds 모델) + Aggregator
                         # + Archiver (pgx → TimescaleDB) + PricingConsumer
                         # + JSONCookerDecoder + ParseEnvelopes (단일/배열 auto-detect)
                         # + GRPCServer + EtcdProfileSource (etcd watch)
  chart/                 # REST (Repository.QueryBars) + WS Hub + SubscribeBar 수신
  admin/                 # 운영 콘솔 — 라우팅/정책/symbols/pricing/profiles CRUD
  push/                  # mci-push 핸들러 — broker rep receiver + HTTP push (Phase 2.x)
                         # + Dispatcher (ws fan-out) + Registry (consistent hash ring)
                         # + MultiClient sticky + mTLS / X-Push-Secret 인증
  fxsync/                # fx-sync 백엔드 — Backend 인터페이스 (file / Oracle) + Syncer (etcd write)
  api/ edge/{api,price,push,chart,fix,md}
cside/                   # C SDK 모음 (POSIX socket, 외부 의존 0 — 운영 C svc 가 WTG 호출)
  wtgpush/               # Phase 2.6 — libwtgpush.a + wtgpush.h (mci-push HTTP push, broker 우회)
                         # sample.c 빌드/실행으로 wire 호환 직접 검증
  wtgprice/              # Phase S3-d — 매칭 엔진의 swap/lock 호출용
  wtgquery/              # PoC — mds W9501S01 종가 조회의 mci-chart REST wrapper
api/proto/wtg/v1/        # .proto 원본 (price / push / quote_validation)
deploy/                  # EC2 배포 — native binary + systemd (docker 미사용), self-hosted runner
                         # setup-ec2.sh / seed-catalog.sh / systemd units / observability
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

`data` 가 JSON 객체이면 `pkg/svcio` 가 svc I/O 헤더 명세 (`win/src/inc/trn/WnnnnSnn.h` 파싱) 기반으로 고정폭 전문을 자동 조립하고, 응답도 역방향 파싱해 JSON 으로 돌려준다. 엔진의 LZO1X (MLZO) 압축 응답은 `pkg/mymq` 가 자동 해제.

**raw 전문 모드 (emp/hts 레거시)**: `Content-Type: application/octet-stream` + `X-WTG-Alias` (또는 `X-WTG-Exchange`/`-Routing-Key`) 헤더로 호출하면 body 전문 바이트를 무변형 통과, 응답도 output 전문 그대로 + `X-WTG-Errn` 헤더 (errn≠0 여도 전문 있으면 200 — 레거시는 COMHDR 로 판단). CP949 무손상. 상세는 `docs/routing.md` §5.

### 2. Push (unsolicited fan-out) — 두 트랙 병행

**트랙 A: broker representative receiver (legacy / 매매 엔진 발 publish)**
`mci-push` 는 `QF_UNSOL_REP` flag 로 broker 의 representative receiver 에 등록 → user 매칭 없이 모든 publish 수신. 큐 이름이 **빈값** 이어야 broker 가 `_CLIENT_` type 으로 등록한다 (`publish.c:185-189` 참조). broadcast prefix 80B 의 `LogonID` 로 fan-out target 을 결정 — 빈값이면 전체 broadcast.

**트랙 B: HTTP push endpoint (Phase 2.x — broker 우회)**
운영 svc 가 `mci-push` 의 HTTP endpoint 로 직접 push 를 던지는 경로. broker SIGABRT 부하 회피 + 운영 C 코드의 mymq 의존 제거가 동기.
- **인증**: `X-Push-Secret` (secret-only 모드, 운영 결정) 또는 mTLS — 동시 지원, secret/mTLS 어느 쪽도 통과면 accept. 자세한 절차는 `docs/push-secret-rotation.md`.
- **MultiClient + consistent hash ring**: 다중 `mci-push` 인스턴스를 sticky 라우팅 (virtual node 로 hash 재분배 최소화). 같은 user → 항상 같은 instance 로 향함.
- **C SDK**: `cside/wtgpush/` (`libwtgpush.a` + `wtgpush.h`) — 외부 의존 0 의 POSIX socket. 운영 C svc 가 `wtg_push_send()` 한 줄로 트랙 B 진입. `make test-cside` 가 wire 호환성 검증.
- **standalone**: `mci-push --no-broker` 로 broker subscribe 없이 HTTP 전용 부팅.
- **rollout / 관측**: Phase 2.7 readiness alert + Grafana dashboard panel — `docs/phase-2.7-rollout.md` 참고.

### 3. 시세 raw (UDP → broker broadcast → mci-price)
`quote-forwarder` 가 UDP FIX 4.4 (35=W/X) 파싱 → v1 envelope publish (`FCCast/SubBroadcast`, Exchange="PRICE"):
- **per-feed reader/worker 분리** — reader 는 pure ReadFromUDP, worker 가 parse+batch+publish. UDP kernel drop 회피.
- **batch publish** — 1 broker message 에 N envelope (default 14, JSON 배열 in `pushdata.msgb`). broker publisher thread ceiling 우회.
- **feed별 독립 broker connection** (ApplName `quote-fwd-NN`) — `pkg/mymq.Client.writeMu` 분리.
- 또는 cooker 가 직접 `pushdata.msgb` 에 v1 평면 envelope (단일 객체) 으로 publish — 동일 path. `mci-price.ParseEnvelopes` 가 `[`/`{` auto-detect.

### 4. 다중시장 best 산정 + 마진 적용 quote (mci-price → mci-edge-price → 고객 ws)
- **BestConsumer** (`internal/price/best.go`, mds 의 `mdssise_make_best` 모델):
  - per (Symbol, Source) 캐시. raw tick 마다 `max(bid) / min(ask)` 재계산.
  - cross (best_bid > best_ask) 시 최신 ts feed 의 bid/ask fallback.
  - 합성 `Tick{Source: "BEST"}` 를 downstream 으로 fan-out — 단일 feed cooker 시는 best of 1 = 자기 자신.
- `Aggregator` 가 BEST tick 을 6 timeframe (1s/1m/5m/15m/1h/1d) OHLC 봉 누적 (UTC bucket).
- `PricingConsumer` 가 BEST tick 에 `PricingTable.Apply` (Profile별 마진) → `MultiQuotePublisher` fan-out:
  - broker `ExchangeQuote` (TOPIC, routing-key=Profile.Key())
  - **gRPC `PriceService.SubscribeQuote`** stream (mci-edge-price 가 소비)
- `mci-edge-price` ws 클라이언트는 로그인 시 `Principal.ProfileKey()` (JWT) 로 자기 Profile 의 quote 만 수신.

### 5. 봉 영속 + 라이브 챠트 (mci-price → TimescaleDB + mci-chart → ws)
- `Aggregator.onClose` 가 봉 close 시 fan-out:
  - `Archiver` → pgx batch INSERT → `TimescaleDB.quote_bars` (1m+ 만 영속, 1s 는 메모리)
  - **gRPC `PriceService.SubscribeBar`** stream → `mci-chart`
- `mci-chart` REST `GET /v1/chart` 는 historical, WS `/v1/chart/stream` 는 라이브 (pair, tf 필터)
- **운영 카탈로그 hot reload**: `mci-admin` 이 etcd 에 PricingTable/SymbolMap/Profile/SymbolEntry write → 모든 `mci-price` 인스턴스가 watch 로 즉시 반영 (재배포 X)

### 6. 운영 진단 endpoint + backpressure 자동 alert
- **서버측 fan-out 가시화**: `mci-price` `GET /v1/subscribers` (구독자) / `GET /v1/customers` (customer 단일 검색·필터 cross-reference).
- **외부 ws 클라이언트 진단**: `mci-edge-price` `GET /v1/connections`.
- **매칭 엔진 lite path**: `mci-price` `GET /v1/quote/spot` — spot-only, Profile=BEST 강제 (S2). 봉/마진 없는 경량 응답.
- **backpressure 자동 alert**: `mci-price` / `mci-edge-price` 가 fan-out 채널 80% 도달 시 WARN 누적 → `mci-admin` "N7 backpressure 이력" 페이지에서 최근 히스토리 조회.
- 관련 metric / endpoint / admin UI 페이지 카탈로그: `docs/observability.md`.

## 인증/권한 분담

> **Authentication (사용자가 누구인가)** — WTG 가 처리.
> **Authorization (사용자가 무엇을 할 수 있는가)** — 매매 엔진이 처리.

WTG 코드에 **거래 한도/통화쌍 활성/거래시간/slippage 같은 비즈니스 권한 체크를 추가하지 말 것.** WTG 가 책임지는 것은 JWT, MFA, rate-limit, IP 화이트리스트, 봇 탐지뿐. login 시 매매 엔진이 발급한 `cookie_t` 를 Redis 에 저장하고 이후 호출에 그대로 첨부 (passthrough). 비즈니스 거부는 항상 엔진 응답을 기다린 후 그대로 전달. 자세히는 `docs/auth.md`.

## `pkg/mymq` 사용 시 주의

- **Channel 자동 첨부** — `Client.Call/Send` 가 `FrameInput.Chan` 빈값이면 `Options.Channel.Bytes()` 자동 주입.
- **Navi 자동 채움** — `Client.applyDefaults` 가 origin/destination navi 자동 채움. 단 broadcast func (FCCast/FCPush/FCSignal) 인 경우는 navi 자동채움 skip + Dirf 기본 `DirPublish` 보정 (그 외 navi 가 있으면 broker 가 transaction 으로 오인). **수동 navi 다루다 빠뜨리면 broker 가 "no navigation" 으로 거부.**
- **broadcast publish 규칙** — Exchange 는 `BroadcastHeader.Exchange` 에 박는다 (FrameInput.Xchg 가 아니라). 그렇지 않으면 navi 자동채움이 트리거되어 broker 가 publish_packet 대신 message_packet_transfer 로 분기 → "Lost reply message" drop.
- **broker subscribe receiver 등록** — `Options.Queue` 에 `ExchangeName` 명시. broker `publish.c:223` 가 `client->xchg` 와 strcasecmp 매칭. 빈값이면 매 publish 마다 "Published 0/N" 으로 skip.
- **subCh drop 카운터** — `Client.SubDrops()` / `SubBufferCapacity()` 로 backpressure 진단. `Options.SubBufferSize` 로 채널 깊이 조정 (default 256).
- **ApplName / Channel 상수** — `pkg/mymq/conventions.go` 의 `ApplMci*` / `Channel*` / `Exchange*` / `RKey*` / `Queue*` 사용. 매직 스트링 박지 말 것.
- **Reconnect** — `Options.Reconnect` 채우면 supervisor goroutine 이 자동 재연결. nil 이면 1회용.

## 코드 스타일

- **주석 / 문서 / 커밋 메시지는 한글**, **식별자(타입/함수/변수/패키지)는 영문**.
- Go 1.25 (`go.mod`) 기준이지만 CI 는 1.23 으로도 통과해야 한다 — 1.24+ 전용 API 도입 시 주의.
- 핸들러는 struct 보다 함수형 + `Deps` 주입 패턴 (`internal/api/handlers/handlers.go` 참조).
- `mymq.Error` 의 errn 은 핸들러에서 그대로 응답 본문에 동봉 (auth 위임 원칙).
- `AGENTS.md` 는 본 파일의 Codex 용 사본 — CLAUDE.md 수정 시 함께 동기화할 것.

## 외부 의존성

- **MyMQ 매매 엔진** (`/Users/winwaysystems/mywork/mymq`) — broker (mymqd) + AP (test_service / WECHO / W*/BW*). 비즈니스 로직은 직접 수정 X. wire schema 확장 (예: mqhdr 끝 `trcid[16]`) 같은 인프라 변경은 양측 동시 deploy 필수.
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
- `docs/customer-connections.md` — 고객별 접속 관리 한 권 가이드 (login → JWT → ws → fan-out 3 트랙 → backpressure 격리 → 다중 인스턴스 → 운영 endpoint). alice/bob/charlie 풀 시나리오 예시 + FAQ

### 운영 패키지 (`docs/operations/` — 신규 운영자에게 통째로 공유)
신규 운영자가 본 디렉토리만 보면 운영의 90% 가 커버되는 단일 패키지. 인덱스 + 7권.
- `docs/operations/README.md` — 1주 학습 순서 + 상황 별 빠른 참조 + 인쇄 권장 절
- `docs/operations/admin-ui-manual.md` — mci-admin UI 37 페이지 × 6칸 매뉴얼 + 시나리오 7 + 부록 (1,930 줄)
- `docs/operations/operator-guide.md` — 설정 4 layer + 로그 위치/형식 + 자주 보는 메시지 25개 + 명령 모음
- `docs/operations/operations-routine.md` — 매일 5분 / 매주 30분 / 사고 시 3단계 SOP (인쇄용)
- `docs/operations/deployment-software.md` — 배포 시 설치할 모든 소프트웨어 + 부트스트랩 순서
- `docs/operations/deployment-scenario-ha-channel.md` — 단일 사이트 HA + 채널 분리 + 5 단계 wire 멘탈모델 (2,204 줄, 가장 큼)
- `docs/operations/deployment-scenario-multi-site.md` — 다중 사이트 Active-Active + GSLB
- `docs/operations/demo-scenario.md` — 시현/데모 60분 풀 코스 + 청중별 단축/확장

### 개발자 / 아키텍처 가이드
- `docs/directory-structure.md` — cmd/pkg/internal/etc 등 디렉토리 카탈로그 + 설정 파일 스키마 + "어디서 무엇을 찾아야 하나"
- `docs/simplification-guide.md` — Part B (자르기 카탈로그) + Part D (운영 UX 덮기) + 4주 로드맵

### 시세 도메인 (FX 챠트 + 마진)
- `docs/cooker-quote-schema.md` — cooker → broker → mci-price wire v1 평면 envelope
- `docs/chart-schema.md` — TimescaleDB quote_bars hypertable (압축/retention 정책)
- `docs/margin-business-spec.md` — 마진 업무 정의서 (운영자 매뉴얼)
- `docs/margin-policy.md` — 마진 정책 명세 (Profile별 bid/ask spread, Skew/Spread 등)
- `docs/margin-recompute.md` — 마진 재계산 트리거/동기화 절차
- `docs/swap-trade-spec.md` — FX swap (near+far 2-leg) 거래 잠금 명세 (Phase S3 초안)
- `etc/sql/quote_bars.sql` — DDL (`create_hypertable` + 압축/retention)
- `etc/{symbols,profiles,pricing}.json` — 운영 카탈로그 샘플 (정적 파일 모드)

### 운영 / 관측
- `docs/operations.md` — (핵심) 서비스별 flag/env + 부트스트랩 순서
- `deploy/README.md` — EC2 배포 가이드 (GitHub Actions self-hosted runner + native binary + systemd, docker 미사용)
- `docs/observability.md` — 운영 진단/관측 통합 가이드 (silent 가드 / metric / endpoint / admin UI 5 페이지)
- `docs/monitoring.md` — 일반 모니터링 가이드 (Prometheus / Grafana)
- `docs/push-monitoring.md` — mci-push source/CN 가시화 (Phase 2.5 dashboard + rules)
- `docs/push-secret-rotation.md` — HTTP push secret 회전 절차 (secret-only 모드 기준)
- `docs/quoteid-operations.md` — quote ID allowlist 운영 (etcd watch / Redis store)
- `docs/ratelimit.md` — rate-limit 정책 + 튜닝 가이드

### 심층 자료 / 특수 주제
- `docs/phase0-analysis.md` — wire protocol 분석 + 설계 결정
- `docs/roadmap.md` — 9-Phase 구현 계획 (~22주)
- `docs/testing.md` — 단위 → 통합 → e2e 단계별 시나리오
- `docs/mci-test-runbook.md` — `mci-test` CLI 운용 절차 (ckey echo 등 GO/NO-GO 검증)
- `docs/admin-ui-test-guide.md` — mci-admin UI 21개 페이지 별 화면/동작/테스트 시나리오
- `docs/cs-ws-migration.md` — legacy cs framework (Visual C++) 가 broker subscribe → mci-edge-price WS 로 마이그레이션. WinHTTP WebSocket 예시 + envelope 호환 옵션 + 일정 안내
- `docs/mds-coverage.md` — NH mds (C 시장 데이터 시스템) → WTG cover 매트릭스. DB/arbit/OTP/FOS 제외 시 cover 범위 + 회색지대 (W9501 종가) + 남는 작업 (W9500 wire adapter / pcap replay / SHM→gRPC latency 측정)
- `docs/fix-gateway-design.md` — `mci-edge-fix` DMZ gateway 설계 (외부 FIX 4.4 카운터파티 → broker). QuickFIX/Go + etcd counterparty 룰 + drop copy (mci-push HTTP push 재사용) + `/v1/tx` alias 1개. Phase A~D 로드맵 ~1개월
- `docs/quickfix-go-spike.md` — QuickFIX/Go 의존성 spike (Phase A 진입 전 평가). deps 6개 / vulncheck 깨끗 / boilerplate 84 LOC / multi-session 자동 지원 → **GO 판정**. Phase A 의 1주 견적 (~1,500 LOC + tests) 일관
- `docs/spot-latency-poc.md` — `/v1/quote/spot` REST latency PoC. `cmd/spot-load-gen` (concurrent goroutine + p50/p95/p99/p999) 으로 1k/5k/10k/20k RPS 측정. 결과: 20k RPS 까지 에러 0 + p99 ≤ 2ms — mds SHM 모델 대비 50~200배 느리지만 NH 운영 SLA (ms 수준) 안에 들어옴
- `docs/ws-fanout-burst-poc.md` — mci-edge-price 의 ws fan-out burst PoC. `cmd/ws-load-gen` 으로 10/100/500/1000 동시 client 측정. 결과: 100 안정 / 500 degradation / 1000 stuck. Mac dev kqueue 한계 가능성 — Linux 재측정 필수. fan-out cap 추정치 갱신 필요
- `docs/broker-sigabrt-analysis.md` — mymqd broker 의 부하 시 SIGABRT 진단. publish.c 정독 결과 + ASAN/core dump 후속 진단 절차
- `docs/broker-reconnect.md` — broker 연결 끊김 시 supervisor goroutine 재연결 정책
- `docs/cooker-patch.md` — 옵션 A-1 (Cooker 가 myrqd + mymqd 양쪽 publish) C 패치 명세
- `docs/broker-tls.md` — broker TLS 합의안
- `docs/dev-main.md` — `win/src/lib/db2stub/dev_main.c` 운영 가이드 (DEV_MAIN_LOG / SIGUSR1 stats / crash handler / structured log)
- `docs/mci-price-ha.md` — mci-price 다중 인스턴스 HA (etcd watch 일관성 + BestConsumer 분산 고려)
- `docs/phase-2.7-rollout.md` — broker 우회 HTTP push Phase 2.7 rollout 계획 (readiness alert / dashboard panel)
- `docs/quoteid-validation-rfc.md` — quote ID 검증 RFC (sync/async 모드, store 선택, 성능 트레이드오프)
