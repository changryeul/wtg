# WTG 디렉토리 구조 & 설정 파일 안내

> WTG (Winway Trading Gateway) 의 **소스 레이아웃과 설정 파일** 의 단일 출처.
> 신규 개발자가 "어디서 무엇을 찾아야 하나" 의 답을 본 문서 한 권에서.
>
> 본 문서는 `wtg/` 저장소 root 를 기준으로 한다.

---

## 1. 한 장으로 본 전체 구조

```
wtg/
├── README.md                     # 프로젝트 짧은 소개
├── CLAUDE.md                     # Claude Code 용 컨텍스트 (운영자도 읽으면 좋음)
├── Makefile                      # 빌드/테스트/설치 진입점
├── go.mod / go.sum               # Go 모듈 정의 + 의존성 잠금
├── .gitignore                    # 산출물/로그 제외
│
├── cmd/                          # 서비스 entrypoint (각 디렉토리 = 1 바이너리)
├── pkg/                          # 공유 라이브러리 (도메인 단위, 외부 import 가능)
├── internal/                     # 서비스별 비즈니스 로직 (외부 import 차단)
├── api/proto/                    # gRPC .proto 원본
│
├── etc/                          # 운영 설정 (시세/마진/Profile/스키마/관측)
│   ├── symbols.json              # 통화쌍 카탈로그
│   ├── profiles.json             # Profile (Channel.Site.Tier) 카탈로그
│   ├── pricing.json              # PricingTable (5-Layer 마진 정책)
│   ├── db-mirror/                # 외환 운영 DB 미러 (currency/pair/hq_margin/...)
│   ├── sql/                      # TimescaleDB 스키마
│   ├── grafana/                  # Prometheus rule / Grafana dashboard
│   └── sdk/                      # 외부 배포용 SDK (C 헤더 등)
│
├── cside/                        # C SDK (외부 매매 엔진 / 매칭 엔진용)
│   ├── wtgpush/                  # mci-push HTTP push 호출용
│   └── wtgprice/                 # mci-price swap_lock 호출용
│
├── test/                         # 통합 테스트 helper
│   ├── etcdtest/                 # embedded etcd
│   ├── pgxtest/                  # postgres testcontainers
│   └── integration/              # 실 mymqd 통합 테스트 (MYMQD_HOST 필요)
│
├── scripts/                      # 운영/부하 스크립트
├── deploy/                       # 배포 자산 (docker-compose / observability)
├── docs/                         # 모든 명세 / 매뉴얼 / 운영 가이드
│
├── build/                        # 빌드 산출물 (gitignore)
│   └── bin/                      # → mci-*, quote-forwarder, ...
└── logs/                         # 로그 (gitignore, 개발 머신만 사용)
```

각 디렉토리의 자세한 내용은 §2 부터.

---

## 2. `cmd/` — 서비스 entrypoint

**규칙** : `cmd/<service>/main.go` 가 존재하는 디렉토리는 자동으로 `make build` 의 대상이 된다 (`Makefile` 의 `CMDS` glob). 새 서비스 추가 시 `cmd/<name>/main.go` 만 만들면 끝.

| 디렉토리                   | 산출물               | 역할                                                                              |
| ---------------------- | ----------------- | ------------------------------------------------------------------------------- |
| `cmd/mci-api/`         | `mci-api`         | `/v1/tx` `/v1/login` `/v1/refresh` (Internal :8080)                             |
| `cmd/mci-push/`        | `mci-push`        | broker rep receiver + HTTP push endpoint (:8081)                                |
| `cmd/mci-price/`       | `mci-price`       | 시세 fan-out + BestConsumer + 마진 + QuoteID + swap-lock (:8082 HTTP / :50051 gRPC) |
| `cmd/mci-chart/`       | `mci-chart`       | TimescaleDB historical 봉 + 라이브 SubscribeBar (:8086)                             |
| `cmd/mci-admin/`       | `mci-admin`       | 운영 콘솔 GUI (:9090)                                                               |
| `cmd/mci-edge-api/`    | `mci-edge-api`    | DMZ : TLS + JWT + IP allowlist + rate-limit (:8090)                             |
| `cmd/mci-edge-push/`   | `mci-edge-push`   | DMZ : mci-push 외부 노출 (:8084)                                                    |
| `cmd/mci-edge-price/`  | `mci-edge-price`  | DMZ : 시세 ws 외부 노출 (:8083)                                                       |
| `cmd/mci-edge-chart/`  | `mci-edge-chart`  | DMZ : mci-chart reverse proxy (:8087)                                           |
| `cmd/quote-forwarder/` | `quote-forwarder` | UDP FIX 4.4 → broker / gRPC publish                                             |
| `cmd/mci-test/`        | `mci-test`        | Phase 1 ckey echo 검증 CLI (GO/NO-GO)                                             |
| `cmd/load-gen/`        | `load-gen`        | UDP 시세 부하 생성기                                                                   |
| `cmd/dev-bar-faker/`   | `dev-bar-faker`   | gRPC SubscribeBar mock (chart 단독 테스트)                                           |
| `cmd/quote-diff/`      | `quote-diff`      | 두 ws envelope 비교 (legacy/best dual-run)                                         |
| `cmd/fx-sync/`         | `fx-sync`         | 외환 운영 DB → etcd 미러링 CLI                                                         |

