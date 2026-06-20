# 배포 시나리오 — HA + 채널 분리 (실사용 예제)

> 단일 사이트의 외환 트레이딩 게이트웨이 운영 구성 예제.
> 본 문서는 추상 명세가 아니라 **실제로 동작하는 한 가지 배치** 의 설정을 그대로 적어둔다.
> 다른 시나리오 (다중 사이트, GSLB 등) 는 별도 문서로.

---

## 1. 요구 사항

다음 세 가지가 본 시나리오의 출발점:

1. **채널 분리**
   - **외부 고객** : `WEB` 채널 (웹/모바일 웹)
   - **영업점 직원 / 본점 직원** : `CS` 채널 (Visual C++ legacy 단말 + 차세대 단말)
   - 모바일 앱 `MOB` 은 본 시나리오에서 미사용
2. **AP 서버 HA** : Active-Standby 2대. 동시에 하나만 active
3. **AP 서버 동거 컴포넌트** : 같은 서버에 `mymqd broker + 매매 AP + 매칭엔진` 가 같이 올라감

> 만약 위 요구사항이 다르면 (예: AP 가 active-active, 또는 broker 가 별도 서버) 본 시나리오의 설정 값을 일부 조정해야 한다. 영향 받는 항목은 각 절의 **(시나리오 의존)** 표시로 명시.

---

## 2. 서버 배치

### 2.1 하드웨어 / 노드 목록

| 노드                    | 역할             | 호스팅 컴포넌트                                                            |
| --------------------- | -------------- | ------------------------------------------------------------------- |
| `ap1.internal`        | **AP active**  | mymqd (11217/11218), 매매 AP (test_service / WECHO / W*), 매칭엔진        |
| `ap2.internal`        | **AP standby** | mymqd (11217/11218 — standby), 매매 AP (대기), 매칭엔진 (대기)                |
| `int1.internal`       | Internal 서비스 1 | mci-api, mci-push, mci-price, mci-chart, mci-admin                  |
| `int2.internal`       | Internal 서비스 2 | mci-api, mci-push, mci-price, mci-chart (HA 짝)                      |
| `dmz1.internal`       | DMZ 1          | mci-edge-api, mci-edge-push, mci-edge-price, mci-edge-chart         |
| `dmz2.internal`       | DMZ 2          | mci-edge-api, mci-edge-push, mci-edge-price, mci-edge-chart         |
| `fwd1.internal`       | 시세 수신          | quote-forwarder (UDP 30044~30051)                                   |
| `etcd1/2/3.internal`  | etcd 클러스터      | etcd 3 node                                                         |
| `redis1/2/3.internal` | Redis Sentinel | Redis master + sentinel × 3                                         |
| `db1.internal`        | TimescaleDB    | quote_bars                                                          |
| `obs1.internal`       | 관측 stack       | Prometheus + Alertmanager + Grafana + OTel collector + Tempo + Loki |

> **노트** — Internal/DMZ 가 1대씩만 있는 단순 구성이라면 `int2`/`dmz2` 를 생략. 다만 mci-edge-price 의 **N6 다중 인스턴스 fan-out** 효과를 보려면 최소 2대.

### 2.2 토폴로지

```
                  외부 (고객)                       내부 (영업점 / 본점)
                  ──────────                       ──────────────────────
                  WEB 브라우저                       CS Visual C++ / 신규 단말
                       │                                      │
                       │ HTTPS / WSS                          │ HTTPS / 사내망 ws
                       ▼                                      │
              ┌──────────────────────┐                       │
              │  사내 LB (HAProxy)   │ ◄─── TLS termination   │
              └──────────┬───────────┘                       │
                         │                                    │
            ┌────────────┴────────────┐                       │
            ▼                         ▼                       ▼
   ┌──────────────────┐      ┌──────────────────┐    ┌────────────────────┐
   │  dmz1.internal   │      │  dmz2.internal   │    │  int*.internal     │
   │    mci-edge-*    │      │    mci-edge-*    │    │   (CS 는 사내 직결) │
   └────────┬─────────┘      └────────┬─────────┘    └─────────┬──────────┘
            └────────┬────────────────┘                        │
                     ▼                                         ▼
          ┌─────────────────────────────────────────────────────────┐
          │                Internal (int1/2.internal)               │
          │   mci-api  mci-push  mci-price  mci-chart  mci-admin    │
          └─────┬──────────┬──────────┬──────────┬──────────────────┘
                │          │          │          │
                ▼          ▼          ▼          ▼
            etcd        Redis     TimescaleDB   관측
                                                  
                Internal 서비스 → AP (mymq wire)
                                  ────────────────────
                                  ▼
              ┌──────────────────────────────────────────┐
              │  AP 서버 (active-standby)                 │
              │                                          │
              │    ap1.internal (ACTIVE)                 │
              │      mymqd :11217 / cluster :11218       │
              │      매매 AP   (WECHO / W* / BW*)        │
              │      매칭엔진  (cside/wtgprice 호출)     │
              │                                          │
              │    ap2.internal (STANDBY — 따라가기)      │
              │      mymqd :11217 / cluster :11218       │
              │      매매 AP  (idle)                     │
              │      매칭엔진 (idle)                     │
              └──────────────────────────────────────────┘

   quote-forwarder (fwd1) ─── UDP 30044~30051 ◄── 외부 FX feed (SMB/KMB/EBS/REUT)
                            └─ gRPC publish → mci-price :50051
```

### 2.3 active-standby 동작

- broker cluster port `11218` 로 ap1 ↔ ap2 가 서로 heartbeat. active 가 죽으면 standby 가 active 로 promote.
- WTG (mci-api/push/price) 는 broker 주소를 **VIP** (또는 DNS round-robin) 로 잡아 active 만 보게 한다. failover 시 supervisor goroutine 이 자동 재연결 (`docs/broker-reconnect.md`).
- 매매 AP / 매칭엔진은 broker 와 동일 노드 — broker 가 죽으면 둘 다 같이 죽고 standby 노드의 짝이 올라옴.

---

## 3. 도메인 모델 — Channel × Site × Tier 카탈로그

본 시나리오의 정책 결정에 따라 다음 12개 Profile 을 사용:

| # | Profile.Key()       | 누가 | 비고 |
|---|---------------------|---|---|
| 1 | `WEB.HQ.VIP`        | 본점 직거래 고객 (VIP) | spread 최소 |
| 2 | `WEB.HQ.GOLD`       | 본점 직거래 고객 (GOLD) |  |
| 3 | `WEB.HQ.STD`        | 본점 직거래 고객 (일반) |  |
| 4 | `WEB.BRANCH.VIP`    | 영업점 소속 고객 (VIP) | 영업점 마진 가산 |
| 5 | `WEB.BRANCH.GOLD`   | 영업점 소속 고객 (GOLD) |  |
| 6 | `WEB.BRANCH.STD`    | 영업점 소속 고객 (일반) | spread 가장 넓음 |
| 7 | `CS.HQ.VIP`         | 본점 트레이더 (VIP) | 시장가에 거의 수직접근 |
| 8 | `CS.HQ.STD`         | 본점 일반 직원 |  |
| 9 | `CS.BRANCH.VIP`     | 영업점 dealing 데스크 |  |
| 10 | `CS.BRANCH.STD`    | 영업점 일반 직원 |  |
| 11 | `CS.HQ.AUDIT`      | (선택) 감사용 read-only 등급 — Tier 신설 필요 |  |
| 12 | `CS.HQ.OPS`        | (선택) 운영자 — admin UI / SOP 접근 — Tier 신설 필요 |  |

> Tier 가 4종 (`VIP/GOLD/STD/AUDIT/OPS`) 이상 필요하면 코드의 `Tier` enum 에 추가 — `pkg/session/types.go` 의 `Tier` 정의 갱신.

### 3.1 admin UI 의 `profiles` 페이지에 등록할 데이터

```json
[
  {"channel": "WEB", "site": "HQ",     "tier": "VIP"},
  {"channel": "WEB", "site": "HQ",     "tier": "GOLD"},
  {"channel": "WEB", "site": "HQ",     "tier": "STD"},
  {"channel": "WEB", "site": "BRANCH", "tier": "VIP"},
  {"channel": "WEB", "site": "BRANCH", "tier": "GOLD"},
  {"channel": "WEB", "site": "BRANCH", "tier": "STD"},
  {"channel": "CS",  "site": "HQ",     "tier": "VIP"},
  {"channel": "CS",  "site": "HQ",     "tier": "STD"},
  {"channel": "CS",  "site": "BRANCH", "tier": "VIP"},
  {"channel": "CS",  "site": "BRANCH", "tier": "STD"}
]
```

→ 이를 `etc/profiles.json` 으로 두고 admin 의 초기 seed 로 사용하거나, admin UI 의 `🧩 프로파일` 페이지에서 수동 추가.

### 3.2 사용자 매핑 예 (admin UI 의 `👥 사용자 프로파일`)

| usid | profile_key | roles |
|---|---|---|
| `customer-1234@partner.co.kr` | `WEB.BRANCH.VIP` | `[]` |
| `customer-5678@example.com` | `WEB.HQ.STD` | `[]` |
| `branch-dealer-01@bank.co.kr` | `CS.BRANCH.VIP` | `[]` |
| `hq-trader-cr@bank.co.kr` | `CS.HQ.VIP` | `[]` |
| `ops-cr@bank.co.kr` | `CS.HQ.STD` | `["admin"]` |

> 운영 환경에선 보통 외환 운영 DB (Oracle) → `fx-sync` CLI 가 etcd 미러링. 본 표는 그 결과의 한 단면.

---

## 4. 라우팅 — broker `exchange` × `routing_key` × `alias`

WTG 의 `mci-api` 는 모든 매매를 **단일 `POST /v1/tx`** 로 받고 `alias → exchange + routing_key` 로 분기. 본 절은 그 세 단어가 각각 무엇이고 왜 이렇게 나뉘었는지부터 정리한다.

### 4.1 exchange / routing_key / alias 의 이해

#### 4.1.1 HTTP 와 1:1 매핑하면 직관이 잡힌다

```
HTTP                            mymq broker
────                            ───────────
http://api.example.com/    →    exchange="API"
        ↑                                ↑
      도메인                          "어느 서비스(프로세스)로"

        /v1/orders             →    routing_key="ORDERS"
              ↑                              ↑
            path                "그 서비스의 어느 transaction"
```

- **exchange** = 한 매매 AP 프로세스 그 자체 (운영자가 평소 "서비스" 라고 부르는 단위)
- **routing_key** = 그 프로세스가 처리하는 transaction 한 종류 (HTTP 의 path / endpoint 와 같다)

#### 4.1.2 본 시나리오의 `ap1` 서버 안에서 보면

ap1 한 대 안에 mymqd 1개 + 매매 AP 여러 프로세스가 떠 있다. 각 AP 가 자기 이름을 `exchange` 로 broker 에 등록하고, 그 프로세스 안에서 여러 transaction 을 처리한다:

```
ap1.internal   (한 대 서버)
│
├─ mymqd  (broker — 라우팅 허브)
│
├─ AUTH       프로세스  ◄── exchange="AUTH"        (로그인 서비스)
│    ├─ routing_key="LOGIN"        ← 고객 로그인
│    └─ routing_key="CS_LOGIN"     ← 직원 로그인
│
├─ QUOTESVC   프로세스  ◄── exchange="QUOTESVC"    (호가 조회 서비스)
│    ├─ routing_key="GET"          ← 단발 조회
│    └─ routing_key="SUBSCRIBE"    ← 구독
│
├─ WECHO      프로세스  ◄── exchange="WECHO"       (고객용 매매 서비스)
│    ├─ routing_key="NEW"          ← 신규 주문
│    ├─ routing_key="CANCEL"       ← 취소
│    └─ routing_key="SWAP_CONFIRM" ← swap 확정
│
└─ BWECHO     프로세스  ◄── exchange="BWECHO"      (직원용 매매 서비스 — B prefix)
     ├─ routing_key="NEW"          ← 직원 신규
     └─ routing_key="CANCEL"
```

ap2 (standby) 의 안 구조도 같다 — 같은 프로세스 묶음이 idle 상태로 대기.

> **한 줄 요약** : `exchange` 는 "어느 프로세스로" (= 서비스 list 의 어느 항목), `routing_key` 는 "그 프로세스 안의 어느 transaction".

#### 4.1.3 왜 1개로 안 합치고 2개로 나눴나

서비스(프로세스) 의 lifecycle 과 transaction 의 lifecycle 이 다르기 때문이다.

| 단위 | 무엇이 일어나는가 |
|---|---|
| **exchange (프로세스)** | 시작 / 종료 / 부하분산 (multi-instance) / failover |
| **routing_key (transaction)** | 신규 추가 / 권한 차등 / 통계 분리 |

broker 가 메시지를 받으면 다음 2단 라우팅을 한다:

1. `exchange` 로 **어느 프로세스 그룹** 에 보낼지 결정 (예: WECHO instance 가 3개면 그중 하나 골라서)
2. `routing_key` 를 메시지 헤더(mqhdr)에 박아 전달 → 받은 AP 가 **어떤 transaction 인지** 판별해 자기 내부 함수로 분기

HTTP 의 nginx 가 domain 으로 backend pool 고르고 path 로 backend handler 분기하는 것과 똑같은 그림.

#### 4.1.4 그럼 `alias` 는 왜 또 있나

`exchange + routing_key` 가 broker 의 **raw 좌표** 라면, **`alias` 는 운영자/클라이언트가 쓰는 별명** 이다:

```
클라이언트 호출         POST /v1/tx { "alias": "WEB_ORDER_NEW", "data": ... }
                              │
                              ▼ (mci-api 가 etcd 의 라우팅 룰로 변환)
broker 가 받는 좌표   exchange="WECHO",  routing_key="NEW"
```

별명을 두는 이유 3가지:

1. **이름 자유** — `WEB_ORDER_NEW` 가 `WECHO/NEW` 보다 의미 명확
2. **이전(rename) 자유** — 내일 broker AP 가 `WECHO/NEW` → `WECHO_V2/CREATE` 로 옮겨도 **클라이언트 코드는 그대로** `WEB_ORDER_NEW` 만 부르면 된다. mci-admin 의 `🔀 라우팅 룰` 페이지에서 룰 한 줄만 갱신
3. **채널 분리** — 같은 broker 의 `WECHO/NEW` 라도 운영자가 `WEB_ORDER_NEW` (WEB 채널) / `CS_ORDER_NEW` (CS 채널) 로 alias 를 따로 만들면 rate-limit / 매매 통계 (alias × tier) / 감사 로그가 채널별로 깔끔하게 갈라진다

#### 4.1.5 본 시나리오의 한 줄 매핑

| 운영자 alias (별명) | broker 좌표 | 누가 호출 |
|---|---|---|
| `WEB_LOGIN`        | exchange=`AUTH`,     rkey=`LOGIN`     | 외부 고객 |
| `CS_LOGIN`         | exchange=`AUTH`,     rkey=`CS_LOGIN`  | 영업점/본점 직원 |
| `WEB_QUOTE_GET`    | exchange=`QUOTESVC`, rkey=`GET`       | 외부 고객 |
| `CS_QUOTE_GET`     | exchange=`QUOTESVC`, rkey=`GET`       | 직원 (같은 broker transaction, 통계용으로만 alias 분리) |
| `WEB_ORDER_NEW`    | exchange=`WECHO`,    rkey=`NEW`       | 외부 고객 |
| `CS_ORDER_NEW`     | exchange=`BWECHO`,   rkey=`NEW`       | 직원 (broker AP 자체가 다른 프로세스) |
| `WEB_ORDER_CANCEL` | exchange=`WECHO`,    rkey=`CANCEL`    |  |
| `CS_ORDER_CANCEL`  | exchange=`BWECHO`,   rkey=`CANCEL`    |  |

> 채널별 alias 가 같은 broker AP (예: `WEB_QUOTE_GET` / `CS_QUOTE_GET` 둘 다 `QUOTESVC/GET`) 를 가리켜도 된다 — 호출 흐름을 채널별로 구분해 통계/감사하기 위함.

#### 4.1.6 자주 받는 질문

- **Q. exchange 와 프로세스가 항상 1:1 인가?**
  대부분 1:1. 단 같은 exchange 를 여러 인스턴스가 등록하면 broker 가 자동으로 부하분산 (예: `WECHO` 3개 instance). 이 경우 1:N — 그래도 exchange 는 "프로세스 그룹 한 단위" 로 보면 된다.
- **Q. routing_key 는 미리 다 정의해야 하나?**
  매매 AP 가 자기가 처리할 transaction 들의 routing_key 를 코드에 박아둔다. mci-admin 의 `📋 서비스 명세` 페이지에 이 카탈로그가 노출 — 운영자는 거기 보고 라우팅 룰 만든다.
- **Q. 같은 routing_key 가 다른 exchange 에 동시에 있어도 되나?**
  된다. `WECHO/NEW` 와 `BWECHO/NEW` 는 별개 — 다른 프로세스이므로. 라우팅은 항상 (exchange, routing_key) 쌍으로.
- **Q. alias 가 broker 로도 흘러가나?**
  안 간다. alias 는 mci-api 안에서만 살아있고, broker 로 가는 mymq wire 에는 `exchange` + `routing_key` 만 박힌다. broker 는 alias 라는 개념을 모른다.

### 4.2 admin UI 의 `🔀 라우팅 룰` 페이지 등록값

