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
| DMZ | `mci-edge-price` | 8083 | (gRPC client) | 시세 외부 ws fan-out |
| Internal | `mci-api` | 8080 | `ChannelWeb` | `/v1/tx` `/v1/login` `/v1/logout` `/v1/refresh` `/v1/ping` |
| Internal | `mci-admin` | 9090 | `ChannelAdmin` | 운영 콘솔 + 라우팅/정책 관리 + audit ring |
| Internal | `mci-push` | 8081 | `ChannelWeb` (rep) | broker unsolicited 수신 → 사용자별 ws fan-out |
| Internal | `mci-price` | TBD | `ChannelWeb` | 시세 fan-out |
| Internal | `quote-forwarder` | UDP 30044~30051 | `ChannelAdmin` | UDP FIX 4.4 시세 → broker broadcast publish |
| 검증 | `mci-test` | — | — | Phase 1 ckey echo 검증 CLI |

mymqd (broker) 는 11217, cluster 11218. `ApplName` 컨벤션과 채널/exchange/routing-key 카탈로그는 `docs/conventions.md` + `pkg/mymq/conventions.go` 가 단일 출처 — 변경 시 양쪽 동기화.

## 디렉토리 매핑

```
pkg/                     # 공유 라이브러리 (= mymq/src/lib/)
  mymq/                  # libmymq-go: wire protocol + Client + ckey 멀티플렉싱
                         # codec / frame / handshake / reconnect / pubsub / conventions
  auth/                  # JWT 발급/검증, 세션 store, refresh token
  policy/                # kill switch / 정비창 / 차단 심볼·routing-key (etcd watch)
  routing/               # transaction alias → exchange/routing_key (etcd watch)
  config/ log/ metrics/ session/ netutil/ ratelimit/ svcio/ tlsutil/
  proto/ wtgpb/          # gRPC (admin↔mci-api, edge-push↔mci-push 내부 채널)
cmd/                     # 서비스 entrypoint (= mymq/src/{mqd,rqd,...})
internal/                # 서비스별 비즈니스 로직 (api / admin / push / price / edge)
api/proto/               # .proto 원본
test/integration/        # 실 mymqd 통합 테스트
test/etcdtest/           # embedded etcd helper
docs/                    # 명세 (auth/conventions/architecture/roadmap/...)
etc/                     # 운영 설정 (= mymq/etc/)
```

## 메시지 흐름 — 알아야 할 핵심 3가지

### 1. Transaction (sync RPC) — `POST /v1/tx`
**transaction별 핸들러를 만들지 말 것.** generic envelope 1개로 모든 매매 transaction 을 broker 에 통과시킨다:
```json
{"alias":"WECHO_PING","data":""}                          // alias 기반 (권장)
{"exchange":"ECHOSVC","routing_key":"PING","data":""}     // raw envelope
```
`alias` 는 `mci-admin` 의 라우팅 룰 store(etcd, watch 동기화)에서 `exchange/routing_key` 로 resolve. broker 가 ckey 를 응답에 echo back 하므로 단일 connection 으로 동시 호출 가능.

### 2. Push (unsolicited fan-out)
`mci-push` 는 `QF_UNSOL_REP` flag 로 broker 의 representative receiver 에 등록 → user 매칭 없이 모든 publish 수신. 큐 이름이 **빈값** 이어야 broker 가 `_CLIENT_` type 으로 등록한다 (`publish.c:185-189` 참조). broadcast prefix 80B 의 `LogonID` 로 fan-out target 을 결정 — 빈값이면 전체 broadcast.

### 3. 시세 (UDP → broker broadcast → ws)
`quote-forwarder` 가 UDP FIX 4.4 (35=W/X) 파싱 → JSON envelope 로 `FCCast/SubBroadcast` publish (LogonID="") → `mci-push` representative 가 받아 모든 ws 사용자에게 fan-out. FIX 파싱은 `cmd/quote-forwarder/main.go` 의 `parseQuote` 함수에 단일 구현 (별도 internal 패키지 없음).

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
- **etcd** — `mci-admin` 이 라우팅 룰/정책을 etcd 에 쓰고, `mci-api` / `mci-push` 등이 watch. 테스트는 `test/etcdtest` 의 embedded etcd 사용.
- **Redis** (운영) — 세션 / refresh token 저장. dev 는 `pkg/auth/memstore.go` in-memory.

## 참고 문서

- `docs/phase0-analysis.md` — wire protocol 분석 + 설계 결정
- `docs/roadmap.md` — 9-Phase 구현 계획 (~22주)
- `docs/conventions.md` — ApplName / Channel / Exchange / RoutingKey / Queue 카탈로그
- `docs/auth.md` — 인증·권한 위임 명세
- `docs/mci-architecture.md` — 컴포넌트 흐름도 + 내부 도구 라우팅 권고
- `docs/testing.md` — 단위 → 통합 → e2e 단계별 시나리오
- `docs/mci-test-runbook.md` — `mci-test` CLI 운용 절차 (ckey echo 등 GO/NO-GO 검증)
- `docs/cooker-patch.md` — 옵션 A-1 (Cooker 가 myrqd + mymqd 양쪽 publish) C 패치 명세
- `docs/broker-tls.md` — broker TLS 합의안