### 빌드 산출물 위치

```
build/bin/<service>      ← make build 의 결과 (gitignore)
```

`make install PREFIX=/opt/wtg` 실행 시 :
```
/opt/wtg/bin/<service>   ← 운영 배포 위치
/opt/wtg/etc/*           ← etc/ 내용 복사
```

---

## 3. `pkg/` — 공유 라이브러리

**규칙** :
- `pkg/` 안은 **외부 import 가능** (`github.com/winwaysystems/wtg/pkg/...` 로 다른 프로젝트도 import 가능)
- 단방향 DAG — 상위 layer 가 하위를 import, 그 반대 금지
- 도메인 단위로 분리 (`session`, `quote`, `pricing` ...)

### 3.1 도메인 leaf (가장 안쪽)

| 패키지 | 역할 |
|---|---|
| `pkg/session/` | `Channel`/`Site`/`Tier` enum + `Profile` + `LogonID` |
| `pkg/quote/` | `Quote`, `RingBuffer`, `SymbolMap`, `Bar`/`Timeframe` + JSON envelope v1, pushdata, etcd watcher |
| `pkg/pricing/` | PricingTable (atomic snapshot) + 5-Layer Apply + crossrate + currency + pair + calendar + etcd watcher |

### 3.2 인프라 & 도메인

| 패키지 | 역할 |
|---|---|
| `pkg/mymq/` | libmymq-go — wire protocol + Client + ckey 멀티플렉싱 + Conventions + Reconnect supervisor |
| `pkg/auth/` | JWT (`Claims.Site/Tier`) + Session.Profile + Memory/RedisStore |
| `pkg/policy/` | kill switch + 정비창 + 차단 심볼 (etcd watch, TLS) |
| `pkg/routing/` | alias → exchange/routing_key Registry (etcd watch, TLS) + SeedPolicy |
| `pkg/quoteid/` | quote_id 검증 (sync/async) + Memory/Redis store + SwapIndex |
| `pkg/idempotency/` | 멱등키 store (Memory / Redis) — 중복 요청 차단 |
| `pkg/push/` | mci-push 의 클라이언트 / Registry / consistent hash ring |
| `pkg/svcio/` | broker AP transaction 카탈로그 (서비스 명세) |
| `pkg/ratelimit/` | rate-limit 정책 + 룰 store (etcd) |
| `pkg/wtgpb/` | gRPC `.pb.go` 코드 — `api/proto` 의 컴파일 결과 |

### 3.3 cross-cutting

| 패키지 | 역할 |
|---|---|
| `pkg/metrics/` | Prometheus 레지스트리 + WTG 메트릭 헬퍼 |
| `pkg/otelinit/` | OpenTelemetry tracer/meter 초기화 |
| `pkg/netutil/` | 네트워크 보조 (CIDR, dial 등) |
| `pkg/tlsutil/` | TLS / mTLS config 로더 |

> `pkg/config/`, `pkg/log/` 은 현재 분리 안 됨 — 각 서비스의 `internal/<svc>/config.go` 가 자기 로직을 가짐.

---

## 4. `internal/` — 서비스별 비즈니스 로직

**규칙** : Go 의 `internal/` 컨벤션 — 외부 import 차단. 서비스 안에서만 쓰는 로직.