| alias                   | exchange   | routing_key    | 누가 호출    | 설명                                |
| ----------------------- | ---------- | -------------- | -------- | --------------------------------- |
| `WEB_LOGIN`             | `AUTH`     | `LOGIN`        | WEB      | 고객 로그인 (mci-api 가 호출하는 broker AP) |
| `WEB_QUOTE_GET`         | `QUOTESVC` | `GET`          | WEB      | 고객 단발 호가 조회                       |
| `WEB_ORDER_NEW`         | `WECHO`    | `NEW`          | WEB      | 고객 신규 주문                          |
| `WEB_ORDER_CANCEL`      | `WECHO`    | `CANCEL`       | WEB      | 고객 주문 취소                          |
| `WEB_SWAP_LOCK_CONFIRM` | `WECHO`    | `SWAP_CONFIRM` | WEB      | 잠금된 swap 의 매매 확정                  |
| `CS_LOGIN`              | `AUTH`     | `CS_LOGIN`     | CS       | 직원 로그인                            |
| `CS_QUOTE_GET`          | `QUOTESVC` | `GET`          | CS       | 직원 호가 조회 (rate-limit 다름)          |
| `CS_ORDER_NEW`          | `BWECHO`   | `NEW`          | CS       | 직원 신규 주문 (broker 측 AP 다름 — B* 계열) |
| `CS_ORDER_CANCEL`       | `BWECHO`   | `CANCEL`       | CS       | 직원 주문 취소                          |
| `CS_INQUIRY`            | `BINQ`     | `LOOKUP`       | CS       | 직원 조회 (계좌/포지션)                    |
| `ADMIN_RELOAD`          | `ADMIN`    | `RELOAD`       | OPS only | mci-admin 운영 명령                   |

> **(시나리오 의존)** — `WEB_*` / `CS_*` alias prefix 는 운영자가 보기 좋은 이름. 실제 broker 의 exchange/routing_key 는 매매 AP 측이 결정.
> 같은 호가 조회라도 `WEB_QUOTE_GET` / `CS_QUOTE_GET` 으로 분리하면 rate-limit / 매매 통계 (alias × tier) / 감사 로그가 채널별로 깨끗하게 나뉜다.

### 4.3 dev-seed (초기 부트스트랩용 정적 룰)

> §4.4 로 이어지는 broker wire 모델은 라우팅 룰 뒤에 두고 읽어도 된다 — 룰만 알면 운영 가능하지만, 사고 디버깅 시 wire 모델을 알아야 broker 의 "navi 없음" / "Lost reply" 같은 에러 메시지를 해석할 수 있다.


`etc/routes.json` (또는 admin 의 `seed` 옵션):

```json
[
  {"alias": "WEB_LOGIN",          "exchange": "AUTH",     "routing_key": "LOGIN"},
  {"alias": "WEB_QUOTE_GET",      "exchange": "QUOTESVC", "routing_key": "GET"},
  {"alias": "WEB_ORDER_NEW",      "exchange": "WECHO",    "routing_key": "NEW"},
  {"alias": "WEB_ORDER_CANCEL",   "exchange": "WECHO",    "routing_key": "CANCEL"},
  {"alias": "WEB_SWAP_LOCK_CONFIRM","exchange": "WECHO",  "routing_key": "SWAP_CONFIRM"},
  {"alias": "CS_LOGIN",           "exchange": "AUTH",     "routing_key": "CS_LOGIN"},
  {"alias": "CS_QUOTE_GET",       "exchange": "QUOTESVC", "routing_key": "GET"},
  {"alias": "CS_ORDER_NEW",       "exchange": "BWECHO",   "routing_key": "NEW"},
  {"alias": "CS_ORDER_CANCEL",    "exchange": "BWECHO",   "routing_key": "CANCEL"},
  {"alias": "CS_INQUIRY",         "exchange": "BINQ",     "routing_key": "LOOKUP"},
  {"alias": "ADMIN_RELOAD",       "exchange": "ADMIN",    "routing_key": "RELOAD"}
]
```

### 4.4 broker wire 모델 — `Channel` / `ckey` / `navi` / `Queue` / publish vs send

§4.1~4.3 는 운영자가 알아야 할 라우팅 룰 수준. 본 절은 한 단계 아래 — broker (mymqd) 가 내부적으로 메시지를 어떻게 다루는지. 사고 디버깅, 새 매매 AP 도입, broker 에러 로그 해독에 필요하다.

#### 4.4.1 `Channel` — 같은 단어, 두 가지 의미

WTG 코드와 broker 에 모두 "Channel" 이 나오지만 **완전히 다른 개념** 이다. 헷갈리기 가장 쉬운 함정.

|           | 사용자 Channel                                    | broker Channel                                                                                    |
| --------- | ---------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| 어디서       | `pkg/session/types.go` 의 `Channel` enum        | `pkg/mymq/conventions.go` 의 `ChannelWeb` / `ChannelAdmin` 등 상수                                    |
| 값 예시      | `WEB`, `MOB`, `CS`                             | `ChannelWeb` (= 정수 또는 사전 정의 문자열), `ChannelAdmin`, `ChannelGuest` ...                              |
| 용도        | 사용자 진입 경로 분류. Profile.Key() 의 첫 토큰. 마진/시세 라우팅. | mymq client 가 broker 에 등록할 때의 **client 종류** 분류. broker 내부 라우팅에 사용.                                |
| 누가 정함     | 비즈니스 정책                                        | wire protocol 컨벤션                                                                                 |
| 본 시나리오 매핑 | WEB 고객 / CS 직원                                 | mci-api / mci-edge-* = `ChannelWeb`, mci-admin = `ChannelAdmin`, quote-forwarder = `ChannelAdmin` |

→ "고객은 WEB 채널" 이라고 했을 때의 Channel 은 **왼쪽** (사용자 Channel). broker 가 mci-api 를 `ChannelWeb` 으로 등록하는 건 **오른쪽** (broker Channel) — 이 둘은 우연히 비슷한 이름일 뿐 별개.

> 코드에서 `session.Channel("WEB")` 과 `mymq.ChannelWeb` 이 동시에 나오면 그건 첫 번째 = 사용자 Channel, 두 번째 = broker Channel.

#### 4.4.2 `ckey` — 한 connection 으로 동시 여러 RPC 굴리는 비결

```
mqhdr (100B 고정 헤더)
┌──────────────────────────────────────────────────────┐
│ origin[16]  dest[16]  exchange[16]  rkey[16]         │
│ dirf  errn  ckey[16]  trcid[16]   ...                │  ← ckey 가 16B
└──────────────────────────────────────────────────────┘
                          ▲
                  16바이트 correlation id
```

- HTTP/1.1 의 `X-Request-ID` 와 같은 역할.
- 클라이언트(mci-api) 가 요청 보낼 때 `ckey` 에 unique 값 박음 → broker 가 응답에 그 값을 그대로 echo back → 클라이언트가 "이 응답이 어느 요청의 것" 인지 매칭.
- 덕분에 **단일 TCP connection** 위에서 동시에 수십 개 RPC 가 inflight 가능 (HTTP/2 의 stream 과 같은 아이디어).
- WTG 의 `pkg/mymq.Client.Call/Send` 가 ckey 자동 발급 + 응답 매칭.

**비유** : 콜센터의 상담번호 — 한 회선으로 여러 상담원이 동시에 통화해도 상담번호로 식별.

**자주 보는 에러** : ckey echo 안 됨 → `pkg/mymq.Client` 가 응답 못 받아서 timeout. broker AP 가 응답에 ckey 안 박으면 발생. `mci-test --ckey-echo` 가 이걸 검증하는 Phase 1 GO/NO-GO.

#### 4.4.3 `navi` — origin/destination 라우팅 정보

mqhdr 안에는 `origin` (이 메시지가 어디서 왔는지) 와 `dest` (어디로 가야 하는지) 가 박힌다. broker 는 이걸로 다음 hop 을 결정.

- **transaction (RPC)** 일 때 — `origin = 호출자`, `dest = 받는 AP`. broker 가 dest 로 forward.
- **broadcast (publish)** 일 때 — `origin = 발행자`, **`dest 는 비움`**. broker 가 dest 가 비었음을 보고 "이건 broadcast" 로 인지 → exchange 의 모든 subscriber 에게 fan-out.

**핵심 함정** :
- broadcast 인데 dest 가 채워져 있으면 broker 가 transaction 으로 오인 → "Lost reply message" drop.
- transaction 인데 navi 비어있으면 broker 가 "no navigation" 으로 reject.
- → `pkg/mymq.Client.applyDefaults` 가 자동으로 origin / dest 채움 (broadcast 시는 skip + `Dirf = DirPublish` 보정). **수동으로 만지지 말 것.**

**비유** : 우편물의 "보낸 사람 / 받는 사람" — 양쪽 다 채워져 있어야 등기, 보낸 사람만 채우면 전단지 (broadcast).

#### 4.4.4 `Queue` — receiver 가 broker 에 자기 큐 등록

매매 AP / mci-push 같은 **받는 쪽** 은 자기를 broker 에 등록하면서 "내 큐 이름" 을 알려준다. broker 는 그 큐로 메시지를 넣는다.

| 등록 방식 | Queue 값 | broker 가 보는 client type | 본 시나리오에서 누가 |
|---|---|---|---|
| 일반 transaction receiver | `Options.Queue = ExchangeName` (예: `WECHO`) | 보통 client | 매매 AP (WECHO / BWECHO / QUOTESVC) |
| representative receiver | **빈값** | `_CLIENT_` type | mci-push 의 rep receiver (`QF_UNSOL_REP` flag) |
| subscribe (broadcast 수신) | `Options.Queue = ExchangeName` 명시 + `Channel = ChannelAdmin/Web` | 일반 client + subscribe flag | mci-price 의 시세 수신, mci-push 의 사용자 unsolicited 수신 |

**핵심 함정** :
- subscribe receiver 가 `Options.Queue` 안 채우면 broker `publish.c:223` 의 `strcasecmp(client->xchg, msg.xchg)` 매칭이 실패 → 매 publish 마다 "Published 0/N" 으로 skip.
- rep receiver 는 정확히 **빈값** 이어야 한다 (`publish.c:185-189` 참조). 무엇이든 값을 넣으면 일반 client 로 등록되어 broadcast 안 옴.

**비유** : 우체국 사서함. Queue 이름 = 사서함 번호. 빈값 = "지점장 직접 수령함" (특수 사서함).

#### 4.4.5 publish vs send — broker 의 두 가지 메시지 전송 방식

mymq wire 는 **publish (broadcast)** 와 **send (transaction RPC)** 의 두 가지 path 가 있다. 같은 wire format 인데 navi / dirf 가 다르게 채워져서 broker 가 다르게 처리.

| | send (RPC) | publish (broadcast) |
|---|---|---|
| 호출자 | mci-api `Client.Call(req)` | quote-forwarder, cooker, mci-price |
| dirf | `DirRequest` / `DirReply` | `DirPublish` |
| navi | 양쪽 채움 | dest 비움 |
| broker 동작 | exchange/rkey → dest 로 1:1 forward + 응답 회수 | exchange 의 모든 subscriber 에게 fan-out |
| 응답 | broker 가 응답 메시지 회수해 호출자에게 (ckey 매칭) | 없음 (fire-and-forget) |
| 본 시나리오 사례 | `WEB_ORDER_NEW` 호출 | 시세 (PRICE exchange) — `quote-forwarder → mci-price` |

**핵심 함정** : `Client.Send` 에 `Xchg` 를 `FrameInput.Xchg` 로 넣으면 navi 자동채움 트리거 → broker 가 publish_packet 대신 message_packet_transfer 로 분기 → "Lost reply message" drop. broadcast 는 반드시 `BroadcastHeader.Exchange` 에 박을 것 (`pkg/mymq.Client` 이 자동 처리).

#### 4.4.6 wire 모델 한눈에 — 본 시나리오 메시지 흐름 4가지

```
[1] WEB 고객 매매 (transaction)
    WEB 브라우저 → mci-edge-api(8090) → mci-api(8080) → Client.Call
        ──[ send: navi 양쪽 채움, ckey=N, dirf=DirRequest ]──►
    mymqd(11217)  ──[ Queue=WECHO 매칭 → forward ]──►  WECHO AP (ap1)
    WECHO 응답  ──[ ckey echo, dirf=DirReply ]──►  mymqd  ──►  mci-api → 브라우저

[2] CS 직원 매매 (transaction)
    CS 단말 → mci-api(8080) → ... → mymqd ──► BWECHO AP (ap1)   ← exchange 만 다름

[3] 시세 broadcast (publish)
    quote-forwarder ──[ publish: dest 비움, dirf=DirPublish, Exchange=PRICE ]──►
    mymqd  ──[ PRICE exchange 의 subscriber 모두 ]──►  mci-price × N

[4] push unsolicited (rep receiver)
    매매 AP (WECHO) ──[ publish: 80B broadcast prefix 에 LogonID 첨부 ]──►
    mymqd  ──[ rep receiver = Queue 빈값 client 에게 모두 ]──►  mci-push
    mci-push 가 LogonID 매칭해서 해당 사용자 ws 로 fan-out
```

#### 4.4.7 자주 받는 질문

- **Q. broker 가 죽으면 inflight RPC 의 ckey 는 어떻게 되나?**
  `pkg/mymq.Client` 의 supervisor goroutine 이 connection 끊김 감지 → inflight 전부 `mymq.Error{inflight_aborted}` 로 reject → 클라이언트에 503. ckey 자체는 재연결 후 새 값으로 갱신.
- **Q. 한 사용자 (Profile=WEB.BRANCH.VIP) 가 동시에 호가 조회 + 주문을 보내면?**
  mci-api 는 단일 broker connection. 호가 조회와 주문이 각자 다른 ckey 로 박혀서 동시 inflight. broker 가 두 응답을 각자의 ckey 로 echo back → mci-api 가 매칭해서 두 사용자 요청에 각각 응답.
- **Q. `Options.Channel = ChannelWeb` 으로 등록한 mci-api 가 `ChannelAdmin` 메시지도 받을 수 있나?**
  못 받는다. broker 가 channel 단위로도 라우팅 — `ChannelAdmin` 메시지는 `ChannelAdmin` 으로 등록된 client (= mci-admin, quote-forwarder) 만 받는다. 그래서 quote-forwarder 가 admin channel 로 시세를 publish 하면 그 채널 받는 mci-price 가 ChannelAdmin 또는 multi-channel 등록 필요.
- **Q. trcid (trace_id) 와 ckey 는 다른가?**
  다르다. ckey = 한 RPC pair (요청↔응답) 매칭용, 짧게 (보통 ms 단위 lifetime). trcid = 분산 trace 전체 식별, end-to-end (브라우저 → mci-edge → mci-api → broker → AP → DB) 일관 유지. 둘 다 mqhdr 안에 박힌다. `docs/broker-tracing.md` 참조.

### 4.5 시세 파이프라인 — raw tick 부터 사용자 호가까지

매매 transaction 은 §4.1~4.4 의 broker 라우팅으로 충분하지만, **시세** 는 다른 path 를 탄다 — broker 의 publish 또는 gRPC PublishTick 으로 들어와서 여러 단계의 변환을 거친다. 본 절은 그 파이프라인을 단계별로 푼다.

#### 4.5.1 한 장으로 본 시세 파이프라인

```
[입력] 외부 FX feed (Bloomberg / Reuters / EBS ...)
        │  UDP FIX 4.4 (35=W / 35=X)
        ▼
   ┌────────────────────────────┐
   │ quote-forwarder (fwd1)     │  reader (UDP 수신) + worker (parse + batch)
   │  - feed = SMB / KMB / EBS  │  pair 별 카탈로그 매칭, invalid_quote 분류
   └─────────────┬──────────────┘
                 │  publish-mode = grpc  →  gRPC PublishTick(Tick)
                 ▼
   ┌────────────────────────────────────────────────────────────────────────┐
   │ mci-price (int1, int2 — N 인스턴스)                                     │
   │                                                                        │
   │  ① broker subscribe + gRPC PublishTick + DevTick 의 단일 입구           │
   │      Tick{Source, Symbol, Bid, Ask, Ts}                                │
   │      ↓                                                                 │
   │  ② BestConsumer  ◄── per (Symbol, Source) 캐시. 매 tick max(bid)/min(ask)│
   │      합성 Tick{Source="BEST", Symbol, Bid=max, Ask=min}                 │
   │      crossed (best_bid > best_ask) → 최신 ts feed 호가로 fallback        │
   │      ↓                                                                 │
   │  ③ CrossConsumer ◄── 직접 호가가 없는 pair 의 합성. (예: EURKRW 직접 호가 X │
   │      → EURUSD × USDKRW 로 합성). 합성 결과도 Source="BEST" tick.         │
   │      ↓                                                                 │
   │  ④ Aggregator  ────► (옆 줄기) close 시 1s/1m/5m/15m/1h/1d 봉 close      │
   │                       Archiver → TimescaleDB INSERT                    │
   │                       gRPC SubscribeBar → mci-chart                    │
   │      ↓                                                                 │
   │  ⑤ PricingConsumer ◄── PricingTable.Apply (5-Layer 마진)                │
   │      - Profile 별 : HQ + Site spread/skew                              │
   │      - Customer 별: HQ + Site + Customer + Window + Swap (5 layer)     │
   │      ↓                                                                 │
   │  ⑥ MultiQuotePublisher ── fan-out 3 갈래 :                              │
   │      a) broker ExchangeQuote (TOPIC, routing-key=Profile.Key())        │
   │      b) gRPC PriceService.SubscribeQuote (Profile-routed CustomerQuote)│
   │      c) gRPC PriceService.SubscribeCustomerQuote (5-Layer customer)    │
   └─────────────┬──────────────────────────────────────────────────────────┘
                 ▼
   ┌────────────────────────────────────────────────────────────────────────┐
   │ mci-edge-price (dmz1, dmz2)                                            │
   │   3 stream 동시 구독 → 단일 ws 에 합쳐 외부 ws 클라이언트 fan-out         │
   │     - SubscribeTick   (raw BEST)                                       │
   │     - SubscribeQuote  (Profile, JWT.profile_key 로 라우팅)              │
   │     - SubscribeCustomerQuote (5L, RegisterCustomer 등록자 한정)         │
   └─────────────┬──────────────────────────────────────────────────────────┘
                 ▼
       브라우저 / CS 단말 (호가창)
```