| 디렉토리 | 어느 cmd 가 사용 | 주요 책임 |
|---|---|---|
| `internal/api/` | `cmd/mci-api/` | `/v1/tx` `/v1/login` 핸들러, broker passthrough, JWT |
| `internal/push/` | `cmd/mci-push/` | broker rep receiver + HTTP push handler + Dispatcher + Registry |
| `internal/price/` | `cmd/mci-price/` | BestConsumer / CrossConsumer / Aggregator / Archiver / PricingConsumer / SwapLockHandler / QuoteValidationServer / GRPCServer |
| `internal/chart/` | `cmd/mci-chart/` | REST `/v1/chart` + WS Hub + SubscribeBar 수신 + Repository (pgx) |
| `internal/admin/` | `cmd/mci-admin/` | 운영 콘솔 — 라우팅/정책/카탈로그 CRUD + audit ring + proxy (Prometheus / Grafana / mci-price / forwarder) |
| `internal/edge/` | `cmd/mci-edge-*` | DMZ 서비스 공통 (TLS / JWT / IP allowlist / rate-limit / proxy) |
| `internal/fxsync/` | `cmd/fx-sync/` | 외환 운영 DB Backend (file / Oracle) + Syncer (etcd write) |

각 디렉토리 안에는 보통 :
- `config.go` — flag/env 파싱
- `server.go` — 서비스 boot
- `<feature>.go` + `<feature>_test.go` — 기능 별 핸들러 / consumer / handler

---

## 5. `api/proto/` — gRPC 인터페이스 정의

```
api/proto/wtg/v1/
├── price.proto              # PriceService — SubscribeTick / SubscribeQuote / SubscribeBar / SubscribeCustomerQuote / PublishTick / RegisterCustomer
├── push.proto               # PushService — Send / Broadcast
└── quote_validation.proto   # QuoteValidationService — Validate / MarkConsumed / ValidateSwap / ConsumeSwap
```

빌드 :
```bash
make proto
# → pkg/wtgpb/v1/*.pb.go  +  pkg/wtgpb/v1/*_grpc.pb.go
```

필요 도구 — `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` (자세히 `docs/deployment-software.md` §6.2).

---

## 6. `etc/` — 운영 설정 파일 (가장 중요)

본 디렉토리 내용은 운영 시 `/opt/wtg/etc/` 로 배포된다 (`make install`). **이 파일들이 운영 정책의 시작점.**

### 6.1 `etc/symbols.json` — 통화쌍 카탈로그

**누가 읽나** : mci-price (정적 모드) — `--symbols etc/symbols.json` flag.
**etcd 모드** : 같은 데이터를 etcd `wtg/symbols/` 에 두고 watch.

**스키마** :
```json
[
  {"symbol": "USDKRW", "pair": "USD/KRW", "active": true},
  ...
]
```

| 필드 | 의미 |
|---|---|
| `symbol` | 외부 FIX feed 가 보내는 형태 (slash 없음) |
| `pair` | UI 표시용 (slash 있음) |
| `active` | 활성 거래 여부 |

**현재 시드 (10건)** : USDKRW / EURKRW / JPYKRW / GBPKRW / AUDKRW / CNYKRW / EURUSD / USDJPY / GBPUSD / AUDUSD.

운영 시 `🔗 통화쌍 마스터` 페이지로 동적 관리.

### 6.2 `etc/profiles.json` — Profile 카탈로그

**누가 읽나** : mci-price (정적 모드) — `--profiles etc/profiles.json`.

**스키마** :
```json
[
  {"channel": "WEB", "site": "BRANCH", "tier": "VIP"},
  ...
]
```

| 필드 | 값 |
|---|---|
| `channel` | `WEB` / `MOB` / `CS` (3자 이내, broker routing-key prefix 의 첫 토큰) |
| `site` | `HQ` / `BRANCH` / `OUTSRC` |
| `tier` | `VIP` / `GOLD` / `STD` |

Profile.Key() = `channel.site.tier` 형태 (예: `WEB.BRANCH.VIP`) — broker routing-key, ws 라우팅, PricingTable lookup 키.

**현재 시드 (10건)** : WEB/MOB/CS × HQ/BRANCH × VIP/GOLD/STD 의 일부 조합.

### 6.3 `etc/pricing.json` — PricingTable (5-Layer 마진 정책)

**누가 읽나** : mci-price (정적 모드) — `--pricing etc/pricing.json`.
**etcd 모드** : `wtg/pricing/table` 단일 doc + watch.