#### 4.5.2 단계별 의미

| 단계 | 컴포넌트 | 입력 → 출력 | 책임 |
|---|---|---|---|
| ① 입구 | mci-price | `Tick{Source, Symbol, bid, ask, ts}` | 3 source (broker / gRPC PublishTick / DevTick) 를 한 channel 로 모음. exchange 필터 (`PRICE` 만 처리). |
| ② BestConsumer | mci-price | source-별 tick → `Tick{Source="BEST"}` | 다중 feed 의 합성 best 호가. mds 의 `mdssise_make_best` 와 동일 모델. |
| ③ CrossConsumer | mci-price | direct pair → cross pair | 직접 호가 없는 pair 를 cross 합성 (예: EURKRW = EURUSD × USDKRW). 마찬가지로 Source="BEST". |
| ④ Aggregator | mci-price | BEST tick → 6 timeframe 봉 | 1s/1m/5m/15m/1h/1d (UTC bucket). close 시 ⑤ 의 옆 줄기로 Archiver + SubscribeBar fan-out. |
| ⑤ PricingConsumer | mci-price | BEST tick + PricingTable → CustomerQuote | Profile 별 호가 (HQ+Site) 와 5L 호가 (HQ+Site+Customer+Window+Swap) 두 갈래 계산. |
| ⑥ MultiQuotePublisher | mci-price | CustomerQuote → 3 stream | broker ExchangeQuote (TOPIC) + gRPC SubscribeQuote + SubscribeCustomerQuote. |
| ⑦ 외부 fan-out | mci-edge-price | 3 gRPC stream → 단일 ws | JWT 의 profile_key 로 Profile quote 라우팅, RegisterCustomer 한 사용자만 5L quote 받음. |

#### 4.5.3 본 시나리오 노드 매핑

| 단계 | 노드 |
|---|---|
| 외부 feed → UDP | 외부 (Bloomberg / Reuters / EBS / 한국 시장 feed) |
| ① quote-forwarder | `fwd1.internal` UDP 30044~30051, gRPC publish → int1:50051 |
| ② ~ ⑥ mci-price | `int1.internal` + `int2.internal` (각자 독립 BestConsumer/Aggregator/PricingConsumer) |
| 봉 영속 (④ 옆 줄기) | `db1.internal` TimescaleDB.quote_bars |
| 봉 라이브 차트 | `int1`/`int2` mci-chart → ws 클라이언트 |
| ⑦ mci-edge-price | `dmz1.internal` + `dmz2.internal` (N6 fan-out) |
| 외부 ws 클라이언트 | WEB 브라우저 (외부) / CS 단말 (사내) |

#### 4.5.4 두 종류의 CustomerQuote — 같은 ws 에 섞여 흐른다

`mci-edge-price` 의 단일 ws 에 다음 envelope 3종이 섞여 도착:

| 종류 | 구별 키 | 누구에게 |
|---|---|---|
| **BEST raw tick** | `data.src = "BEST"` | 모든 ws 클라이언트 (broadcast) |
| **Profile CustomerQuote** | `type="quote"`, `customer_id` 없음 | JWT 의 `profile_key` 가 등록된 Profile 이면 |
| **5-Layer CustomerQuote** | `type="quote"`, `customer_id` 있음 | RegisterCustomer 한 사용자에 한해 본인 customer_id 의 것만 |

> 본 시나리오의 admin UI `💱 시세 (라이브 ws)` 페이지가 위 3종을 각자 카드로 카운트한다.

#### 4.5.5 본 시나리오 시점별 호가 흐름 (USDKRW 한 tick)

```
14:32:15.001  Bloomberg → UDP FIX 35=W → fwd1:30044
                  Source=SMB, bid=1380.50, ask=1381.20

14:32:15.001  fwd1 worker 가 parse → gRPC PublishTick → int1:50051
14:32:15.002  int1 BestConsumer 캐시 업데이트
                  USDKRW SMB → bid=1380.50 ask=1381.20
                  (이미 EBS 도 활성 → max(bid)/min(ask) 비교)
                  best = bid=1380.50 ask=1381.20  (SMB 만 있다면 그대로)
                  → BEST tick fan-out

14:32:15.003  CrossConsumer  : direct pair → skip
14:32:15.003  Aggregator     : 1s 봉 누적, 15초가 되면 close

14:32:15.004  PricingConsumer 5L 계산 (Profile=WEB.BRANCH.VIP, customer=cr) :
                  raw      bid=1380.50 ask=1381.20
                  HQ       (±0.05)  bid=1380.45 ask=1381.25
                  Site BR  (±0.05)  bid=1380.40 ask=1381.30
                  Customer VIP(0)   bid=1380.40 ask=1381.30
                  Window   (낮)     bid=1380.40 ask=1381.30   ← Final

14:32:15.005  MultiQuotePublisher → 3 stream 동시 발사 :
                  a) broker.Publish(Exchange=ExchangeQuote, rkey=WEB.BRANCH.VIP, ...)
                  b) gRPC SubscribeQuote stream  → mci-edge-price 모든 인스턴스
                  c) gRPC SubscribeCustomerQuote → 같은 인스턴스, customer_id 별

14:32:15.005  dmz1 mci-edge-price 가 3 stream 모두 수신,
              ws 클라이언트별 fan-out :
                - 비로그인/다른 Profile : BEST raw 만
                - WEB.BRANCH.VIP 로그인 : BEST raw + Profile quote
                - 위 + RegisterCustomer(cr) : 거기에 + 5L quote(customer_id=cr)

14:32:15.006  브라우저 호가창 한 줄 갱신 (Pair=USD/KRW)
                  BEST     1380.50 / 1381.20
                  Profile  1380.40 / 1381.30
                  5L (cr)  1380.40 / 1381.30
```

#### 4.5.6 흔한 함정과 해석

| 증상 | 원인 | 어디서 보나 |
|---|---|---|
| `received` 만 증가, `matched` 가 안 늘어남 | exchange 필터 (PRICE) 매칭 실패 — broker subscribe Queue 이름 잘못 | mci-price logs `Published 0/N` 메시지 |
| `dropped > 0` | tick decoding 실패 — wire schema mismatch | mci-price `/v1/price-stats` |
| `sub_drops > 0` | broker subscribe 채널 buffer overrun | `Options.SubBufferSize` 늘리거나 부하 줄임 |
| `crossed_fallback = true` | 두 source 호가대가 어긋남 (best_bid > best_ask) | mci-price `/v1/best-stats` — 일시는 OK, 지속이면 source 하나가 stale |
| `conflation Swaps ≫ Updates 차이` 작음 | conflation 효과 적음 (tick rate ≈ consumer rate) | mci-price `/v1/price-stats.conflation` |
| `Profile quote 미수신` | JWT 의 profile_key 가 운영 Profile 카탈로그에 미등록 | mci-edge-price 의 SubscribeQuote profile_filter |
| `5L quote 미수신` | RegisterCustomer 호출 안 됨 (customer-stream flag 비활성 또는 클라이언트 미등록) | mci-price `/v1/customers` 응답이 빈 by_profile |
| `Aggregator 가 모든 tick silent drop` | SymbolMap 비어있음 — Pair 카탈로그 등록 누락 | mci-price 부팅 시 `SymbolMap 비어있음` WARN |
| `차트 historical 빈 화면` | Archiver DSN 미설정 또는 1m+ 봉 close 전 (1분 미만 dev) | mci-chart `/v1/chart` rows=0 |
| backpressure WARN 누적 | queue_cap < rate × consumer 지연 | mci-edge-price queue_cap 늘리기 또는 부하 줄이기 |

#### 4.5.7 자주 받는 질문

- **Q. quote-forwarder 가 publish-mode 를 broker 가 아니라 grpc 로 쓰면 broker 부담이 없어지나?**
  그렇다. 시세 publish 가 broker 우회 → mci-price 50051 직접. broker 의 publisher thread 부하 회피. 단 broker subscribe 로 받는 다른 컴포넌트 (legacy cs framework 등) 가 있으면 publish-mode=both 로 dual-write.
- **Q. mci-price 인스턴스가 여러 개면 BestConsumer 캐시가 분산되나?**
  분산된다. 각 인스턴스가 자기가 받은 tick 만 캐싱 → 인스턴스마다 BEST 가 미세하게 다를 수 있다. 운영에선 모든 mci-price 가 같은 source 를 받도록 grpc PublishTick fan-out 또는 broker subscribe 로 통일.
- **Q. PricingTable 변경 직후 라이브 호가가 즉시 바뀌나?**
  PricingConsumer 가 다음 tick 부터 새 마진 적용. 캐시된 5L customer quote 가 stale 하면 `🧮 마진 재계산` 트리거로 강제 갱신.
- **Q. broker 끊겼는데 시세는 어떻게 흐르나?**
  본 시나리오는 quote-forwarder 가 publish-mode=grpc 라 broker 와 무관. 매매는 막혀도 시세는 계속 흐름.

### 4.6 mci-push 의 두 트랙 — broker rep receiver vs HTTP push

`mci-push` 는 매매 엔진 / 운영 svc 가 사용자에게 **unsolicited 메시지** (체결 알림, 상태 변경, 시스템 공지) 를 fan-out 하는 핵심. 역사적/운영적 이유로 두 가지 path 가 병행되는 구조다 — 둘 다 같은 mci-push 가 받는다.

#### 4.6.1 왜 두 트랙이 같이 존재하나

- **트랙 A** = legacy. 매매 AP (WECHO 등) 가 broker 로 publish → broker 의 representative receiver 로 등록된 mci-push 가 받음. **wire 호환 보장** — broker 와 같은 mymq protocol.
- **트랙 B** = 신규 (Phase 2.x). 운영 svc 가 HTTP 로 mci-push 에 직접 push. **broker 우회**. 도입 동기 :
  - broker 가 SIGABRT 부하 상황에서 publisher thread 가 ceiling — `docs/broker-sigabrt-analysis.md`
  - 운영 C 코드에서 mymq 의존을 제거하고 싶음 (POSIX socket 만으로 충분)
  - C SDK `cside/wtgpush` 가 외부 의존 0 으로 만들어져 운영 매매 엔진의 어느 platform (AIX/Solaris/HP-UX/Linux/Darwin) 에서나 build

→ **두 트랙은 같이 동작한다.** 운영 의사결정은 "어느 호출자가 어느 트랙으로" 의 분담.

#### 4.6.2 트랙 A — broker representative receiver

```
매매 AP (WECHO)
  │
  │  publish: BroadcastHeader.Exchange="USERMSG",
  │           80B broadcast prefix 의 LogonID="cr01_WEB_VIP",
  │           dest 비움, dirf=DirPublish
  ▼
mymqd
  │  representative receiver 매칭 :
  │    Queue 가 빈값으로 등록된 _CLIENT_ type client 모두에게 forward
  │    (publish.c:185-189)
  ▼
mci-push (int1, int2) — QF_UNSOL_REP flag 로 부팅
  │
  │  Dispatcher : 80B prefix 의 LogonID 로 user 매칭
  │    - LogonID 비어있음 → 전체 broadcast (시스템 공지)
  │    - LogonID 채워짐  → 그 사용자의 ws 한정 fan-out
  ▼
mci-push 의 ws 클라이언트
  (mci-edge-push → 외부 ws)
```

**핵심 설정**
- mci-push 부팅 옵션 : `--broker host:port` + `--rep-receiver=true` (또는 ENV `QF_UNSOL_REP=1`)
- broker 에 등록할 때 `Options.Queue = ""` (반드시 빈값) → broker 가 `_CLIENT_` type 으로 분류 → broadcast 매핑.
- LogonID 컨벤션 : `Profile.Key()` + `usid` 의 인코딩 (예: `WEB.BRANCH.VIP|crlee123`) — 운영 합의 필요.

**본 시나리오** :
- mci-push 가 `int1` + `int2` 두 인스턴스로 떠 있고 둘 다 broker 에 rep receiver 로 등록 → broker 가 publish 한 메시지가 **양쪽 모두에 도착**.
- 같은 사용자의 ws 가 두 mci-push 에 분산되면 fan-out 이 중복 — 회피하려면 트랙 B 의 consistent hash ring 사용 (다음 절).

#### 4.6.3 트랙 B — HTTP push endpoint

```
운영 svc (예: 매칭엔진 / 정산 시스템 / 알림 봇)
  │  HTTP POST /v1/push
  │  Header : X-Push-Secret: <secret>   또는 mTLS client cert
  │  Body   : { logon_id, payload }
  ▼
mci-push (int1, int2)
  │
  │  ① 인증 :
  │     - X-Push-Secret 헤더 검증 (운영 정책상 secret-only 모드) — `docs/push-secret-rotation.md`
  │     - 또는 mTLS client cert 검증 (대안)
  │     - 둘 중 어느 쪽도 통과면 accept
  │
  │  ② consistent hash ring 으로 라우팅 :
  │     - 같은 logon_id → 항상 같은 mci-push instance 로 향함
  │     - virtual node 로 hash 재분배 최소화
  │     - 만약 호출자가 다른 instance 에 도달했으면 → MultiClient forwarder 가 sticky instance 로 reroute
  │
  │  ③ Dispatcher : ws 클라이언트로 fan-out (트랙 A 와 같은 path)
  ▼
mci-push 의 ws 클라이언트
```

**핵심 설정**
- mci-push 부팅 옵션 :
  - `--push-secret-file /etc/pki/wtg/push-secret`
  - `--push-mtls-ca /etc/pki/wtg/ca.crt` (둘 다 두면 OR)
  - `--push-instances int1.internal:8081,int2.internal:8081` (consistent hash ring 의 peer 목록)
- **standalone** : `mci-push --no-broker --listen :8081` 로 broker 없이 HTTP-only 부팅 가능 (PoC / 회귀 / 로컬 개발).
- **C SDK** : `cside/wtgpush/` 의 `wtg_push_send()` 한 줄로 운영 C svc 가 트랙 B 진입.

**본 시나리오 호출 예** (운영 C 매칭엔진 → 체결 알림):

```c
#include "wtgpush.h"

wtg_push_client_t cli;
wtg_push_client_init(&cli, "int1.internal", 8081);
wtg_push_client_set_secret(&cli, "/etc/pki/wtg/push-secret");  /* X-Push-Secret 헤더 자동 첨부 */

wtg_push_message_t msg = {
    .logon_id = "WEB.BRANCH.VIP|customer-1234@partner.co.kr",
    .payload  = "{\"event\":\"trade_filled\",\"order_id\":\"O-12345\",...}",
};
int rc = wtg_push_send(&cli, &msg);
if (rc != 0) {
    /* 재시도 정책은 호출자가 결정 — 보통 idempotency-key 첨부 후 retry */
}
```

#### 4.6.4 두 트랙 비교

| 항목 | 트랙 A (broker rep) | 트랙 B (HTTP push) |
|---|---|---|
| wire | mymq binary | HTTP/1.1 + JSON |
| 호출자 의존 | mymq 라이브러리 필수 | POSIX socket 만 (의존 0) |
| 인증 | broker connection auth | X-Push-Secret 또는 mTLS |
| broker 의존 | 100% | 0% (broker 죽어도 동작) |
| 다중 인스턴스 라우팅 | 모든 인스턴스에 fan-out (중복 위험) | consistent hash ring sticky (중복 없음) |
| 운영 C svc 부담 | mymq link + connection 관리 | POSIX socket + curl-수준 |
| 본 시나리오에서 누가 | 매매 AP (WECHO 등) — legacy 호환 | 매칭엔진 / 정산 / 알림 봇 — 신규 도입 |

> 트랙 B 가 운영 편의성 ↑, 트랙 A 는 broker 가 살아있는 한 wire 호환 ↑. **두 트랙 병행** 이 본 시나리오의 권장.

#### 4.6.5 본 시나리오 의사결정

| 호출자 | 트랙 | 이유 |
|---|---|---|
| 매매 AP (WECHO / BWECHO) | A | 이미 broker connection 보유 + 기존 publish 코드 재사용 |
| 매칭엔진 (libwtgprice 호출자) | B | mymq 의존 회피 + broker SIGABRT 부하 회피 |
| 정산 시스템 (야간 batch) | B | 외부 시스템 — broker connection 만들기 부담 |
| 알림 봇 (시스템 공지) | B | 단발성 — HTTP 가 간단 |
| 운영 admin (긴급 공지) | B | mci-admin → `🔌 API 테스터` 또는 별도 endpoint |

#### 4.6.6 흔한 함정

| 증상 | 원인 |
|---|---|
| 트랙 A 에서 publish 가 안 흐른다 (Published 0) | `Options.Queue` 가 빈값이 아님 — broker 가 일반 client 로 등록 → broadcast 안 옴 (`publish.c:185-189`) |
| 트랙 A 메시지가 중복 도착 | 두 mci-push 인스턴스 모두 rep receiver → 양쪽 ws 에 fan-out. 트랙 B 또는 user→instance sticky 필요 |
| 트랙 B 가 401 | `X-Push-Secret` 헤더 누락 또는 mTLS client cert 없음 |
| 트랙 B 가 다른 instance 로 도달 | 호출자가 라운드로빈으로 보냄 — consistent hash ring 의 forwarder 가 다시 sticky instance 로 reroute (정상 동작 — latency 1-hop 증가) |
| 트랙 B 가 secret 회전 후 401 | 회전 시 두 secret (`old+new`) 동시 유효 기간이 짧음 — `docs/push-secret-rotation.md` 의 절차 (`grace_period`) 준수 필요 |
| LogonID 매칭 실패 (메시지가 어디로도 안 감) | LogonID 컨벤션 (Profile.Key() + usid) 가 broker 측과 mci-push 측 불일치 — `pkg/mymq/conventions.go` 의 LogonID encoder 확인 |
| 브로드캐스트 (LogonID 빈값) 가 너무 많은 ws 에 폭주 | mci-edge-push 의 queue_cap 초과 → backpressure close. 공지 빈도 제한 또는 queue_cap 증대 |

#### 4.6.7 자주 받는 질문

- **Q. 트랙 A 와 B 를 동시에 같은 메시지에 사용하면?**
  같은 logon_id 에 대해 매매 AP (트랙 A) 와 매칭엔진 (트랙 B) 가 둘 다 push 하면 사용자가 같은 이벤트 2회 수신. 호출자 단계에서 분담 명확화 필요 (예: 매매 AP 는 매매 체결, 매칭엔진은 시장 상태).
- **Q. mci-push 가 죽으면?**
  consistent hash ring 에서 죽은 instance 가 제외되고 다른 instance 가 sticky 책임 가져감. 외부 ws 클라이언트는 mci-edge-push 가 LB → 살아있는 instance 자동 재연결.
- **Q. broker 끊어진 상태에서 트랙 A push 는?**
  drop. 매매 AP 가 broker connection 없으면 publish 자체가 fail. 트랙 B 는 broker 무관하게 계속 동작.
- **Q. 트랙 B 의 secret 은 어디에 보관?**
  파일 `/etc/pki/wtg/push-secret` (mode 0600, wtg user 만 read) + 운영 매매 엔진 측 별도 사본. 회전 절차는 `docs/push-secret-rotation.md`. Vault 같은 secret manager 도입하면 mci-push 가 init 시 fetch.
- **Q. consistent hash ring 의 instance 추가/제거 시 일부 사용자는 끊기나?**
  ring 의 일부 슬롯만 재분배 → 보통 1/N 의 사용자만 reroute. 끊김은 없지만 새 instance 로 옮긴 사용자는 잠깐 ws 재연결 (mci-edge-push 자동).

### 4.7 quote_id / swap_id — 시세 잠금과 매매 AP 검증

매매 직전에 사용자에게 보여준 호가가 실제 매매에도 동일하게 적용되도록 "한 시점의 호가를 잠그는" 메커니즘. 외환의 핵심 운영 안전장치.

#### 4.7.1 왜 필요한가

호가는 200ms 단위로 흔들린다. 사용자가 화면에서 "사기" 버튼 누르고 → 네트워크 → mci-api → broker → 매매 AP 까지 도달하는 사이 호가가 바뀌면 :
- 사용자가 본 가격과 다른 가격으로 체결 (= **slippage**)
- 분쟁의 출발점

→ 사용자에게 호가를 보여주는 순간 **그 호가의 snapshot 을 잠가서 id 부여**. 매매 AP 가 그 id 로 다시 호가를 확인 → 일치하면 그 가격으로 체결, 불일치/만료면 reject.

#### 4.7.2 핵심 단어

| 단어 | 뜻 |
|---|---|
| **quote_id** | 한 시점 호가 snapshot 의 식별자. spot/forward 의 leg 1개당 1개 |
| **swap_id** | swap 거래 (near + far 2-leg) 묶음 식별자. `SW-` prefix |
| **Registry** | quote_id ↔ Record 매핑 저장소. 운영은 Redis, dev 는 MemoryRegistry |
| **SwapIndex** | swap_id ↔ (near_id, far_id) 매핑. Registry 의 확장 인터페이스 |
| **Validity** | quote_id 가 살아있는 시간 (보통 500ms) |
| **Issuer** | 발급한 mci-price 인스턴스 라벨 (감사용) |
| **Engine** | 검증을 호출하는 매매 AP 식별자. `🔑 QuoteID 엔진` 페이지에 등록된 RBAC 키 |

#### 4.7.3 4개 RPC 의 의미

매매 AP 는 mci-price 의 gRPC `QuoteValidationService` 에 다음 4개 RPC 를 가진다.

| RPC | 누가 호출 | 입력 | 응답 status | 의미 |
|---|---|---|---|---|
| **Validate** | 매매 AP (spot/forward) | quote_id | `OK / NOT_FOUND / EXPIRED / ALREADY_CONSUMED / DENIED` | "이 id 가 아직 유효한가" 확인 (read-only) |
| **MarkConsumed** | 매매 AP (spot/forward) | quote_id, consumer_id | `OK / ALREADY_CONSUMED / NOT_FOUND / EXPIRED` | "지금 거래에 사용했음" 표시 (atomic). 두 번 호출 시 두 번째는 ALREADY_CONSUMED |
| **ValidateSwap** | 매매 AP (swap) | swap_id | 같음 + `ONE_LEG_FAIL / BOTH_FAIL` | swap 의 near + far 둘 다 OK 여야 OK |
| **ConsumeSwap** | 매매 AP (swap) | swap_id, consumer_id | 같음 + `PARTIAL_RACE` | near + far 둘 다 MarkConsumed. 한 leg 만 ALREADY 면 PARTIAL_RACE |

> Validate 와 MarkConsumed 의 분리는 의도된 것 — "거래 시도 가능 여부 사전 체크" 와 "실제 사용 완료" 를 따로 둬서 사고 분석 시 각 단계 카운터 추적 가능.

#### 4.7.4 quote_id 의 상태 머신

```
        Put (issue)                    MarkConsumed
   ─────────────────► ACTIVE ─────────────────► CONSUMED
                       │
                       │  ValidUntil 경과 + grace
                       ▼
                     EXPIRED
                       │
                       │  Registry GC
                       ▼
                    NOT_FOUND
```

- **ACTIVE** — Put 직후. Validate / MarkConsumed 가 `OK` 또는 `ALREADY_CONSUMED` (전자가 정상)
- **EXPIRED** — `now > IssuedAt + Validity` 인 상태. grace 동안은 Registry 에 남아있지만 응답은 `EXPIRED`
- **CONSUMED** — MarkConsumed 가 성공한 상태. 이후 Validate 는 `ALREADY_CONSUMED`. 같은 quote_id 로 두 번 거래 차단 (중복 매매 방지)
- **NOT_FOUND** — Registry 에서 사라진 상태. 본 적 없는 id 거나 grace 도 지난 id

#### 4.7.5 본 시나리오 — spot 거래 한 건의 흐름

사용자 `customer-1234` (Profile=WEB.BRANCH.VIP) 가 USDKRW spot buy 100만 USD :

```
14:32:15.001  브라우저 → mci-edge-api → mci-api
                  /v1/quote/spot/lock { pair=USDKRW, profile=WEB.BRANCH.VIP, customer_id=cr1234, side=buy, amount=1000000 }
              ↓
              mci-api 가 mci-price 에 forward (또는 mci-edge-price 가 직접)
              ↓
14:32:15.005  mci-price :
                  ① BEST snapshot 조회 → bid=1380.50 ask=1381.20
                  ② PricingTable.Apply (5-Layer) → bid=1380.40 ask=1381.30 (VIP 마진 적용)
                  ③ quote_id 발급 : "dev-mqbu1234-a3" (16자)
                  ④ Registry.Put({quote_id, profile, customer_id, bid=1380.40, ask=1381.30,
                                  issued=15.005, valid_until=15.505, issuer="int1"})
              ← 응답 : { quote_id, bid, ask, valid_until }

14:32:15.080  사용자가 화면 보고 "사기" 버튼 클릭 (75ms 의사결정)
              브라우저 → mci-api : POST /v1/tx
                  { alias: "WEB_ORDER_NEW",
                    data: { quote_id: "dev-mqbu1234-a3", side: "buy", amount: 1000000 } }
              ↓
              mci-api 가 broker 로 send → exchange=WECHO, routing_key=NEW (§4.1.5)

14:32:15.090  매매 AP (WECHO) 가 수신 :
                  ① gRPC : QuoteValidation.Validate(quote_id="dev-mqbu1234-a3")
                     → response : { status: OK, bid: 1380.40, ask: 1381.30,
                                    valid_until_ns: ..., issuer: "int1" }
                  ② 매매 AP 측 비즈니스 검증 (잔고 / 한도 / 거래시간 — WTG 가 안 한다고 §8.3)
                  ③ 체결 가격 결정 : side=buy 이므로 ask = 1381.30 사용

14:32:15.110  매매 AP :
                  ④ gRPC : QuoteValidation.MarkConsumed(quote_id, consumer_id="WECHO-ap1-tx-9999")
                     → response : { status: OK }   ← 같은 quote_id 로 두번째 매매는 ALREADY_CONSUMED
                  ⑤ DB INSERT, ledger 갱신, cookie_t 갱신
              ← broker 응답 → mci-api → 사용자에게 200 OK

14:32:15.115  사용자 브라우저 : 체결 완료 화면
```

> 단계 ④ 의 `consumer_id` 는 어느 매매 AP 의 어느 transaction 이 이 quote 를 썼는지 감사용. `🔍 QuoteID 조회` 페이지에서 quote_id 입력 시 `consumed_by` 필드로 노출.

#### 4.7.6 본 시나리오 — swap 거래 한 건의 흐름

같은 사용자가 **USDKRW SPOT/1M swap** (= 지금 사서 1개월 후 팔기, buy_sell) :

```
14:32:30.001  사용자가 swap 화면 진입 → 매칭엔진이 swap_lock 호출

              매칭엔진 (ap1) → mci-price :
                  POST /v1/quote/swap/lock
                  { pair=USDKRW, near={tenor:SPOT}, far={tenor:1M},
                    profile=WEB.BRANCH.VIP, customer_id=cr1234, side=buy_sell, amount=1000000 }
              ↓
14:32:30.008  mci-price.SwapLockHandler :
                  ① BEST snapshot + PricingTable.ApplyForCustomer (5-Layer) × 2 leg
                     near : bid=1380.40  ask=1381.30
                     far  : bid=1381.25  ask=1382.25  (1M swap +0.85/+0.95 가산)
                  ② quote_id 발급 : near_id, far_id, swap_id 3개
                  ③ putSwapAtomic (atomic 보장) :
                     - Reg.Put(near)          → OK
                     - Reg.Put(far)           → OK
                     - SwapIdx.PutSwap(...)   → OK
                  ④ 응답 : { swap_id="SW-dev-...", near={quote_id,bid,ask}, far={...}, swap_diff }

14:32:30.060  사용자 "확인" → mci-api : POST /v1/tx
                  { alias: "WEB_SWAP_LOCK_CONFIRM",
                    data: { swap_id: "SW-dev-...", side: "buy_sell" } }
              ↓
              broker → exchange=WECHO, routing_key=SWAP_CONFIRM → 매매 AP

14:32:30.070  매매 AP :
                  ① gRPC : ValidateSwap(swap_id)
                     → response : { status: OK,
                                    near: {bid:1380.40, ask:1381.30, ...},
                                    far : {bid:1381.25, ask:1382.25, ...} }
                  ② 체결 가격 결정 : near 는 buy(ask=1381.30), far 는 sell(bid=1381.25)
                  ③ gRPC : ConsumeSwap(swap_id, consumer_id="WECHO-ap1-swap-12345")
                     → response : { status: OK, near_consumed: OK, far_consumed: OK }
                  ④ DB INSERT (2 leg), 정산 예약

14:32:30.105  사용자 화면 : swap 체결 완료
```

> ConsumeSwap 의 AND 정책 — 두 leg 모두 OK 일 때만 swap 성공. 한 leg 이 ALREADY 면 `PARTIAL_RACE` 응답 → 매매 AP 가 다른 leg 도 회수 (compensating consume) 후 reject.

#### 4.7.7 swap_lock 내부 atomic 보장 (재확인)

`putSwapAtomic` 의 부분실패 처리 :

```
   Reg.Put(near)  ─── OK ───►  Reg.Put(far)  ─── OK ───►  SwapIdx.PutSwap(sw)  ─── OK ───► 응답
        │ fail                      │ fail                       │ fail
        ▼                           ▼                            ▼
   metrics.fail_near        revokeLeg(near)              revokeLeg(near)
                            metrics.fail_far             revokeLeg(far)
                                                         metrics.fail_swap_index

   revoke 자체가 실패하면 revoke_fail += 1 → stale quote_id 가 Registry 에 잔존
   → 매매 AP 의 ValidateSwap 이 이상 응답 가능 → reject 처리해야 함
```

→ admin UI `🔁 FX swap 잠금 통계` 페이지의 카운터가 이 단계별 fail / revoke 결과를 그대로 노출.

#### 4.7.8 본 시나리오 RBAC (engines)

`🔑 QuoteID 엔진` 페이지 등록 (§7 와 같은 데이터 — 여기서는 RPC 권한 관점):

| engine_id | Validate | MarkConsumed | ValidateSwap | ConsumeSwap | 비고 |
|---|---|---|---|---|---|
| `wecho-ap1` | ✓ | ✓ | ✓ | ✓ | 고객 매매 AP (ap1) — full ops |
| `wecho-ap2` | ✓ | ✓ | ✓ | ✓ | 고객 매매 AP (ap2 standby) |
| `bwecho-ap1` | ✓ | ✓ | ✗ | ✗ | 직원 매매 AP (CS) — spot 만 |
| `matching-ap1` | ✓ | ✗ | ✓ | ✗ | 매칭엔진 — 검증만 (소진은 매매 AP) |
| `audit-bot` | ✓ | ✗ | ✗ | ✗ | 감사 봇 — read-only |

각 engine 의 `secret_hash` 가 gRPC metadata 의 `x-engine-secret` 와 일치해야 호출 허용. 불일치 시 `DENIED` 응답.

#### 4.7.9 흔한 함정

| 증상 | 원인 |
|---|---|
| `EXPIRED` 가 자주 보임 | 사용자의 매매 의사결정이 500ms 보다 오래 걸림 → validity 늘리거나 confirm 화면 자동 갱신 |
| `ALREADY_CONSUMED` 가 자주 보임 | 클라이언트가 같은 quote_id 로 retry — idempotency-key 도입 권장 |
| `NOT_FOUND` (방금 발급한 id) | Registry 가 mci-price 재시작 됐거나 Redis 가 끊겼었음. `🔍 QuoteID 조회` 로 issuer 확인 |
| 한 leg 만 stale 잔존 | swap_lock 의 revoke 가 실패 → fail_swap_index + revoke_fail 동시 증가. 매매 AP 가 ValidateSwap 시 ONE_LEG_FAIL 으로 reject 해야 |
| `DENIED` | engine 권한 부재 또는 secret 불일치. `🔑 QuoteID 엔진` 페이지에서 allowed_ops + secret 회전 확인 |
| swap-lock endpoint 자체가 404 | mci-price `--enable-swap-lock=false` 또는 Registry 가 SwapIndex 미구현 (Redis 1차) |
| `PARTIAL_RACE` 발생 | 두 mci-price 가 같은 swap_id 동시 ConsumeSwap. Registry SET NX 가 한 leg 만 성공. 매매 AP 가 다른 leg 회수 후 reject |

#### 4.7.10 자주 받는 질문

- **Q. validity 500ms 가 짧지 않나?**
  외환 시장의 정상 수치. 500ms 안에 mci-edge-* → mci-api → broker → 매매 AP → mci-price 검증까지 다 도는 게 정상. 더 길게 잡으면 stale 호가로 매매되어 회사 손실. 운영 정책으로 confirm 화면이 valid_until 까지 카운트다운 표시 권장.
- **Q. customer 가 화면에서 망설이면 어떻게 되나?**
  500ms 지나면 EXPIRED. confirm 시도 시 매매 AP 가 reject → 클라이언트가 자동으로 새 quote_id 발급 받아 다시 시도 (refresh & re-confirm).
- **Q. ValidateSwap 의 호가가 swap_lock 응답의 호가와 다르면?**
  안 다르다. swap_lock 의 응답 호가와 ValidateSwap 의 응답 호가는 같은 Record 에서 나온 같은 snapshot. 다르게 보이면 버그 — `docs/swap-trade-spec.md` 의 spec 위반.
- **Q. MarkConsumed 후 환불/취소는?**
  Registry 단에서 unmark 안 한다 — 한 번 소진된 quote_id 는 그대로. 취소 거래는 별도 transaction (`WEB_ORDER_CANCEL`) 로 매매 AP 의 ledger 에서 처리. WTG 는 그 흐름에 관여 안 함.
- **Q. quote_id 가 매매 AP 외에서 reuse 되면?**
  돼선 안 됨. 운영 합의 — quote_id 는 매매 호출 1회용. 다른 endpoint 에서 검증/조회는 `🔍 QuoteID 조회` 만 (read-only).
- **Q. swap_id 와 near_id/far_id 를 매매 AP 가 모두 알 필요 있나?**
  swap_id 1개로 ValidateSwap/ConsumeSwap 다 처리. near_id/far_id 는 매매 AP 가 굳이 다룰 필요 없음 — SwapIndex 가 자동 lookup. 감사용 / 분쟁 시 `🔍 QuoteID 조회` 에서만 노출.

---

## 5. PricingTable — 채널 × Site × Tier 마진 정책

본 시나리오의 마진 결정 예시. **숫자는 예시이며 실제 정책은 별도로 확정.**

### 5.1 HQ layer (회사 전체 기본 spread, pair × tenor)

| pair | SPOT | 1W | 1M | 3M | 6M | 1Y |
|---|---|---|---|---|---|---|
| USDKRW | ±0.05 | ±0.06 | ±0.08 | ±0.12 | ±0.18 | ±0.25 |
| EURKRW | ±0.10 | ±0.12 | ±0.14 | ±0.20 | ±0.28 | ±0.40 |
| JPYKRW | ±0.005 | ±0.006 | ±0.008 | ±0.012 | ±0.018 | ±0.025 |

### 5.2 Site layer (HQ vs BRANCH)

영업점은 본점 대비 추가 spread:

| Site | USDKRW 추가 | 비고 |
|---|---|---|
| `HQ` | ±0.00 | 본점은 추가 없음 |
| `BRANCH` | ±0.05 | 영업점 마진 (영업점 수익원) |