**최상위 구조** :
```json
{
  "version": 2,
  "time_windows": [ ... ],
  "swap_point":     [ ... ],
  "hq_margin":      [ ... ],
  "site_margin":    [ ... ],
  "customer_margin":[ ... ]
}
```

#### `version`
정수. 운영자가 변경 시 +1. mci-price 가 `pricing_version` 메트릭으로 노출.

#### `time_windows` — Window layer 의 정의
시각대별 마진 차등. `complement_of` 로 보수 표현 가능.
```json
[
  {"name": "regular",   "start": "09:00", "end": "15:30", "tz": "Asia/Seoul", "days": "MON-FRI"},
  {"name": "off_hours", "complement_of": "regular"}
]
```

#### `swap_point` — Swap layer
forward tenor 별 호가 차이 (운영자가 매일 갱신).
```json
[{"pair": "USD/KRW", "tenor": "1M", "bid_amount": 0.15, "ask_amount": 0.25}]
```

#### `hq_margin` — HQ layer
회사 전체 spread (pair × tier). `tier=""` 는 와일드카드 (fallback).
```json
[
  {"pair": "USD/KRW", "tier": "VIP",  "bid_amount": 0.02, "ask_amount": 0.02},
  {"pair": "USD/KRW", "tier": "",     "bid_amount": 0.15, "ask_amount": 0.15}
]
```

#### `site_margin` — Site layer
Channel × Site 별. `channel=""` 와일드카드.
```json
[{"pair": "USD/KRW", "channel": "WEB", "site": "BRANCH", "bid_amount": 0.05, "ask_amount": 0.05}]
```

#### `customer_margin` — Customer layer
customer_id 별 우대/페널티. `mode` 와 `priority` 가 핵심.

| 필드 | 의미 |
|---|---|
| `customer_id` | 특정 고객 |
| `pair` | "" 면 모든 통화쌍 |
| `bid_delta` / `ask_delta` | 변화량 |
| `mode` | `add` (HQ+Site 위에 추가) / `override` (HQ+Site 무시) |
| `priority` | 같은 customer 의 룰이 여러 개일 때 우선순위 |
| `comment` | 운영 메모 |

**현재 시드 예시** : alice (VIP, add −0.01), bob (GOLD, override ±0.005), carol (wildcard −0.02).

→ 5-Layer 결합 산식은 `docs/deployment-scenario-ha-channel.md` §5.7 참조.

### 6.4 `etc/db-mirror/` — 외환 운영 DB 미러

운영 환경에서 `fx-sync` CLI 가 외환 운영 DB (Oracle 등) → etcd 미러링 할 때의 **샘플/dev seed**. file backend 일 땐 이 파일들 그대로 사용.

| 파일 | 내용 |
|---|---|
| `currency.json` | 통화 코드 + 이름 + ref_code (ISO 4217) + decimal_places + active |
| `pair.json` | 통화쌍 + base/quote + kind(direct/cross) + symbol + spot_days + decimals |
| `hq_margin.json` | HQ layer 원본 (DB 모델) |
| `site_margin.json` | Site layer 원본 |
| `swap_point.json` | Swap layer 원본 |

→ `fx-sync --backend file --dir etc/db-mirror` 로 dev seed 부트스트랩 가능.

### 6.5 `etc/sql/` — TimescaleDB 스키마

| 파일 | 내용 |
|---|---|
| `quote_bars.sql` | `quote_bars` 테이블 + `create_hypertable` (1일 chunk) + 인덱스 + 압축 정책 (7일) + `add_compression_policy` |

mci-chart 부트스트랩 시 :
```bash
psql -d wtg -f etc/sql/quote_bars.sql
```

> 일반 PostgreSQL 만 쓰려면 hypertable / 압축 / retention 줄을 제거. mci-chart 의 SELECT 는 평범한 SQL 이라 동작 (`docs/deployment-scenario-ha-channel.md` §10.6.x 참조).

### 6.6 `etc/grafana/` — Prometheus rule + Grafana dashboard