### 5.3 Customer layer (Tier 별, Profile 의 site/channel 무관)

| Tier | USDKRW skew | 의미 |
|---|---|---|
| `VIP` | bid +0.00 / ask +0.00 | 가산 없음 (최우대) |
| `GOLD` | -0.05 / +0.05 | 추가 spread |
| `STD` | -0.15 / +0.15 | 가장 넓음 |

### 5.4 Window layer (시간대별)

| 시간 (UTC) | USDKRW 추가 |
|---|---|
| 00:00 ~ 07:00 (한국 야간) | ±0.10 (유동성 ↓ → spread 확대) |
| 그 외 | ±0.00 |

### 5.5 Swap layer

`📅 선물환 스왑` 페이지에서 운영자가 매일 갱신. 시나리오 예 (1M):

| pair | bid | ask |
|---|---|---|
| USDKRW | +0.85 | +0.95 |
| EURKRW | +1.10 | +1.30 |
| JPYKRW | -0.02 | -0.02 |

### 5.6 최종 효과 예시

USDKRW BEST `bid=1380.50 / ask=1381.20`, 한국 낮시간, **`WEB.BRANCH.VIP`** 사용자:

```
raw                  1380.50 / 1381.20
+ HQ      (±0.05)   1380.45 / 1381.25
+ Site BRANCH (±0.05) 1380.40 / 1381.30
+ Customer VIP (0)   1380.40 / 1381.30
+ Window (00)        1380.40 / 1381.30   (낮시간이라 적용 안 됨)
Final                1380.40 / 1381.30   (spread 0.90)
```

같은 시점 **`WEB.BRANCH.STD`** 사용자:

```
raw                  1380.50 / 1381.20
+ HQ      (±0.05)   1380.45 / 1381.25
+ Site BRANCH (±0.05) 1380.40 / 1381.30
+ Customer STD (±0.15) 1380.25 / 1381.45
+ Window             1380.25 / 1381.45
Final                1380.25 / 1381.45   (spread 1.20)
```

같은 시점 **`CS.HQ.VIP`** (본점 트레이더):

```
raw                  1380.50 / 1381.20
+ HQ      (±0.05)   1380.45 / 1381.25
+ Site HQ (0)        1380.45 / 1381.25
+ Customer VIP (0)   1380.45 / 1381.25
Final                1380.45 / 1381.25   (spread 0.80)
```

→ 본점 VIP 가 가장 좁고, 영업점 STD 가 가장 넓다. 의도된 결과.

마진 정책 변경 직전엔 `🔬 마진 계산기` + `🪄 마진 변경 미리보기` 로 영향 시뮬레이션.

### 5.7 정확한 산식 — spread / skew / swap_point 의 결합 공식

§5.1~5.6 가 값(정책) 이라면, 본 절은 그 값을 어떻게 결합해 final 호가가 나오는지의 수학. `🔬 마진 계산기` 페이지가 이 공식대로 단계별 분해해 보여준다.

#### 5.7.1 기본 단어 두 가지 — spread 와 skew

| 단어 | 부호 | 효과 |
|---|---|---|
| **spread** | `s ≥ 0` | bid 는 `-s`, ask 는 `+s`. 양쪽으로 폭 확대 → mid 변화 없음, 폭만 +2s |
| **skew** | `k` (음/양 가능) | bid 와 ask 둘 다 같은 `+k` 이동 → mid 이동, 폭 그대로 |

좌표축으로 그리면 :

```
            spread (s)                          skew (k > 0)
              s         s                        k         k
        ◄──────┤       ├──────►             ─────┤       ┤────►
              bid     ask                       bid     ask
        bid ← spread/2 ──► mid ← spread/2 ──► ask  +  mid 이동
```

→ **회사는 spread 로 폭을 벌어 마진 확보, skew 로 매수/매도 한쪽을 유도** (예: 회사가 USD 사고 싶으면 ask 를 낮춰 skew < 0).

운영 PricingTable 의 각 cell 은 보통 `{spread, skew}` 쌍을 가지지만, §5.1 ~ §5.3 처럼 `±X` 표기는 `spread=X, skew=0` 의 의미.

#### 5.7.2 한 cell 의 적용 공식

cell 한 칸이 호가에 적용되는 기본 식 :

```
bid_after = bid_before  +  skew  -  spread
ask_after = ask_before  +  skew  +  spread
```

부호 컨벤션 (반드시 일관) :
- `spread` 는 항상 ≥ 0 (음수면 bid > ask 가 되어 cross — 거래 불가)
- `skew` 는 음수 / 양수 둘 다 가능 — 부호가 시장 방향 표현

bid/ask 양쪽 같은 `skew` 를 더해야 폭(spread) 이 유지된다. 만약 cell 정의가 `{bid_delta, ask_delta}` 2개 값이면 :

```
bid_after = bid_before + bid_delta    (bid_delta 는 보통 음수)
ask_after = ask_before + ask_delta    (ask_delta 는 보통 양수)
```

두 표기는 동치 — `bid_delta = skew - spread`, `ask_delta = skew + spread`.

#### 5.7.3 5-Layer 결합 순서 (좌→우)

```
raw_bid                                                                          final_bid
   │                                                                                ▲
   ▼                                                                                │
[Swap] ────► [HQ] ────► [Site] ────► [Customer] ────► [Window] ──► (출력) ◄────────┘
(forward 만)   ↑           ↑              ↑                ↑
              pair       pair × profile  pair × usid    pair × time-of-day
```

**왜 이 순서인가** — 각 layer 의 의미가 더해지는 자연 순서:

1. **Swap (forward 만)** — tenor 가 SPOT 이 아니면 가장 먼저 swap_point 적용. spot 호가 → forward 호가로 변환. SPOT 거래는 본 layer 통과 안 함.
2. **HQ** — 회사 전체 기본 마진. tenor 무관 (운영 정책상).
3. **Site** — 본점/영업점 단위 가산. 영업점이 본점보다 spread 더 받음 = 영업점 수익.
4. **Customer** — 개별 고객 우대/페널티. VIP 는 0, STD 는 가장 넓음.
5. **Window** — 시간대별 (야간 spread 확대, 변동성 ↑).

각 layer 가 §5.7.2 의 공식을 한 번씩 적용. 즉 5번 누적.

> Customer 와 Window 의 순서는 정책 결정 — "VIP 가 야간엔 어떻게 되나" 의 해석. 본 시나리오는 Customer 먼저, Window 가 마지막에 한번 더 — VIP 도 야간엔 spread 확대.

#### 5.7.4 누적 공식 (수학적 합산)

bid 와 ask 둘 다 각 layer 의 `bid_delta` / `ask_delta` 의 단순 합 :

```
bid_final = bid_raw
          + (swap_bid_delta   if forward)
          + hq_bid_delta
          + site_bid_delta
          + customer_bid_delta
          + window_bid_delta

ask_final = ask_raw
          + (swap_ask_delta   if forward)
          + hq_ask_delta
          + site_ask_delta
          + customer_ask_delta
          + window_ask_delta
```

→ **선형 합** 이라 순서가 바뀌어도 결과는 같다 (수학적으로) — 운영상 §5.7.3 의 순서를 표시 단계로 쓰는 것뿐.

총 spread/skew 도 합산 :
```
spread_total = sum(spread_i)
skew_total   = sum(skew_i)
spread_final = spread_raw + 2 × spread_total           ← 폭 확장
mid_final    = mid_raw + skew_total                    ← mid 이동
```

#### 5.7.5 본 시나리오 — 풀어쓴 USDKRW 1M / WEB.BRANCH.STD 산식

§5.6 의 STD 케이스를 본 절 공식으로 재계산 (모든 layer 가 `spread=X, skew=0`):

```
raw                             bid=1380.50    ask=1381.20    (spread 0.70)

Swap (1M, USDKRW)               swap_bid=+0.85  swap_ask=+0.95
  → bid_after_swap = 1380.50 + 0.85 = 1381.35
  → ask_after_swap = 1381.20 + 0.95 = 1382.15

HQ (USDKRW, ±0.05)
  → bid = 1381.35 - 0.05 = 1381.30
  → ask = 1382.15 + 0.05 = 1382.20

Site (BRANCH, ±0.05)
  → bid = 1381.30 - 0.05 = 1381.25
  → ask = 1382.20 + 0.05 = 1382.25

Customer (STD, ±0.15)
  → bid = 1381.25 - 0.15 = 1381.10
  → ask = 1382.25 + 0.15 = 1382.40

Window (낮시간, 0)
  → bid = 1381.10
  → ask = 1382.40

Final                           bid=1381.10    ask=1382.40    (spread 1.30)
```

> §5.6 의 SPOT 예시는 Swap 단계 skip — 본 결과는 forward 라 swap layer 의 `+0.85/+0.95` 가 추가로 들어간 모습.

#### 5.7.6 broken-date 보간 — value_date 가 tenor cell 사이일 때

운영자는 PricingTable 에 정해진 tenor 만 등록 (`SPOT/1W/1M/3M/6M/1Y` 등). 사용자가 임의 value_date (예: `2026-07-15`) 를 요청하면 양 옆 tenor 의 swap_point 를 **일수 가중 평균** 으로 보간.

```
value_date          : 2026-07-15
issue_date          : 2026-06-13                         ← 발급 시각 기준
days_to_value_date  : 32                                 ← (value_date - issue_date)
near_tenor          : 1M  (= 30일,  swap 0.85/0.95)      ← 자동 선택
far_tenor           : 3M  (= 90일,  swap 2.40/2.55)
near_days           : 30
far_days            : 90

weight_near = (far_days - days_to_value_date) / (far_days - near_days)
            = (90 - 32) / (90 - 30) = 58/60 = 0.9667
weight_far  = 1 - weight_near = 0.0333

swap_bid_interp = 0.85 × 0.9667 + 2.40 × 0.0333 = 0.8217 + 0.0800 = 0.9017
swap_ask_interp = 0.95 × 0.9667 + 2.55 × 0.0333 = 0.9183 + 0.0850 = 1.0033
```

→ 보간 결과 (0.9017 / 1.0033) 가 swap layer 의 입력값으로 들어가서 위의 5.7.4 누적식에 흘러간다.

휴일 인접 시 결제일 roll-forward 가 일어나도 본 보간식은 같다 — `📆 휴일 캘린더` 가 결정한 value_date 기준 일수.

#### 5.7.7 `🔬 마진 계산기` 페이지의 단계별 출력과 1:1 매핑

admin UI 의 `🔬 마진 계산기 (5-Layer)` 페이지 카드 7개가 §5.7 의 식을 그대로 시각화 :

| 카드 | 본 절 매핑 |
|---|---|
| **0. Inputs** | raw bid/ask + Profile + Customer + value_date |
| **1. Active TimeWindows** | §5.4 의 시각 매칭 |
| **1.5 Value Date 보간** | §5.7.6 의 가중평균 |
| **2. Swap** | §5.7.3 의 ① — swap layer 적용 후 호가 |
| **3. HQ** | ② |
| **4. Site** | ③ |
| **5. Customer** | ④ |
| **6. Final 산식** | §5.7.4 의 final + spread_total / skew_total |

→ 운영자가 사용자 보고 "내 호가가 왜 이렇지?" 받으면, 본 페이지에서 같은 입력으로 계산해 어느 layer 가 그 값을 만들었는지 추적.

#### 5.7.8 부호 / cross 검증

`PricingTable.Apply` 가 결과 검증으로 다음 두 가지 체크 :

1. **cross 방지** — `bid_final < ask_final` 이어야 함. 위반 시 reject (mci-price 가 `crossed_final` 카운터 += 1).
2. **음수 호가 방지** — `bid_final > 0`. 위반 시 reject (`non_positive_final`).

운영 사고 시 위 카운터가 spike 면 PricingTable 의 어떤 cell 이 비정상 (예: spread 값이 raw spread 보다 큰 음수 skew). admin UI `📊 시세 통계 (mci-price)` 와 `🔬 마진 계산기` 의 마지막 카드에서 검증.

#### 5.7.9 자주 받는 질문

- **Q. spread 만 양수 제약이면 왜 `±0.05` 처럼 ± 로 표기?**
  운영자 가독성. ±0.05 = `bid -= 0.05, ask += 0.05` 의미. 내부 표현은 single `spread` 값.
- **Q. swap_point 가 음수면?**
  가능. 예: JPYKRW 의 `1M = -0.02/-0.02` — 1M 후 JPYKRW 가 spot 보다 0.02 낮을 거란 forward 기대. swap_bid_delta / swap_ask_delta 자체가 음수일 뿐 공식은 같다.
- **Q. Customer layer 가 customer_id 별로 다르지 않고 Tier 만 보는 게 맞나?**
  본 시나리오는 그렇다 (§5.3 가 Tier 만). 운영 정책이 개별 고객 우대를 요구하면 `pricing` 페이지의 Customer 탭에서 customer_id × pair grid 로 개별 cell 입력 — 그 경우 Customer layer 의 검색 키가 (Tier, customer_id) 우선순위로 동작.
- **Q. Window layer 의 시각 컷오프가 UTC 인 이유?**
  서머타임 회피 + 전 site 동일 적용. 운영자가 한국시간으로 입력하면 헷갈림. 운영 가이드에 "UTC 입력 — KST = UTC+9" 명시.
- **Q. 산식 변경 시 (예: skew 적용을 곱셈으로) 어디를 고치나?**
  `pkg/pricing/Apply` 와 `internal/price/handlers/preview.go` 동시 수정. `margin-calc` 페이지의 표시 카드 step 도 갱신. 변경 즉시 5-Layer 의미가 달라지므로 운영 일괄 공지 + `🪄 마진 변경 미리보기` 로 영향 측정 후 배포.

---

## 6. 각 서비스 구체 설정 — flag / env

### 6.1 AP 서버 (`ap1` / `ap2`)

#### mymqd broker
```bash
mymqd --port 11217 \
      --cluster-port 11218 \
      --peer ap1.internal:11218,ap2.internal:11218 \
      --role auto \                  # active 결정은 cluster 자동
      --tls-cert /etc/pki/wtg/broker.crt \
      --tls-key  /etc/pki/wtg/broker.key \
      --tls-ca   /etc/pki/wtg/ca.crt \
      --log-dir  /var/log/mymqd
```

#### 매매 AP (예: WECHO)
```bash
WECHO --broker localhost:11217 \
      --appl WECHO \
      --instance 1 \
      --cookie-store-dsn ...        # 매매 엔진 측 cookie_t 저장소
```

#### 매칭엔진 (libwtgprice 사용)
- `cside/wtgprice/wtgprice.h` 의 `wtg_price_swap_lock()` 호출
- mci-price 의 `--price-grpc` 가 아닌 **internal 측 mci-price 의 HTTP** 호출:
  - `wtg_price_client_init(&cli, "int1.internal", 8082);` (HA 면 VIP 사용)

### 6.2 Internal — `int1` / `int2`

#### mci-api (인증 + envelope)
```bash
mci-api --broker ap1.internal:11217,ap2.internal:11217 \    # active-standby DNS 또는 VIP
        --broker-tls-ca /etc/pki/wtg/ca.crt \
        --broker-tls-cert /etc/pki/wtg/int1.crt \
        --broker-tls-key  /etc/pki/wtg/int1.key \
        --etcd https://etcd1.internal:2379,...,etcd3.internal:2379 \
        --etcd-tls-cert ... --etcd-tls-key ... --etcd-tls-ca ... \
        --auth-redis redis1.internal:26379,redis2.internal:26379,redis3.internal:26379 \
        --auth-redis-master wtg-auth-master \
        --listen :8080 \
        --otel-endpoint otel-col.obs1.internal:4317
```

#### mci-push (broker rep + HTTP push)
```bash
mci-push --broker ap1.internal:11217,ap2.internal:11217 \
         --broker-tls-... \
         --listen :8081 \
         --push-secret-file /etc/pki/wtg/push-secret \
         --push-mtls-ca /etc/pki/wtg/ca.crt
```

#### mci-price (시세 + 마진 + QuoteID + swap-lock)
```bash
mci-price --broker ap1.internal:11217,ap2.internal:11217 \
          --broker-tls-... \
          --listen :8082 --grpc :50051 \
          --grpc-tls-cert ... --grpc-tls-key ... --grpc-tls-client-ca ... \
          --etcd https://etcd1...:2379,... \
          --etcd-tls-... \
          --enable-swap-lock \
          --quoteid-redis redis1.internal:26379,redis2.internal:26379,redis3.internal:26379 \
          --quoteid-redis-master wtg-quoteid-master \
          --quoteid-instance int1 \              # int2 는 instance=int2
          --quoteid-validity 500ms \
          --quoteid-grace 1s \
          --dsn postgres://wtg@db1.internal/wtg \   # Archiver 활성
          --otel-endpoint otel-col.obs1.internal:4317
```

#### mci-chart
```bash
mci-chart --listen :8086 \
          --upstream int1.internal:50051 \      # mci-price 의 gRPC
          --grpc-tls-... \
          --dsn postgres://wtg@db1.internal/wtg \
          --pool 20
```

#### mci-admin (운영 콘솔)
```bash
mci-admin --listen :9090 \
          --tls-cert ... --tls-key ... \         # admin 자체도 TLS
          --etcd https://etcd1...:2379,... --etcd-tls-... \
          --auth-redis ... --auth-redis-master ... \
          --broker ap1.internal:11217,ap2.internal:11217 \
          --broker-tls-... \
          --price-url http://int1.internal:8082 \
          --fwd-url   http://fwd1.internal:9091 \
          --prom-url  http://prom.obs1.internal:9091 \
          --grafana-url http://grafana.obs1.internal:3000 \
          --grafana-user wtg-readonly --grafana-pass-file /etc/pki/wtg/grafana-pass
```

> `mci-admin` 은 1대만 띄움 (운영 콘솔). HA 필요하면 active-standby — 동시에 두 운영자가 쓰면 etcd 의 write race 위험.

### 6.3 DMZ — `dmz1` / `dmz2`

#### mci-edge-api
```bash
mci-edge-api --listen :8090 \
             --tls-cert /etc/pki/wtg/dmz.crt --tls-key /etc/pki/wtg/dmz.key \
             --upstream http://int1.internal:8080,http://int2.internal:8080 \
             --upstream-mtls-cert ... --upstream-mtls-key ... --upstream-mtls-ca ... \
             --jwt-jwks-url ... \
             --allow-cidrs 10.0.0.0/8,192.168.0.0/16 \
             --otel-endpoint otel-col.obs1.internal:4317
```

#### mci-edge-price
```bash
mci-edge-price --listen :8083 \
               --tls-cert ... --tls-key ... \
               --upstream int1.internal:50051,int2.internal:50051 \   # N6 fan-out
               --grpc-tls-... \
               --quote-stream \
               --customer-stream \
               --queue-cap 1024 \                       # 운영은 256 → 1024
               --ws-ping 30s --ws-pong 1m \
               --allow-cidrs ...
```

#### mci-edge-push / mci-edge-chart
```bash
mci-edge-push  --listen :8084 --tls-... --upstream int1.internal:8081,int2.internal:8081 ...
mci-edge-chart --listen :8087 --tls-... --upstream int1.internal:8086,int2.internal:8086 ...
```

### 6.4 quote-forwarder (`fwd1`)

```bash
quote-forwarder \
    --multi SMB:30044,KMB:30045,EBS:30046,REUT:30051 \
    --bind 0.0.0.0 \
    --batch-max 14 --batch-timeout 10ms \
    --publish-mode grpc \
    --price-grpc int1.internal:50051 \
    --metrics 0.0.0.0:9091 \
    --otel-endpoint otel-col.obs1.internal:4317
```

> `--publish-mode grpc` 로 broker 우회. broker 부하 회피 + 운영 C 코드의 mymq 의존 제거 (`docs/cooker-patch.md`).

### 6.5 인프라

#### etcd cluster (`etcd{1,2,3}`)
- 3 node, peer port `:2380`, client port `:2379`
- mTLS 활성 (`--peer-trusted-ca-file`, `--trusted-ca-file`, ...)
- snapshot 매일 (`etcdctl snapshot save`)
- 모든 WTG 서비스가 같은 PKI 사용

#### Redis Sentinel (`redis{1,2,3}`)
- 각 node 가 Redis + Sentinel 동거. Sentinel port `:26379`
- 두 master (논리적) :
  - `wtg-auth-master` — pkg/auth.RedisStore
  - `wtg-quoteid-master` — pkg/quoteid.RedisRegistry
- 두 master 가 같은 노드에 있어도 됨 (Sentinel 명만 분리)
- AOF everysec, `noeviction`
- TLS 활성

#### TimescaleDB (`db1`)
```sql
CREATE DATABASE wtg;
\c wtg
\i /opt/wtg/etc/sql/quote_bars.sql        -- 본 시나리오는 TimescaleDB 사용 → hypertable + 압축 + retention 활성
-- 운영 user
CREATE ROLE wtg WITH LOGIN PASSWORD '...';
GRANT INSERT, SELECT ON quote_bars TO wtg;
GRANT USAGE ON SCHEMA public TO wtg;
```

#### 관측 stack (`obs1`)
- Prometheus scrape config:
```yaml
scrape_configs:
  - job_name: mci-api
    static_configs: [{ targets: ['int1.internal:8080','int2.internal:8080'] }]
  - job_name: mci-price
    static_configs: [{ targets: ['int1.internal:8082','int2.internal:8082'] }]
  - job_name: mci-edge-price
    static_configs: [{ targets: ['dmz1.internal:8083','dmz2.internal:8083'] }]
  - job_name: quote-forwarder
    static_configs: [{ targets: ['fwd1.internal:9091'] }]
  - job_name: mymqd
    static_configs: [{ targets: ['ap1.internal:9100','ap2.internal:9100'] }]   # node_exporter
  - job_name: etcd
    static_configs: [{ targets: ['etcd1.internal:2379','etcd2.internal:2379','etcd3.internal:2379'] }]
```
- Alertmanager — Slack webhook + PagerDuty
- Grafana dashboard import : `docs/monitoring.md`, `docs/push-monitoring.md`
- alert rule `etc/grafana/mci-price-swaplock-alerts.yml` 포함

---

## 7. QuoteID 엔진 등록 (매칭엔진 측)

매칭엔진이 `cside/wtgprice` C SDK 로 mci-price 의 `/v1/quote/swap/lock` 을 호출하고, 발급된 quote_id 를 매매 AP 가 `ValidateSwap` / `ConsumeSwap` 으로 검증한다. mci-admin 의 `🔑 QuoteID 엔진` 페이지에 등록:

| engine_id | allowed_ops | secret 보관 |
|---|---|---|
| `matching-ap1` | `["validate","mark_consumed","validate_swap","consume_swap"]` | `/etc/pki/wtg/engine-matching-ap1.secret` (mode 0600, ap1 만 read) |
| `matching-ap2` | 같음 | `/etc/pki/wtg/engine-matching-ap2.secret` |
| `wecho-ap1` | `["validate","mark_consumed"]` | (선택) spot 검증만 하면 충분 |
| `bwecho-ap1` | 같음 | CS 채널 매매 AP |
| `audit-bot` | `["validate"]` | read-only 감사 |

매칭엔진의 C 호출 예 :

```c
wtg_price_client_t cli;
wtg_price_client_init(&cli, "int1.internal", 8082);   // VIP 또는 DNS
wtg_price_swap_lock_request_t req = {
    .pair        = "USDKRW",
    .near_tenor  = "SPOT",
    .far_tenor   = "1M",
    .profile     = "WEB.BRANCH.VIP",
    .customer_id = "customer-1234@partner.co.kr",
    .side        = "buy_sell",
    .amount      = 1000000.0,
};
wtg_price_swap_lock_response_t resp;
int rc = wtg_price_swap_lock(&cli, &req, &resp);
if (rc != 0) {
    // 거래 거부 (retry 금지 — quote_id unique)
}
// resp.swap_id 를 매매 AP 에 첨부
```

---

## 8. 인증 / 권한

### 8.1 JWT claim 매핑

mci-api `/v1/login` 이 발급하는 JWT 의 claim:

```json
{
  "iss": "wtg",
  "sub": "customer-1234@partner.co.kr",
  "usid": "customer-1234@partner.co.kr",
  "channel": "WEB",
  "site": "BRANCH",
  "tier": "VIP",
  "profile_key": "WEB.BRANCH.VIP",
  "exp": 1781329615,
  "iat": 1781328715
}
```

→ mci-edge-price 가 ws connect 시 JWT 의 `profile_key` 로 SubscribeQuote 라우팅. mci-edge-api 가 매매 호출 시 같은 claim 으로 ratelimit / 감사 키.

### 8.2 채널별 인증 경로

| 채널 | 로그인 경로 | 토큰 |
|---|---|---|
| WEB | `POST /v1/login` (mci-edge-api → mci-api) | JWT (1시간) + refresh (24시간) |
| CS | 사내 SSO → mci-api 의 `/v1/cs-login` (예시) | JWT (8시간) |

> 사내 CS 는 LDAP/AD SSO 와 결합. WEB 은 회원 DB.

### 8.3 권한 위임 원칙 (재확인)

- **Authentication (누구인가)** → WTG
- **Authorization (무엇을 할 수 있는가)** → 매매 AP
- WTG 는 거래 한도/통화쌍 활성/거래시간/slippage 같은 비즈니스 권한 체크 안 함.
- 로그인 시 매매 AP 가 발급한 `cookie_t` 를 Redis 에 저장, 이후 호출에 그대로 첨부 (passthrough).
- 자세히 `docs/auth.md`.

### 8.4 cookie_t passthrough — 가장 헷갈리는 부분

§8.3 의 한 줄 "cookie_t 를 Redis 에 저장, 이후 호출에 그대로 첨부 (passthrough)" 만으로는 운영 시 헷갈리는 케이스가 많다. 본 절은 그 메커니즘을 단계별로 푼다.

#### 8.4.1 cookie_t 가 뭔지

| | JWT | cookie_t |
|---|---|---|
| 누가 발급 | WTG (mci-api 의 `/v1/login`) | 매매 AP (broker `AUTH` exchange) |
| 누가 검증 | WTG (mci-edge-api / mci-api 가 매 요청마다) | 매매 AP (매 transaction 요청마다) |
| 의미 | 인증 — 이 사용자가 누구인지 (`usid`, `profile_key`) | 권한 — 무엇을 할 수 있는지 (한도, 통화쌍, 시간) |
| WTG 는 어떻게 다루나 | claim 파싱, profile_key 로 라우팅 | **불투명 바이트**. 파싱 X, 첨부만. |
| 형태 | JWT (`eyJh...`) | 매매 AP 가 정한 binary blob (~ N 백 바이트) |
| 만료 | exp claim (예: 1시간) | 매매 AP 정책 (보통 같은 세션) |

**핵심** : WTG 는 cookie_t 를 **열어보지 않는다**. 매매 AP 가 자기 형식으로 만든 token 을 그대로 받아 그대로 첨부해 broker 에 돌려준다.

#### 8.4.2 로그인 흐름 — JWT 발급 + cookie_t 저장

```
브라우저
   │  POST /v1/login { id, password }
   ▼
mci-edge-api (DMZ) ── TLS, ratelimit, IP allowlist ──►  mci-api (Internal)
                                                          │
                                                          │  broker call : alias=WEB_LOGIN
                                                          │    exchange=AUTH, routing_key=LOGIN
                                                          │    body={ id, password }
                                                          ▼
                                                       mymqd ──► AUTH AP (ap1)
                                                                  │
                                                                  │  자체 인증 + 사용자 권한 조회
                                                                  │  cookie_t 발급
                                                                  ▼
                                                       응답 : { usid, profile (Channel.Site.Tier),
                                                                cookie_t: "<binary blob>",
                                                                expires_at }
                                                          ▲
                                                          │
                                                          │  mci-api 가 응답 받음 → 다음 4단계 :
                                                          │
                                                          │  ① JWT 생성 — claim 에 usid, profile_key, channel, site, tier
                                                          │  ② Redis 에 저장 :
                                                          │        key   : "wtg:session:<jwt-jti>"
                                                          │        value : { usid, profile_key, cookie_t, expires_at }
                                                          │        TTL   : JWT 만료까지
                                                          │  ③ 응답에 JWT (+ refresh_token) 만 노출 — cookie_t 는 절대 클라이언트에 안 보냄
   ◄──────────────────────────────────────── { access_token: "eyJh...", refresh_token: "...", profile: "WEB.BRANCH.VIP" }
```

→ **클라이언트는 JWT 만 가진다. cookie_t 는 Redis 안에서만 산다.**

#### 8.4.3 매매 호출 흐름 — cookie_t 자동 첨부

```
브라우저  POST /v1/tx { alias: "WEB_ORDER_NEW", data: {...} }
   │  Authorization: Bearer eyJh...
   ▼
mci-edge-api  ── JWT verify (서명 + exp) → 통과
   │  forward 요청에 X-WTG-User claim 헤더 첨부
   ▼
mci-api
   │  ① JWT 의 jti (또는 sub+iat 해시) 로 Redis lookup :
   │        Redis.Get("wtg:session:<jwt-jti>") → { usid, profile_key, cookie_t, expires_at }
   │        없으면 → 401 (세션 만료 또는 로그아웃)
   │        TTL 짧으면 → 재발급 트리거 (refresh)
   │
   │  ② alias 라우팅 룰 → exchange=WECHO, routing_key=NEW
   │
   │  ③ broker 호출 body 조립 :
   │        { ...요청 data...,
   │          cookie_t: "<binary blob 그대로>" }   ◄── 그대로 박는다
   │  Client.Call(exchange=WECHO, routing_key=NEW, body, cookie_t)
   ▼
mymqd  ──►  WECHO (ap1)
                  │  자체 cookie_t 검증 :
                  │   - 자기가 발급한 것인지 (서명/HMAC)
                  │   - 만료 여부
                  │   - 비즈니스 권한 (이 cookie_t 의 사용자가 거래 가능한가)
                  │  → 통과 시 매매 진행, 실패 시 mymq.Error{errn=PERM_DENIED}
                  ▼
                  매매 처리 + 응답
   ◄──────  broker 응답 (cookie_t 갱신 포함 가능)
   │
   │  ④ 응답에 새 cookie_t 가 있으면 Redis 의 cookie_t 만 갱신 (rolling)
   │  ⑤ 클라이언트에는 비즈니스 응답만 (cookie_t 노출 X)
   ◄──────────  { success, order_id, ... }
```

→ **WTG 는 cookie_t 를 "들고 다니는 자전거 짐받이" 역할.** 내용을 읽지도, 만들지도 않는다.

#### 8.4.4 cookie_t 갱신 (rolling) 과 만료

매매 AP 가 응답에 새 cookie_t 를 박아 줄 수도 있다 (rolling token 패턴) :

| 시점 | 동작 |
|---|---|
| 매 transaction 응답 | 응답 body 에 `cookie_t_new` 있으면 mci-api 가 Redis 갱신 |
| `expires_at` 임박 | mci-api 가 미리 broker 의 `AUTH/REFRESH` 호출해 갱신 (refresh-on-near-expiry) |
| 만료 후 호출 | 매매 AP 가 `EXPIRED_SESSION` 응답 → mci-api 가 401 → 클라이언트가 `/v1/login` 재호출 |
| 로그아웃 | `/v1/logout` → mci-api 가 broker `AUTH/LOGOUT` 호출 + Redis 키 삭제 |

> 본 시나리오의 매매 AP (WECHO/BWECHO) 가 rolling token 을 쓰는지 안 쓰는지는 운영 합의 문서 (`docs/auth.md`) 참조. 안 쓰면 ④ step 은 no-op.

#### 8.4.5 Redis store — 멀티 인스턴스 공유

```
브라우저 #A  ──► mci-edge-api (LB) ──► mci-api @ int1
                                          │
                                          ▼ Redis.Get/Set("wtg:session:<jti>")
                                       ┌──────────────────────────┐
                                       │ Redis Sentinel cluster   │
                                       │ master + 2 replica       │
                                       └──────────────────────────┘
                                          ▲
                                          │ Redis.Get/Set
                                          │
브라우저 #A 의 다음 요청 ──► mci-edge-api (LB) ──► mci-api @ int2 ──┘
   같은 사용자가 다른 mci-api 인스턴스에 도달해도 같은 cookie_t 사용 가능
```

운영 의미 :
- mci-api 가 N 인스턴스로 떠도 같은 세션 공유 — sticky 라우팅 불필요
- mci-api 한 대가 죽어도 사용자는 다른 인스턴스로 바로 잇기 (cookie_t 보존)
- Redis 가 죽으면 세션 lookup 전부 fail → 모든 로그인 사용자가 즉시 401
- → Redis Sentinel HA 가 필수 (§6.5)

#### 8.4.6 본 시나리오 정확한 Redis key 구조

```
wtg:session:<jti>          string  →  JSON({usid, profile_key, cookie_t, expires_at})
                            TTL   = JWT exp - now (보통 1h)

wtg:user-sessions:<usid>   set     →  { <jti_1>, <jti_2>, ... }   동일 사용자의 다중 디바이스
                            TTL   = 가장 긴 세션의 만료

wtg:cookie-blob:<jti>      bytes   →  cookie_t 원본 (큰 경우 별도 키로 분리)
                            optional 최적화

wtg:refresh:<refresh_jti>  string  →  JSON({sub, original_jti, expires_at})
                            TTL   = refresh exp (보통 24h)
```

→ `🔍 Customer 검색` 페이지에서 사용자 검색 시 `wtg:user-sessions:<usid>` set 으로 그 사용자의 활성 세션 수 확인.

#### 8.4.7 채널별 차이 — WEB vs CS

| 항목 | WEB | CS |
|---|---|---|
| 로그인 alias | `WEB_LOGIN` → `AUTH/LOGIN` | `CS_LOGIN` → `AUTH/CS_LOGIN` |
| 사전 인증 | 비밀번호 (mci-api 검증 X — broker AUTH AP 가 검증) | 사내 SSO (LDAP/AD/SAML) — JWT 발급 시 SSO 검증 통과 필요 |
| cookie_t | 매매 AP 발급 | 같음 (다른 broker AP 이지만 동일 wire) |
| JWT exp | 1h | 8h (직원이라 더 길게) |
| profile_key | `WEB.HQ.*` / `WEB.BRANCH.*` | `CS.HQ.*` / `CS.BRANCH.*` |
| 로그아웃 | 명시 `/v1/logout` 또는 만료 | SSO 로그아웃 연동 또는 만료 |

→ cookie_t passthrough 자체는 **양 채널이 동일 메커니즘**. 위에 인증 단계만 다름.

#### 8.4.8 흔한 함정

| 증상 | 원인 |
|---|---|
| 로그인 직후 매매 401 | Redis 쓰기 실패 (마스터 failover 중) — 다시 시도 |
| 임의 시각 401 | JWT 는 살아있는데 Redis TTL 이 짧음. mci-api 의 `--auth-session-ttl` 가 JWT exp 와 일치해야 |
| `PERM_DENIED` (broker errn) | cookie_t 가 stale 또는 변조 — Redis 갱신 누락 / 매매 AP 쪽 권한 데이터 변경 |
| 로그아웃 후에도 매매 가능 | mci-api 가 `/v1/logout` 처리 후 Redis 키 삭제했지만 broker AUTH AP 에 LOGOUT 안 보냄 — 한쪽만 정리 |
| 한 사용자가 2 디바이스 로그인 시 한쪽이 끊김 | 정책에 따라 다름. 본 시나리오는 동시 허용 (각 jti 가 별개). 차단하려면 mci-api 가 같은 usid 의 이전 jti 들 invalidate |
| 클라이언트가 토큰 새로 발급 후에도 같은 401 | Redis 캐시 stale — 갱신 후 짧은 대기 (~수 ms) 필요. drop-in retry 정책 |
| Redis 끊김 5분 | 활성 사용자 전부 401 → kill switch 와 같은 효과. Redis HA 의 **단일 실패점** — Sentinel 필수 |