| 파일 | 종류 | 내용 |
|---|---|---|
| `mci-price-swaplock-alerts.yml` | Prometheus rule | swap_lock 부분실패율 / revoke 실패 alert (S3-e) |
| `mci-push-alerts.yml` | Prometheus rule | mci-push 의 backpressure / sender error / dispatcher fail |
| `mci-push-recording-rules.yml` | Prometheus rule | mci-push pre-aggregated 시계열 |
| `mci-push-dashboard.json` | Grafana dashboard | mci-push 운영 panel |
| `p6-cross-master-alerts.json` | Grafana alert | cross-rate consumer / currency master alerts |
| `p6-cross-master-dashboard.json` | Grafana dashboard | cross/master 시각화 |
| `p7-broker-alerts.json` | Grafana alert | broker connect/disconnect/inflight aborted |
| `p7-ratelimit-alerts.json` | Grafana alert | rate-limit denied breakdown |
| `quoteid-alerts.json` | Grafana alert | quoteid op total / denied / expired |
| `quoteid-dashboard.json` | Grafana dashboard | QuoteID 시각화 |
| `quoteid-recording-rules.yml` | Prometheus rule | quoteid pre-aggregated |
| `README.md` | — | 본 디렉토리 dashboard / rule 카탈로그 |

운영 import :
```bash
# Prometheus
sudo cp etc/grafana/*.yml /etc/prometheus/rules/
# Grafana — UI 의 "Import dashboard" 로 JSON 업로드
```

### 6.7 `etc/sdk/c/` — C SDK 외부 배포용 헤더

C 매매 엔진 / 매칭 엔진이 cside 의 SDK 를 사용할 때 배포되는 헤더 사본.

→ 실제 SDK 소스는 `cside/` 에 있고, 본 디렉토리는 외부 배포용 미러.

---

## 7. `cside/` — C SDK

운영 C 매매 엔진 / 매칭 엔진이 사용. **외부 의존 0**, POSIX socket + HTTP/1.1 minimal + 간이 JSON 파서.

| 디렉토리 | 산출물 | 용도 |
|---|---|---|
| `cside/wtgpush/` | `libwtgpush.a` + `wtgpush.h` + `sample.c` | C svc → `POST mci-push HTTP push` (트랙 B) |
| `cside/wtgprice/` | `libwtgprice.a` + `wtgprice.h` + `sample.c` | 매칭엔진 → `POST mci-price /v1/quote/swap/lock` |

빌드 :
```bash
make cside        # wtgpush
make wtgprice     # wtgprice
make test-cside       # build tag=cside, mci-push 와 wire 호환 검증
make test-wtgprice    # build tag=wtgprice, swap_lock 와 wire 호환 검증
```

플랫폼 — AIX / Solaris / HP-UX / Linux / Darwin 어디서나 빌드.

---

## 8. `test/` — 통합 테스트 helper

`go test` 가 자동으로 picking up.

| 디렉토리 | 내용 |
|---|---|
| `test/etcdtest/` | embedded etcd helper. integration build tag. ~30s 부팅. |
| `test/pgxtest/` | testcontainers-go 로 postgres 띄움. integration build tag. |
| `test/integration/` | 실 mymqd 통합 테스트. `MYMQD_HOST` 환경변수 없으면 자동 skip → CI 는 broker 없이 green. |

실행 :
```bash
make test                  # 단위 (broker / etcd / postgres 없이)
make test-integration      # build tag=integration, embedded etcd 사용
MYMQD_HOST=10.0.0.10 go test -v ./test/integration/...   # 실 broker 통합
```

---

## 9. `scripts/` — 운영 / 부하 스크립트

| 스크립트 | 용도 |
|---|---|
| `dev-up.sh` | dev stack 일괄 부팅 (mci-* + etcd + Redis) |
| `dev-down.sh` | dev stack 일괄 종료 |
| `load-test.sh` | UDP 시세 부하 시나리오 — `low` (640/s) / `mid` (6.4k/s) / `high` (64k/s) / `custom` |
| `load-test-ratelimit.sh` | rate-limit 부하 (POST /v1/tx 폭주 → denied 검증) |
| `chaos-broker.sh` | broker 강제 kill → reconnect supervisor 검증 |
| `broker-loss-diag.sh` | broker subscribe 채널 drop 진단 (`SubDrops()` 카운터 추적) |
| `alias-smoke.sh` | dev-seed 후 모든 alias 에 1 건씩 호출 — routing 룰 회귀 |
| `import-svc-routes.sh` | broker AP svcio 카탈로그 → admin UI 의 라우팅 룰로 import |
| `seed_demo_bars.py` | mci-chart 데모용 봉 INSERT (TimescaleDB 직접) |

부하 결과는 `logs/load-<scenario>-<ts>.csv` 에.