#### 8.4.9 자주 받는 질문

- **Q. WTG 가 cookie_t 를 안 보면 어떻게 검증?**
  검증 안 한다. broker AUTH AP 가 발급할 때 자기 서명/HMAC 박아두고, 매매 AP 가 매 요청마다 그걸 검증. WTG 는 운반만.
- **Q. JWT 안에 cookie_t 를 박지 왜 Redis 에 따로 두나?**
  cookie_t 크기 (수백 B ~ KB) + revoke 가능성. JWT 에 박으면 클라이언트에 노출 + revoke 안 됨. Redis 에 두면 즉시 invalidate 가능 + 클라이언트는 작은 JWT 만.
- **Q. mci-api 재시작 시 활성 세션은?**
  영향 없음. cookie_t 가 Redis 에 있으니 다른 인스턴스 (또는 재시작 후 같은 인스턴스) 가 그대로 lookup 가능.
- **Q. 매매 AP 가 cookie_t 형식 바꾸면 WTG 도 바꿔야 하나?**
  WTG 코드는 **그대로 OK**. 불투명 바이트라 형식에 무관. 단 Redis 에 저장된 기존 cookie_t 들은 stale → 일괄 로그아웃 (re-login) 권장.
- **Q. 한 사용자가 동시에 spot 매매 + swap 매매 호출하면 cookie_t 충돌?**
  안 한다. mci-api 가 매 요청마다 같은 cookie_t 를 broker 에 보내고, broker AP 는 매 요청을 독립 처리. cookie_t 자체가 atomic counter 같은 게 아니라 권한 token 이므로 동시 사용 가능.
- **Q. CS 사용자의 사내 SSO 토큰과 cookie_t 의 관계?**
  분리. SSO 가 1차 인증 (직원 신원 확인) → mci-api 가 SSO 검증 후 broker `AUTH/CS_LOGIN` 호출 → 매매 AP 가 cookie_t 발급. SSO 토큰은 그 이후 안 씀.

---

## 9. 부트스트랩 순서 (first-time)

다음 순서가 안전:

```
1. OS / 네트워크 / firewall 룰 / PKI 배치
2. infra :  etcd cluster ✓  →  Redis Sentinel ✓  →  TimescaleDB ✓  →  관측 stack ✓
3. AP standby (ap2) 부터 :  mymqd → 매매 AP → 매칭엔진
4. AP active (ap1) :  같은 컴포넌트, cluster 합류
5. Internal :  mci-admin (etcd 초기 seed 입력) → mci-api / push / price / chart (int1 → int2)
6. DMZ :  mci-edge-* (dmz1 → dmz2)
7. quote-forwarder :  feed source 가 ready 된 후
8. 검증 :
     /tmp/wtg-status.sh                       (모든 프로세스 UP, /metrics 200)
     mci-test --ckey-echo                     (broker round-trip)
     매칭엔진 → mci-price /v1/quote/swap/lock  (한 건 호출 → swap_id)
     매매 AP → ValidateSwap → ConsumeSwap     (전 단계 OK)
     mci-admin → 운영 모니터링 페이지 모두 정상
     mci-admin → 시세 (라이브 ws) ▶ 연결      (호가창 정상)
```

> `mci-admin` 의 초기 seed:
> - `profiles.json` (§3.1)
> - `routes.json` (§4.2)
> - `pricing/table` 초기값 (§5)
> - `quoteid/engines` (§7)
> - `users` 운영자 + admin role 부여

---

## 10. failover 시나리오

### 10.1 ap1 (active) 가 죽는 경우

1. broker cluster 가 ap2 를 active 로 promote (수초)
2. WTG 측 `pkg/mymq.Client` supervisor goroutine 이 connection refused 감지 → 재연결 시도 → ap2:11217 로 붙음
3. 매매 AP / 매칭엔진은 ap2 의 짝이 takeover (broker 와 같은 노드라 한 묶음)
4. WTG 의 `mci-admin → 대시보드` 의 broker 카드가 `disconnected` → `connecting` → `connected` 로 회복
5. 그 사이 진행 중인 매매는 `mymq.Error{inflight_aborted}` 로 503 — 클라이언트 retry 정책으로 자동 재시도

**관측 신호**:
- Prometheus `wtg_broker_disconnects_total` += 1
- 운영 모니터링 페이지 "Broker disconnects" 카드 spike
- Backpressure 이력 페이지에 broker rep receiver queue 80% 도달 가능 (전환 직후 burst)

### 10.2 ap2 (standby) 가 죽는 경우

운영 정상 영향 없음. 단 HA 가 깨진 상태 — 즉시 복구 필요.

### 10.3 mci-edge-price 한 인스턴스 (dmz1) 가 죽는 경우

- N6 fan-out 으로 dmz2 가 계속 서비스
- dmz1 에 붙어있던 ws 클라이언트는 LB 가 자동 재연결 → dmz2 로 붙음
- 운영 모니터링 → 구독자 (gRPC) 페이지에서 mci-edge-price 인스턴스 수가 2 → 1 로 떨어짐

### 10.4 etcd 한 노드 (etcd1) 가 죽는 경우

- quorum 유지 (3 중 2) → 정상 서비스
- WTG 의 etcd watch 는 자동 재연결
- 즉시 복구 필요 (1 node 만 더 죽으면 quorum loss)

### 10.5 Redis master 가 죽는 경우

- Sentinel 이 새 master promote
- WTG 의 redis.UniversalClient (FailoverClient) 가 자동 재연결
- 그 사이 진행 중인 quote_id Put 은 fail → mci-price 의 swap-lock 페이지 fail_near 카운터 증가
- 회복 후 정상

### 10.6 broker reconnect supervisor goroutine — failover 의 진짜 엔진

§10.1 에서 "supervisor goroutine 이 자동 재연결" 한 줄로 넘긴 메커니즘. 외환 시스템에서 가장 자주 발생하는 사고 (broker 끊김) 의 대응을 한 곳에 정리한다.

#### 10.6.1 supervisor 는 누가, 왜

`pkg/mymq.Client` 가 broker 와 single TCP connection 을 유지하는 객체. 이 connection 이 끊기면 mci-api / mci-push / mci-price 가 broker 호출/구독을 못 한다 → 운영 중단.

→ `Client` 옵션의 `Reconnect != nil` 이면 **supervisor 라는 background goroutine** 이 하나 같이 돌아간다. 책임 3가지 :

1. **끊김 감지** — read/write 실패, EOF, heartbeat timeout
2. **재연결** — backoff 정책 + 다중 endpoint round-robin
3. **상태 복원** — 끊김 전에 등록했던 subscribe / rep receiver 설정을 재연결 후 다시 broker 에 알림

#### 10.6.2 정상 동작 — 3 goroutine 의 합작

```
                 ┌───────────────────────────────────────────┐
                 │  Client                                   │
                 │                                           │
   호출자  ────► │  Call() / Send() / Subscribe()           │
                 │       │                                   │
                 │       ▼                                   │
                 │  writer  goroutine ◄── writeMu mutex     │
                 │       │                                   │
                 │       ▼                                   │
                 │  TCP socket (broker)                     │
                 │       ▲                                   │
                 │       │                                   │
                 │  reader  goroutine ─► inflight map        │
                 │       │                  (ckey → ch)      │
                 │       ▼                                   │
                 │  4B 빈 프레임 = heartbeat                  │
                 │                                           │
                 │  supervisor goroutine                     │
                 │    - 끊김 감지                             │
                 │    - 재연결                                │
                 │    - 상태 복원                             │
                 └───────────────────────────────────────────┘
```

평상시 3 goroutine 의 역할 :
- **writer** — 호출자 요청을 broker 로 보냄. mqhdr 의 ckey 자동 발급.
- **reader** — broker 응답 frame 받아 ckey 로 inflight map lookup → 호출자에게 응답
- **supervisor** — 일정 주기 heartbeat (4B 빈 프레임) 보내고 read 실패 모니터링

#### 10.6.3 끊김 감지의 3 가지 트리거

| 트리거 | 어디서 | 의미 |
|---|---|---|
| **read EOF / RST** | reader goroutine | broker 가 깔끔히 close 또는 강제 끊김 |
| **write fail (EPIPE)** | writer goroutine | broker 죽음 / 네트워크 끊김 — 다음 호출 시 발견 |
| **heartbeat timeout** | supervisor goroutine | N초간 broker 로부터 frame 없음 (read deadline) — 좀비 connection |

세 트리거 중 어느 것이든 발생하면 → `Client` 의 `state` 가 `Connected → Disconnected` 로 전환, supervisor 가 재연결 시도 시작.

#### 10.6.4 inflight RPC 처리 — 끊김 순간

끊김 직전에 broker 응답을 기다리던 RPC 들 (`inflight map` 의 entries) 은 다음과 같이 정리 :

```
foreach (ckey, response_ch) in inflight:
    response_ch <- mymq.Error{ errn: ECONN_LOST, message: "broker disconnected" }
    close(response_ch)
inflight.clear()
```

→ 호출자 (mci-api 의 `/v1/tx` 핸들러) 가 immediately 에러로 reject → 사용자에게 503. **timeout 안 기다림** — 정확한 정보를 빠르게 돌려주는 게 운영 친화적.

이 동작이 운영 모니터링 `wtg_broker_inflight_aborted_total` 카운터로 노출. spike 가 보이면 broker failover 가 일어났다는 신호.

#### 10.6.5 재연결 — backoff + 다중 endpoint

`Options.Reconnect` 의 정책 :

| 필드 | 기본값 | 의미 |
|---|---|---|
| `MinBackoff` | 100ms | 첫 시도 사이 최소 대기 |
| `MaxBackoff` | 30s | 시도 사이 최대 대기 (cap) |
| `Multiplier` | 1.5 | 매 실패마다 backoff × N |
| `Jitter` | 10% | 동시 재연결 폭주 회피용 random |
| `Endpoints` | `[ap1.internal:11217, ap2.internal:11217]` | 시도할 broker 주소 list |

재연결 흐름 :

```
attempt = 0
while not stop:
    endpoint = endpoints[attempt % len(endpoints)]   ← round-robin
    backoff = min(MinBackoff * Multiplier^attempt, MaxBackoff)
    backoff += random_jitter
    sleep backoff

    try:
        conn = dial(endpoint)             ← TCP + TLS handshake
        register(channel, applname)        ← mymq handshake (ChannelWeb 등)
        restoreSubscriptions(conn)         ← §10.6.6
        connected!
        attempt = 0                        ← 성공 시 backoff 리셋
        break
    except:
        attempt += 1
        log.Warn("재연결 실패", endpoint, attempt)
        continue
```

본 시나리오에서 `Endpoints = [ap1:11217, ap2:11217]` → ap1 active 가 죽으면 첫 시도는 ap1 fail (refused), 두 번째 시도 ap2 가 성공 (broker cluster 가 promote 한 새 active).

> ap2 가 즉시 active 로 promote 못 한 케이스라면 (broker cluster 의 vote 지연) → 양쪽 fail → attempt 가 누적 backoff. 최대 30s 까지 늘었다가 cluster 안정화되면 연결.

#### 10.6.6 상태 복원 — 재연결 후 subscribe / rep receiver 재등록

`Client` 가 `Subscribe` / `Receive` 로 등록했던 정보들은 끊김 전에 메모리에 남아있다. 재연결 직후 supervisor 가 자동으로 broker 에 다시 등록 :

```
restoreSubscriptions(conn):
    for sub in self.subscriptions:
        conn.send_subscribe(exchange=sub.Exchange, queue=sub.Queue, ...)
    for rep in self.rep_receivers:
        conn.send_register_rep(queue="")                  ← 빈값 (§4.4.4)
```

→ mci-push 가 broker rep receiver 로 등록되어 있으면, 끊김 후에도 같은 등록이 자동 복원. 운영자가 손 댈 게 없음.

본 시나리오 사례 :
- mci-price 가 `Channel=ChannelWeb, Queue="PRICE"` 로 subscribe 등록 → ap1 죽음 → ap2 재연결 후 같은 subscribe 자동 등록 → 시세 fan-out 즉시 재개
- mci-push 가 rep receiver 등록 → 같은 패턴

#### 10.6.7 본 시나리오 ap1 → ap2 failover timeline

```
T=0.000   ap1 의 mymqd 가 SIGSEGV (예시)
T=0.010   mci-api 의 writer goroutine 이 EPIPE 감지 → inflight 12건 abort (errn=ECONN_LOST)
            wtg_broker_disconnects_total += 1
T=0.011   사용자 12명에게 503 "broker disconnected" 응답
T=0.012   supervisor 가 state = Disconnected, 재연결 attempt=1 시작
T=0.020   ap1:11217 → connection refused
T=0.120   100ms 대기 후 ap2:11217 시도
T=0.140   ap1 cluster 가 ap2 를 active 로 promote 중 (broker 내부)
T=0.150   ap2:11217 dial 성공 but mymq handshake 거부 ("not active yet")
T=0.300   attempt=2, ap1 다시 시도 → refused
T=0.450   ap2 시도 → handshake OK ! cluster promote 완료
T=0.451   restoreSubscriptions : 12건 subscribe 재등록
T=0.460   state = Connected, 새 요청 받기 시작

총 다운타임 ≈ 460ms
```

운영 모니터링 페이지에서 보이는 신호 :
- Broker disconnects rate 카드 spike
- Broker inflight aborted 카드 +12
- HTTP 5xx rate 카드 spike (사용자 503)
- 500ms 이내 모두 정상 회복 → "0 으로 떨어짐"

#### 10.6.8 흔한 함정

| 증상 | 원인 |
|---|---|
| `Reconnect = nil` 인데 broker 끊김 발생 | supervisor 미작동 → connection 영원히 끊긴 채로 남음. 모든 호출이 에러. 운영에선 절대 nil 두지 말 것 |
| 재연결 후에도 subscribe 가 안 옴 | subscribe 등록 시 `Options.Channel`/`Options.Queue` 가 메모리에 보존되어 있지만 broker 측 cluster 가 다른 노드라 상태 동기화 누락 — broker config 의 cluster sync 정책 확인 |
| 재연결 backoff 가 너무 길다 | `MaxBackoff=30s` 가 default — 30s 동안 503 폭주. 외환 운영에선 10s 정도가 적당. flag 로 조정 |
| backoff 동안 inflight RPC 가 hang | inflight 는 즉시 abort 되므로 hang 안 됨. hang 보인다면 호출자 코드의 context timeout 이 너무 김 |
| ap1 → ap2 failover 가 매번 발생 | broker cluster 의 active 가 flapping. broker 측 health check / split-brain 점검 필요 |
| 재연결 성공해도 `wtg_broker_disconnects_total` 카운터가 reset 안 됨 | 정상 — Prometheus counter 는 누적값. rate 계산 시에만 의미 |

#### 10.6.9 자주 받는 질문

- **Q. supervisor 가 매 connection 마다 하나?**
  맞다. mci-api / mci-push / mci-price 각 인스턴스가 자기 `Client` 를 가지고 각자 supervisor 가 돈다. 인스턴스 N 개면 supervisor N 개.
- **Q. 재연결 동안 새 요청 받으면?**
  `Client.Call/Send` 가 `state=Disconnected` 면 즉시 errn=ECONN_LOST 반환. 호출자가 retry 정책으로 짧게 대기 후 재시도. 사용자에겐 503 응답.
- **Q. broker cluster 가 split-brain 되면?**
  WTG 측에서 막을 길 없음. broker 측 quorum 정책으로 회피 (`docs/broker-tls.md` 의 cluster 합의안).
- **Q. broker 재연결 동안 시세도 끊기나?**
  본 시나리오는 quote-forwarder 가 publish-mode=grpc 라 broker 무관 → 시세 정상 흐름. 단 broker subscribe 도 같이 쓰면 (publish-mode=both) 동안 시세도 일시 끊김.
- **Q. graceful shutdown 시는?**
  `Client.Close()` 호출 → supervisor 가 `stop=true` set → 재연결 시도 중단 → reader/writer 정리 → 리턴. 운영 systemd 의 stop 시 호출.
- **Q. 재연결 후 ckey 가 충돌?**
  안 함. ckey 는 client 측에서 발급, 끊김 직전 inflight 는 모두 abort 되어 응답 안 받음. 새 connection 이 발급한 ckey 는 fresh.
- **Q. failover 동안 매매 1건이 broker 에선 처리되고 응답만 못 받은 경우?**
  발생 가능 (재해 케이스). 본 시나리오의 대응 = idempotency-key — 클라이언트가 매 매매에 unique key 첨부, broker AP 가 같은 key 재호출 시 cached response 반환. 자세히 `pkg/idempotency`.

---

## 11. 운영자 SOP — 본 시나리오 한정

### 11.1 마진 정책 갱신
`§5` 의 layer 별 변경 후 admin UI :
1. `🔬 마진 계산기` 로 새 정책 시뮬레이션
2. `🪄 마진 변경 미리보기` 로 영향 customer 수 / Δspread 확인
3. `💰 마진 테이블` 에서 변경 저장
4. `🧮 마진 재계산` 으로 5L customer quote 캐시 갱신
5. `💱 시세 (라이브 ws)` 에서 호가 새 값으로 흐르는지 시각 확인

### 11.2 신규 영업점 도입
1. `🧩 프로파일` 에 `WEB.BRANCH.VIP` / `WEB.BRANCH.GOLD` / `WEB.BRANCH.STD` + `CS.BRANCH.VIP/STD` 추가 (이미 있으면 skip)
2. `💰 마진 테이블` 의 Site 탭에서 새 영업점 site 가 필요하면 신설 (예: `BRANCH-NEW`) — `pkg/session/types.go` 의 `Site` enum 갱신 필요할 수도
3. `👥 사용자 프로파일` 에 영업점 직원 + 고객 등록
4. `📈 매매 통계 (alias × tier)` 로 신규 영업점 트래픽 모니터링

### 11.3 사고 대응
1. `🛡 정책 엔진` 의 Kill Switch (필요 시 채널별)
2. `📜 감사 로그` 로 사고 시작 시각 변경 이력
3. `📜 매매 감사 (최근 N건)` 로 마지막 transaction 들
4. `⚠️ Backpressure 이력` 로 부하 spike 확인
5. trace_id 모아 Tempo 에서 분산 trace 추적

---

## 12. 시나리오 의존 항목 모음

본 문서의 다음 항목들이 **이 시나리오에 한정** 됨. 환경이 다르면 조정:

| 항목 | 시나리오 가정 | 환경이 다르면 |
|---|---|---|
| MOB 채널 | 사용 안 함 | `pkg/session/types.go` 의 `Channel` enum 에서 `MOB` 제거하거나 그대로 두고 미사용 |
| Site = HQ/BRANCH 만 | 다지점 / 위탁 (`OUTSRC`) 없음 | Site enum 에 `OUTSRC` 추가 |
| Tier = VIP/GOLD/STD | 4종 이상 필요 시 | `Tier` enum 갱신 |
| AP active-standby | active-active 이면 | broker 측 cluster 정책 + WTG 측 routing 검토 (`docs/mci-price-ha.md`) |
| broker 와 매매 AP 동거 | 분리 가능 | broker 만 별도 서버로 옮기는 건 wire 호환만 맞으면 무관 |
| Redis Sentinel | Cluster 사용하면 | `--quoteid-redis` flag 가 `cluster:host:port,...` 형식 |
| TimescaleDB | 일반 Postgres | `etc/sql/quote_bars.sql` 의 hypertable / 압축 / retention 줄 제거 |
| 사내 LB = HAProxy | nginx / F5 | TLS 1.3 / WS / HTTP/2 만 지원하면 무관 |
| 사내 SSO (CS) | LDAP/AD/SAML | `/v1/cs-login` 핸들러 구현체만 교체 |

---

## 13. 검증 체크리스트 (배포 직후)

| 항목 | 명령 / 화면 |
|---|---|
| 모든 프로세스 UP | `/tmp/wtg-status.sh` |
| broker round-trip | `mci-test --ckey-echo` |
| Prometheus 모든 target up | `:9091/targets` 또는 `up{}` |
| WEB 고객 로그인 | DMZ TLS 경유 `POST /v1/login` → JWT 발급 |
| CS 직원 로그인 | 사내 SSO → `POST /v1/cs-login` → JWT 발급 |
| 시세 라이브 (WEB) | 브라우저 admin UI 의 `💱 시세 (라이브 ws)` |
| 매매 한 건 (WEB) | `POST /v1/tx alias=WEB_ORDER_NEW` → 정상 응답 |
| 매매 한 건 (CS) | `POST /v1/tx alias=CS_ORDER_NEW` → 정상 응답 |
| swap-lock 한 건 | 매칭엔진 → `wtg_price_swap_lock()` → swap_id 발급 |
| 매매 AP 검증 | `ValidateSwap(swap_id)` → OK |
| 차트 라이브 봉 | `📈 차트 (mci-chart)` 새 탭 → 캔들 streaming |
| failover 테스트 | `ap1` 의 mymqd kill → ap2 로 전환 확인 → 매매 1건 OK |

---

## 14. 매매 AP 측에서 본 WTG — mymq side 관점

지금까지는 모두 **WTG 가 broker / 매매 AP 를 어떻게 호출하는가** 의 관점. 본 절은 반대 시점 — **매매 AP (C 엔진) 가 broker 에 어떻게 등록하고, WTG 가 보내는 메시지를 어떻게 받고 응답하는지**. WTG 코드 자체에는 안 들어가지만, 매매 AP 개발자/운영자와 합의할 때 같은 wire 모델로 대화하기 위한 절.

### 15.1 매매 AP 가 broker 에 등록하는 4가지 정보

`mymq` C 라이브러리의 register 함수 호출 시 매매 AP 는 다음 4가지를 broker 에 알린다 :

| 필드 | 값 예시 | 의미 |
|---|---|---|
| **ApplName** | `WECHO`, `BWECHO`, `WECHO-2` | 프로세스 이름. `pkg/mymq/conventions.go` 의 `ApplMci*` 와 1:1 합의 |
| **Channel** | `ChannelWeb` (정수) | broker 측 client 분류 (§4.4.1 의 broker Channel) |
| **Exchange** | `WECHO` | 자기 transaction 들을 묶는 좌표 (§4.1.2 의 exchange) |
| **Queue** | `WECHO` (보통 Exchange 와 같음) | broker 가 메시지를 큐잉할 사서함 이름 (§4.4.4) |

본 시나리오의 매매 AP 등록 매핑 :

```
ApplName     Channel        Exchange    Queue       역할
─────────────────────────────────────────────────────────────────────────
AUTH         ChannelWeb     AUTH        AUTH        로그인 (WEB_LOGIN, CS_LOGIN)
QUOTESVC     ChannelWeb     QUOTESVC    QUOTESVC    호가 조회
WECHO        ChannelWeb     WECHO       WECHO       고객 매매
BWECHO       ChannelWeb     BWECHO      BWECHO      직원 매매 (CS)
BINQ         ChannelWeb     BINQ        BINQ        직원 조회
ADMIN        ChannelAdmin   ADMIN       ADMIN       운영 명령
```

> Channel 이 `ChannelWeb` 으로 통일된 이유 — 본 시나리오 모든 매매 transaction 이 사용자/직원 트래픽이라 같은 broker channel. `ChannelAdmin` 은 운영 명령 전용 분리.

### 15.2 매매 AP 가 받는 메시지 — mqhdr 의 어디를 보나

WTG 가 `Client.Call(exchange="WECHO", rkey="NEW", body=...)` 하면 broker 가 mqhdr 100B + body 를 매매 AP 로 forward. 매매 AP 는 다음을 읽는다 :

```
받은 mqhdr 100B 에서 :
  ┌─────────────────────────────────────────────────────────┐
  │ origin[16]    = "mci-api-int1"     ← 누가 보냈는지       │
  │ dest[16]      = "WECHO"            ← 자기 자신            │
  │ exchange[16]  = "WECHO"            ← 보통 dest 와 같음     │
  │ rkey[16]      = "NEW"              ← 자기 내부 핸들러 분기 │
  │ dirf          = DirRequest         ← 응답 줘야 함          │
  │ ckey[16]      = <random>           ← 응답에 그대로 echo    │
  │ trcid[16]     = <trace_id>         ← OTel 분산 추적용      │
  │ errn          = 0                  ← 호출자가 비움          │
  │ ...                                                       │
  └─────────────────────────────────────────────────────────┘

받은 body :
  { quote_id: "dev-mqbu1234-a3",
    side: "buy", amount: 1000000,
    cookie_t: "<binary blob>" }       ← §8.4 의 cookie_t passthrough
```

매매 AP 는 다음 6 단계로 처리 :

1. **rkey 로 핸들러 분기** — `"NEW"` → `handle_order_new()`
2. **cookie_t 검증** — 자체 HMAC/서명 검증 (§8.4.1)
3. **quote_id 검증** — gRPC `QuoteValidation.Validate(quote_id)` 호출 (§4.7.3)
4. **비즈니스 권한 검증** — 한도/통화쌍 활성/거래시간/slippage (WTG 가 안 하는 부분)
5. **매매 처리** — DB INSERT, ledger 갱신, 매칭 엔진 호출
6. **응답 생성** — §15.3

### 15.3 매매 AP 의 응답 — ckey echo + dirf 전환

매매 AP 는 응답 mqhdr 을 만들 때 :

| 필드 | 값 |
|---|---|
| `origin` | 자기 자신 (`WECHO`) |
| `dest` | 요청의 `origin` 그대로 (`mci-api-int1`) |
| `exchange` / `rkey` | 요청 그대로 |
| `dirf` | `DirReply` (DirRequest 의 반대) |
| **`ckey`** | **요청의 ckey 그대로 echo** — 가장 중요 |
| `trcid` | 요청 trcid 그대로 (분산 trace 유지) |
| `errn` | 성공 0, 실패 시 정의된 에러코드 |

→ broker 가 이 응답을 받아 `dest = mci-api-int1` 로 forward → WTG 의 reader goroutine 이 `ckey` 로 inflight map lookup → 호출자에게 응답 (§10.6.2).

**ckey echo 안 박으면** = WTG 가 응답 매칭 못 함 → timeout. **이게 Phase 1 GO/NO-GO 의 핵심** — `mci-test --ckey-echo` 가 매매 AP 가 ckey echo 잘 하는지 검증.

응답 body :

```
{
  success: true,
  order_id: "O-12345",
  filled_price: 1381.30,
  filled_amount: 1000000,
  cookie_t_new: "<refreshed blob>"     ← rolling 갱신 (있을 때만)
}
```

errn ≠ 0 일 때는 body 가 에러 정보 :
```
errn = ERR_PERM_DENIED                  ← 비즈니스 권한
body = { reason: "limit_exceeded", limit: 5000000, requested: 10000000 }
```

mci-api 가 이 errn 과 body 를 그대로 사용자 응답에 전달 (passthrough 원칙 §8.3).

### 15.4 본 시나리오 매매 AP 4종의 책임 매트릭스

| AP | rkey 목록 | cookie_t 검증 | quote_id 검증 | 발급 / 갱신 |
|---|---|---|---|---|
| **AUTH** | `LOGIN`, `CS_LOGIN`, `REFRESH`, `LOGOUT` | 발급/갱신/만료 | 안 함 | **cookie_t 발급자** |
| **QUOTESVC** | `GET`, `SUBSCRIBE` | 검증 | 안 함 (read-only) | 안 함 |
| **WECHO** | `NEW`, `CANCEL`, `SWAP_CONFIRM` | 검증 | Validate + MarkConsumed (또는 ValidateSwap + ConsumeSwap) | cookie_t rolling (응답에 박음) |
| **BWECHO** | `NEW`, `CANCEL` | 검증 | 같음 | 같음 |
| **BINQ** | `LOOKUP` | 검증 | 안 함 | 안 함 |
| **ADMIN** | `RELOAD`, `KILL`, `DUMP` | 운영자 role 검증 | 안 함 | 안 함 |

### 15.5 unsolicited publish — 트랙 A 의 매매 AP 측 코드 모양

매매 AP 가 사용자에게 체결 알림을 보내는 코드 (§4.6.2 의 트랙 A):

```c
/* C 의사코드 */
mymq_broadcast_t bcast;
mymq_init_broadcast(&bcast, "USERMSG");          /* Exchange */
mymq_set_logon_id(&bcast, "WEB.BRANCH.VIP|customer-1234@partner.co.kr");
mymq_set_dirf(&bcast, DirPublish);
/* dest 는 비움 — broker 가 보고 broadcast 로 인지 (§4.4.3) */

const char *body = "{\"event\":\"trade_filled\",\"order_id\":\"O-12345\",...}";
mymq_publish(&conn, &bcast, body, strlen(body));
```

→ broker 가 받아서 Queue 가 **빈값** 으로 등록된 mci-push (rep receiver) 들에게 fan-out (§4.6.2). LogonID 로 사용자 매칭 후 ws 로 전달.

### 15.6 매매 AP 측에서 본 QuoteValidation — mymq 가 아닌 gRPC

`Validate` / `MarkConsumed` / `ValidateSwap` / `ConsumeSwap` 4개 RPC 는 **mymq 가 아니라 gRPC**. mci-price 의 50051 포트에 직접 호출.

본 시나리오 매매 AP 코드 :

```c
/* C 의사코드, gRPC C++/Go 클라이언트 또는 cside/wtgprice 같은 SDK */
grpc_client_t cli;
grpc_client_init(&cli, "int1.internal", 50051);
grpc_client_set_metadata(&cli, "x-engine-id",     "wecho-ap1");
grpc_client_set_metadata(&cli, "x-engine-secret", "/etc/pki/wtg/engine-wecho-ap1.secret");

ValidateRequest req = { .quote_id = "dev-mqbu1234-a3" };
ValidateResponse resp;
grpc_call(&cli, "QuoteValidationService.Validate", &req, &resp);

if (resp.status != STATUS_OK) {
    /* §4.7.4 의 상태별 처리 */
}
```

→ 매매 AP 가 두 가지 connection 을 가진다 :
- mymq broker 11217 (TCP) — 매매 메시지 송수신
- gRPC mci-price 50051 (HTTP/2) — quote_id 검증

본 시나리오 ap1 의 매매 AP 는 `int1.internal:50051` 을 호출 (또는 VIP). int1 mci-price 가 죽으면 grpc 측 endpoint 도 fallback 필요 — `--upstream int1:50051,int2:50051` round-robin.

### 15.7 매매 AP 가 ChannelAdmin 메시지 받는 경우

본 시나리오에서 `ADMIN` AP 는 `Channel=ChannelAdmin` 으로 등록 (§15.1). 운영자가 admin UI 의 `🖥 브로커 명령` 페이지에서 `reload-config` 클릭 :

```
mci-admin → broker call : exchange="ADMIN", rkey="RELOAD", channel=ChannelAdmin
              ↓
           broker 가 channel 매칭 :
              - ChannelWeb 만 등록한 client → skip
              - ChannelAdmin 등록한 client → forward
              ↓
           ADMIN AP 만 받음 → reload 처리
```

→ §4.4.1 의 broker Channel 이 "누구에게 forward 할지" 의 추가 라우팅 layer. exchange + rkey + channel 의 3축으로 매칭.

### 15.8 자주 받는 질문

- **Q. 매매 AP 1개가 여러 Exchange 를 동시에 처리할 수 있나?**
  가능. 한 프로세스가 broker 에 여러 번 register (Exchange 별) 호출. 단 보통 1 프로세스 = 1 Exchange 가 운영 깔끔.
- **Q. WTG 가 보낸 메시지의 trcid 는 매매 AP 가 신경 써야 하나?**
  응답 mqhdr 의 trcid 에 받은 값을 그대로 echo. 매매 AP 가 내부에서 다른 시스템 (DB, 매칭엔진) 호출할 때도 trcid 를 전파해야 분산 trace 완성. `docs/broker-tracing.md`.
- **Q. ChannelAdmin 으로 등록한 AP 가 ChannelWeb 메시지도 받을 수 있나?**
  안 받는다 — 보통은 단일 channel 등록. 다중 channel 받으려면 broker register 를 channel 마다 한 번씩 해야.
- **Q. 매매 AP 가 broker 끊김 감지 후 어떻게 해야 하나?**
  C 측 mymq 라이브러리도 비슷한 supervisor 패턴이 있음 (`docs/dev-main.md`). 끊김 시 reconnect + 자기 등록 정보 복원. WTG 의 Go supervisor (§10.6) 와 같은 책임.
- **Q. 매매 AP 가 응답에 cookie_t_new 안 박으면?**
  WTG 의 Redis 의 cookie_t 가 그대로 유지 → 다음 호출에도 같은 cookie_t. rolling 안 한다는 의미. 매매 AP 가 cookie_t 의 유효기간을 다른 메커니즘으로 관리해야 (예: AUTH/REFRESH 로 주기적 갱신).
- **Q. 매매 AP 가 시세 잠금 (quote_id) 검증 실패 시 어떻게?**
  errn 에 정의된 에러코드 (예: `ERR_QUOTE_EXPIRED`) + body 에 설명 박아 응답. WTG 의 mci-api 가 errn 을 그대로 사용자 응답에 전달 (§8.3 의 passthrough 원칙).
- **Q. broker 의 cluster active 가 ap1 → ap2 로 promote 되면 매매 AP 도 같이 옮겨가나?**
  본 시나리오는 매매 AP 가 broker 와 같은 노드라 broker 가 죽으면 같이 죽음. ap2 의 매매 AP 가 따로 살아있다가 ap2 broker promote 후 자동으로 wire 처리 시작. 별도 promote 로직 불필요.

### 15.9 매매 AP 개발자에게 한 줄 요약

> "broker register 시 4가지 (ApplName/Channel/Exchange/Queue) 알리고, 받는 메시지의 mqhdr 에서 rkey 로 핸들러 분기 + cookie_t/quote_id 검증, 응답에 ckey 그대로 echo + dirf=DirReply + errn 0/에러코드. unsolicited 알림은 dest 비우고 LogonID 박아서 publish."

이 한 줄로 모든 매매 AP 가 WTG 와 호환된다.

---

## 15. 참고 문서

- `docs/admin-ui-manual.md` — 운영 매뉴얼 (37 페이지)
- `docs/deployment-software.md` — 배포 소프트웨어 명세
- `docs/operations.md` — 서비스 flag/env + 부트스트랩 순서
- `docs/auth.md` — JWT + Session.Profile + cookie_t passthrough
- `docs/conventions.md` — ApplName / Channel / Exchange / RoutingKey 카탈로그
- `docs/broker-reconnect.md` — supervisor goroutine 재연결 정책
- `docs/broker-tracing.md` — mqhdr trcid 확장
- `docs/cooker-patch.md` — broker 우회 publish 패치
- `docs/swap-trade-spec.md` — FX swap 2-leg 잠금
- `docs/quoteid-validation-rfc.md` — quote_id 검증 RFC
- `docs/mci-price-ha.md` — mci-price 다중 인스턴스 HA