---

## 10. `deploy/` — 배포 자산

| 파일 / 디렉토리 | 내용 |
|---|---|
| `docker-compose.yml` | dev / 스테이징용 (etcd / redis / postgres / prometheus / grafana 일괄) |
| `prometheus/` | scrape config + rule include |
| `grafana/` | provisioning (datasource + dashboard 자동 import) |
| `observability/` | OTel collector / Tempo / Loki 컨피그 |
| `README.md` | 배포 시작점 |

---

## 11. `docs/` — 명세 / 매뉴얼 / 운영 가이드

운영자/개발자/QA 가 보는 모든 문서. 카테고리별 정리 :

### 11.1 운영 매뉴얼 / 시나리오 (최근 작성, 가장 자세)

| 문서 | 내용 |
|---|---|
| `docs/admin-ui-manual.md` | mci-admin UI 37 페이지 매뉴얼 (37 페이지 × 6칸 + 시나리오 7 + 부록) |
| `docs/admin-ui-test-guide.md` | 페이지별 테스트 시나리오 |
| `docs/deployment-software.md` | 배포 시 설치할 모든 소프트웨어 |
| `docs/deployment-scenario-ha-channel.md` | 단일 사이트 HA + 채널 분리 (WEB / CS) 시나리오 — broker 라우팅 / wire / 시세 / push / quote_id 5 단계 멘탈모델 |
| `docs/deployment-scenario-multi-site.md` | 다중 사이트 Active-Active + GSLB |
| `docs/directory-structure.md` | (본 문서) 디렉토리 + 설정 파일 안내 |

### 11.2 아키텍처 / 컨벤션

| 문서 | 내용 |
|---|---|
| `docs/mci-architecture.md` | 컴포넌트 흐름 + 내부 도구 라우팅 |
| `docs/conventions.md` | ApplName / Channel / Exchange / RoutingKey / Queue 카탈로그 |
| `docs/operations.md` | 서비스 flag/env + 운영 작업 + 부트스트랩 |
| `docs/auth.md` | JWT + Session.Profile + cookie_t passthrough |
| `docs/routing.md` | alias → exchange/rkey, Registry, SeedPolicy |

### 11.3 시세 / 마진 도메인

| 문서 | 내용 |
|---|---|
| `docs/cooker-quote-schema.md` | cooker → broker → mci-price wire v1 envelope |
| `docs/chart-schema.md` | TimescaleDB hypertable 설계 |
| `docs/margin-business-spec.md` | 마진 업무 정의 |
| `docs/margin-policy.md` | 마진 정책 명세 (Profile spread / Skew / Spread) |
| `docs/margin-recompute.md` | 마진 재계산 트리거 / 동기화 |
| `docs/swap-trade-spec.md` | FX swap 2-leg 잠금 (Phase S3) |

### 11.4 운영 / 관측

| 문서 | 내용 |
|---|---|
| `docs/observability.md` | 운영 진단 / 관측 통합 가이드 |
| `docs/monitoring.md` | Prometheus / Grafana 가이드 |
| `docs/push-monitoring.md` | mci-push source/CN 가시화 |
| `docs/push-secret-rotation.md` | HTTP push secret 회전 절차 |
| `docs/quoteid-operations.md` | quote_id allowlist 운영 |
| `docs/ratelimit.md` | rate-limit 정책 + 튜닝 |

### 11.5 심층 / 특수 주제

| 문서 | 내용 |
|---|---|
| `docs/phase0-analysis.md` | wire protocol 분석 + 설계 결정 |
| `docs/roadmap.md` | 9-Phase 구현 계획 (~22주) |
| `docs/testing.md` | 단위 → 통합 → e2e 시나리오 |
| `docs/mci-test-runbook.md` | mci-test CLI 운영 절차 |
| `docs/cs-ws-migration.md` | legacy cs framework (VC++) → mci-edge-price ws 마이그레이션 |
| `docs/broker-sigabrt-analysis.md` | broker SIGABRT 부하 사고 사후 분석 |
| `docs/broker-reconnect.md` | supervisor goroutine 재연결 정책 |
| `docs/broker-tls.md` | broker TLS 합의안 |
| `docs/broker-tracing.md` | mqhdr trcid 확장 |
| `docs/cooker-patch.md` | broker 우회 publish 패치 |
| `docs/dev-main.md` | dev_main.c 운영 가이드 |
| `docs/mci-price-ha.md` | mci-price 다중 인스턴스 HA |
| `docs/phase-2.7-rollout.md` | broker 우회 push rollout |
| `docs/quoteid-validation-rfc.md` | quote_id 검증 RFC |

### 11.6 패치 (마이그레이션)

| 디렉토리 | 내용 |
|---|---|
| `docs/patches/` | broker C 코드 패치 명세 + diff |

---

## 12. 메타 파일

### 12.1 `go.mod` / `go.sum`

```
module github.com/winwaysystems/wtg
go 1.25.0
```

주요 의존성 :
- `go.etcd.io/etcd/...` — etcd 3.6 (client + embedded server)
- `github.com/redis/go-redis/v9` — Redis 클라이언트
- `github.com/jackc/pgx/v5` — PostgreSQL / TimescaleDB
- `github.com/gorilla/websocket` — WS server / client
- `github.com/prometheus/client_golang` — 메트릭
- `go.opentelemetry.io/otel/...` — 분산 추적
- `github.com/alicebob/miniredis/v2` — Redis 단위 테스트
- `github.com/testcontainers/testcontainers-go` — postgres 통합 테스트

> Go 1.25 (`go.mod`) 기준 but CI 는 1.23 으로도 통과해야 — 1.24+ 전용 API 도입 시 주의.

### 12.2 `Makefile`

```bash
make build              # 모든 cmd/*/main.go → build/bin/*
make test               # 단위 (broker 없이)
make test-race          # -race + coverage.out
make test-integration   # build tag=integration, embedded etcd
make coverage           # coverage.html 생성
make lint               # fmt-check + vet + staticcheck
make vulncheck          # govulncheck
make ci                 # CI 와 동일 전체
make ckey-echo          # Phase 1 GO/NO-GO
make proto              # api/proto/*.proto → pkg/wtgpb/v1/*.pb.go
make install            # build/bin/* + etc/* → /opt/wtg/{bin,etc}/
make cside              # cside/wtgpush C SDK
make wtgprice           # cside/wtgprice C SDK
make test-cside         # cside wire 호환
make test-wtgprice      # wtgprice wire 호환
```

핵심 변수 (override 가능) :
- `PREFIX=/opt/wtg` — install 의 base. 운영은 `/opt/wtg`, 사이트 별로 다른 경로 가능.
- `BINDIR=$(PREFIX)/bin`
- `ETCDIR=$(PREFIX)/etc`

### 12.3 `CLAUDE.md`

Claude Code (AI assistant) 에게 주는 컨텍스트. 운영자 / 신규 개발자가 읽어도 좋은 **프로젝트 한 페이지 요약** — 구조, 빌드 명령, 컴포넌트 표, 메시지 흐름 5가지, 인증/권한 분담, 외부 의존성, 참고 문서 인덱스.

### 12.4 `README.md`

리포지토리 첫 페이지. 짧은 소개 + 디렉토리 구조 + 핵심 설계 원칙.

### 12.5 `.gitignore`

빌드 산출물 (`build/`) + 로그 (`/logs/`) + repo root 에 잘못 떨어진 바이너리 + 에디터 메타 (`.idea/` `.vscode/` `.obsidian/`).

---

## 13. 빌드 / 배포 경로 한눈에

### 13.1 개발 머신

```
git clone wtg                       # 소스
cd wtg
make build                          # → build/bin/* (gitignore)
./build/bin/mci-admin --dev ...     # 로컬 실행
```

### 13.2 운영 서버

```
make install PREFIX=/opt/wtg
↓
/opt/wtg/
├── bin/                            # 모든 바이너리
│   ├── mci-admin
│   ├── mci-price
│   └── ...
└── etc/                            # 운영 설정
    ├── symbols.json                # (운영 시엔 etcd 사용 권장)
    ├── pricing.json
    ├── profiles.json
    ├── db-mirror/
    ├── sql/quote_bars.sql
    ├── grafana/*.yml *.json
    └── sdk/c/
```

systemd unit 예시 :
```ini
[Unit]
Description=WTG mci-price
After=network.target

[Service]
ExecStart=/opt/wtg/bin/mci-price --etcd https://etcd-sl-1:2379,... ...
Restart=on-failure
User=wtg
EnvironmentFile=/etc/wtg/mci-price.env

[Install]
WantedBy=multi-user.target
```

---

## 14. "어디서 무엇을 찾아야 하나" 빠른 안내

| 질문 | 어디 |
|---|---|
| 새 매매 transaction 의 alias 등록 | mci-admin UI 의 `🔀 라우팅 룰` 페이지 (런타임) |
| 마진 정책 변경 | mci-admin UI 의 `💰 마진 테이블` (런타임), 또는 `etc/pricing.json` (정적 모드) |
| 통화쌍 추가 | `etc/db-mirror/pair.json` + fx-sync, 또는 `🔗 통화쌍 마스터` |
| Profile / Tier 추가 | `etc/profiles.json` + `pkg/session/types.go` 의 enum |
| broker AP 의 transaction schema 확인 | mci-admin UI 의 `📋 서비스 명세` 페이지 |
| 운영 모니터링 대시보드 | `etc/grafana/*.json` import → Grafana UI |
| Prometheus alert 룰 | `etc/grafana/*.yml` → `/etc/prometheus/rules/` |
| dev stack 일괄 부팅 | `scripts/dev-up.sh` 또는 `docs/admin-ui-manual.md` 부록 A |
| 부하 테스트 | `scripts/load-test.sh low/mid/high/custom` |
| broker 재연결 검증 | `scripts/chaos-broker.sh` |
| broker AP 와 wire 합의 | `pkg/mymq/conventions.go` (ApplName / Channel / Exchange / RoutingKey / Queue) |
| C 매매 엔진의 push 호출 | `cside/wtgpush/wtgpush.h` (헤더) + `sample.c` (예시) |
| 매칭 엔진의 swap_lock 호출 | `cside/wtgprice/wtgprice.h` + `sample.c` |
| 새 protobuf 추가 | `api/proto/wtg/v1/*.proto` → `make proto` |
| TimescaleDB 스키마 변경 | `etc/sql/quote_bars.sql` + migration script |
| C 매매 엔진 SDK 배포본 | `etc/sdk/c/` |
| 운영 SOP / 사고 대응 | `docs/admin-ui-manual.md` §12 + `docs/operations.md` |
| 5-Layer 마진 산식 | `docs/deployment-scenario-ha-channel.md` §5.7 |
| 다중 사이트 배포 | `docs/deployment-scenario-multi-site.md` |

---

## 15. 새 디렉토리 / 파일 추가 시 가이드

### 15.1 새 서비스 (Go 바이너리)

1. `cmd/<svc>/main.go` 생성 → `make build` 가 자동 picking up
2. 비즈니스 로직은 `internal/<svc>/` 에
3. 공유 가능한 도메인이면 `pkg/<domain>/`
4. `docs/operations.md` 에 flag 추가
5. `etc/grafana/` 에 dashboard / alert 추가

### 15.2 새 설정 파일

1. `etc/<name>.json` (또는 yaml) — 단순 시드면 .json
2. 정적 파일 모드 + etcd watch 두 path 모두 지원하도록 코드 작성
3. `etc/db-mirror/` 같은 외부 DB 미러면 fx-sync 의 Backend 추가
4. 본 문서 §6 에 한 줄 추가

### 15.3 새 proto RPC

1. `api/proto/wtg/v1/<service>.proto` 신설 또는 기존 파일에 method 추가
2. `make proto` → `pkg/wtgpb/v1/` 갱신
3. 서버 구현은 `internal/<svc>/`, 클라이언트는 호출 위치
4. `docs/conventions.md` 에 service 카탈로그 갱신

### 15.4 새 문서

1. `docs/<name>.md`
2. 본 문서 §11 의 카테고리에 한 줄 추가
3. `CLAUDE.md` 의 "참고 문서" 섹션에도 인덱스

---

## 16. 참고 문서

- `CLAUDE.md` — 프로젝트 한 페이지 요약
- `README.md` — 소개 + 디렉토리 구조 (간략)
- `docs/admin-ui-manual.md` — 운영 콘솔 37 페이지
- `docs/deployment-software.md` — 설치할 모든 소프트웨어
- `docs/deployment-scenario-ha-channel.md` — 단일 사이트 시나리오 + 5 단계 멘탈모델
- `docs/deployment-scenario-multi-site.md` — 다중 사이트 시나리오
- `docs/operations.md` — 서비스 flag/env + 운영 작업
- `docs/conventions.md` — ApplName/Channel/Exchange/RoutingKey 카탈로그
