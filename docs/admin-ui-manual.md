# mci-admin 운영 매뉴얼

> WTG (Winway Trading Gateway) 의 운영 콘솔 — `mci-admin` 의 사이드바 모든 메뉴를 처음부터 끝까지 설명한다.
> 본 문서는 운영자/QA/개발자 모두가 같은 어휘로 같은 화면을 보도록 만드는 게 목적이다.
> DevMode 기준 예시 (브라우저 `http://127.0.0.1:9090/`) 로 설명하지만 운영도 거의 동일하다 — 차이가 있을 때만 별도 명시.

---

## 1. 들어가기

### 1.1 이 문서는 무엇인가
mci-admin 의 사이드바에는 7개 카테고리, 총 37개 페이지가 있다. 이 문서는 **각 페이지가 무엇을 보여주고, 어떤 endpoint 를 호출하며, 화면 요소가 각각 무슨 뜻인지** 를 한 줄도 빠뜨리지 않고 설명한다. 모든 항목에 "예시" 가 붙어 있어 실제 값이 어떻게 나오는지를 볼 수 있다.

### 1.2 mci-admin 의 위치
WTG 는 한 덩어리 게이트웨이가 아니라 여러 마이크로 서비스로 쪼개져 있다:

```
브라우저 / 외부 시스템
   │   HTTPS (DMZ)
   ▼
┌──────────────────────────────────────────────────────┐
│ mci-edge-{api, push, price, chart}   <DMZ>          │  TLS / JWT / IP allowlist / rate-limit
└──────────────────────────────────────────────────────┘
   │
   ▼
┌──────────────────────────────────────────────────────┐
│ mci-{api, push, price, chart, admin}   <Internal>   │  비즈니스 로직 / 시세 가공 / 운영 콘솔
└──────────────────────────────────────────────────────┘
   │       gRPC / mymq wire
   ▼
┌──────────────────────────────────────────────────────┐
│ mymqd broker + 매매 AP (test_service / WECHO ...)   │  C 매매 엔진 (기존)
└──────────────────────────────────────────────────────┘
```

`mci-admin` 은 **사람이 운영을 하는 GUI 콘솔** 이다. 라우팅 룰을 etcd 에 쓰고, 다른 서비스(`mci-price`, `mci-edge-price`, `quote-forwarder`)의 `/stats`/`/metrics`/`/v1/...` 같은 endpoint 를 **proxy** 로 가져와 화면에 보여준다. **자기가 직접 실시간 시세를 만들지 않는다** — 다른 서비스의 결과를 모아 보여주는 대시보드다.

### 1.3 화면 구성
- **왼쪽 사이드바** — 7 카테고리, 37 페이지 + 외부 도구 링크
- **상단 헤더** — 페이지 제목, DevMode 표식, 사용자 ID
- **본문** — 각 페이지의 카드/표/차트
- **우측 상단 작은 글씨** — "마지막 갱신 hh:mm:ss" (자동 갱신 페이지에 한함)

### 1.4 인증
- **운영** : JWT (`/v1/login` → access_token → 모든 요청에 `Authorization: Bearer ...`)
- **DevMode** (`--dev`) : `X-WTG-User: <id>` 헤더만으로 로그인. 로그인 화면의 "ID 만으로 입장 (DevMode)" 버튼. 본 문서의 예시는 모두 DevMode 기준.

### 1.5 매뉴얼 읽는 법
각 페이지 설명은 다음 6칸으로 통일했다:

| 항목           | 의미                         |
| ------------ | -------------------------- |
| **무엇**       | 한 줄로 이 페이지가 보여주는 것         |
| **endpoint** | 페이지가 백엔드의 어디를 호출하는지 (디버깅용) |
| **언제 보나**    | 운영 중 어떤 상황에서 이 페이지를 여는지    |
| **화면 요소**    | 카드/표/입력 한 칸 한 칸의 의미        |
| **예시**       | 실제 값이 어떻게 나오는지, 의미 해석 포함   |
| **주의**       | 흔한 오해, 빈 화면이 나오는 조건, 안전장치  |

---

## 2. 용어 사전 (Glossary)

이 절은 따로 떼서 두고 두고 참조. 처음 보면 어려워도 페이지 설명 보면서 다시 돌아오면 자리잡는다.

### 2.1 채널·사용자 분류

| 용어                  | 뜻                                                                                       | 값 예시                 |
| ------------------- | --------------------------------------------------------------------------------------- | -------------------- |
| **Channel**         | 사용자가 진입한 경로. `WEB` / `MOB` (모바일 앱) / `CS` (영업점 단말). 3자 enum, broker routing-key 의 첫 토큰. | `WEB`                |
| **Site**            | 본사/지점/영업소 단위. `HQ` (본사) / `BRANCH` (지점) / `OUTSRC` (위탁).                                | `BRANCH`             |
| **Tier**            | 등급. `VIP` / `GOLD` / `STD` (일반). 마진 차등의 핵심 키.                                           | `VIP`                |
| **Profile**         | Channel + Site + Tier 의 묶음. 마진/시세 fan-out 의 기본 라우팅 키. `.` 로 연결한 형태로 사용.                 | `WEB.BRANCH.VIP`     |
| **Profile.Key()**   | Profile 을 문자열로 만든 결과 (broker routing-key 의 prefix 로 박힌다).                               | `"WEB.BRANCH.VIP"`   |
| **Customer / Usid** | 실제 사용자 한 명. DevMode 에선 `x_wtg_user` 헤더 값, 운영에선 JWT 의 `usid` claim.                      | `crlee123@gmail.com` |
| **LogonID**         | broker 가 알고 있는 사용자 식별자. 80B `broadcast prefix` 의 한 필드. push fan-out target 매칭에 쓰임.      | `cr01_WEB_VIP`       |

### 2.2 거래·통화

| 용어 | 뜻 | 값 예시 |
|---|---|---|
| **Pair** | 통화쌍. **표기 형태가 두 가지** — symbol 형태 `USDKRW` (UDP/저장용) vs slash 형태 `USD/KRW` (UI/사람용). 코드 안에서 자주 `replace("/", "")` 로 변환. | `USDKRW` / `USD/KRW` |
| **Symbol** | Pair 의 symbol 형태. 외부 FIX feed 가 보내는 형태이기도 함. | `USDKRW` |
| **Tenor** | 결제일까지의 기간 이름. `SPOT` (보통 T+2) / `1W` / `1M` / `3M` / `6M` / `1Y` 등. | `SPOT`, `1M` |
| **Value Date** | 결제일 자체 (broken date). tenor 와 별개로 임의 날짜 지정 가능. | `2026-07-15` |
| **Side** | 매매 방향. spot 은 `buy`/`sell`, swap 은 `buy_sell` (near buy + far sell) / `sell_buy`. | `buy_sell` |
| **Amount** | base 통화 수량 (감사용, 가격에 영향 X). USDKRW 면 USD 수량. | `1,000,000` |

### 2.3 시세 (Quote) 파이프라인

| 용어                               | 뜻                                                                                                                                    |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| **Raw tick**                     | 외부 feed (Bloomberg / Reuters / EBS 등) 가 보내는 한 건의 호가. `bid` / `ask` 만 있다. broker 발 / UDP FIX 발 / DevTick 발 셋 중 하나.                    |
| **Feed / Source**                | 한 외부 데이터 소스. 라벨로 `SMB` (Smart Mart B), `KMB` (KEB Multi B), `EBS`, `REUT` 등. mci-price 의 BestConsumer 가 source 별 캐시를 따로 둔다.          |
| **BEST**                         | 다중 feed 중에서 **bid 는 최대, ask 는 최소** 로 합성한 호가. `src="BEST"` 라는 가짜 source 로 표현. 이게 모든 다운스트림의 단일 진실.                                     |
| **CustomerQuote (Profile-only)** | BEST tick 에 Profile 의 마진(HQ + Site spread) 을 더해서 만든 사용자별 호가. SubscribeQuote gRPC stream 으로 흐른다. P3.                                  |
| **CustomerQuote (5-Layer)**      | Customer 한 명마다 따로 만들어주는 호가. HQ + Site + **Customer** + **Window** + **Swap** 5 layer 의 마진을 모두 적용. SubscribeCustomerQuote 로 흐른다. P4c. |
| **Conflation**                   | 빠르게 들어오는 raw tick 을 일정 윈도우 안에서 마지막 값으로 덮어쓰기. `Updates` 대비 `Swaps` 가 많으면 conflation 효과 큼.                                             |
| **Backpressure**                 | 다운스트림이 못 따라잡아 큐가 가득 차려는 상태. mci-edge-price 는 queue 80% 도달 시 WARN, 100% 도달 시 클라이언트 close.                                             |

### 2.4 마진 (Margin) 5-Layer

운영 환경에서 사용자에게 보여주는 호가는 다음 5 단계를 거쳐 BEST 호가에서 변환된다 (Pricing → CustomerQuote):

```
raw bid/ask  →  ① HQ layer       (회사 전체 spread)
              →  ② Site layer     (지점/본사 추가 spread)
              →  ③ Customer layer (개별 고객 우대/페널티)
              →  ④ Window layer   (시간대별 차등, 예: 야간 spread 확대)
              →  ⑤ Swap layer     (forward tenor 의 swap point 가산)
              =  최종 customer bid/ask
```

운영 환경에선 5 layer 모두, dev 환경에선 보통 HQ + Site 만 켜 두기도 한다. **마진 계산기 페이지 (`margin-calc`)** 가 각 layer 의 기여분을 디버깅용으로 분해해서 보여준다.

| 용어 | 뜻 |
|---|---|
| **PricingTable** | 모든 layer 의 spread/skew 가 들어있는 단일 문서. etcd `wtg/pricing/table` 키. 모든 mci-price 가 watch. 변경 즉시 hot reload. |
| **Spread** | bid 와 ask 사이의 폭. customer 쪽으로는 항상 넓어진다 (bid 는 낮추고 ask 는 높임). |
| **Skew** | bid/ask 를 비대칭으로 이동. 한쪽 거래만 막거나 유도할 때. |
| **Swap point** | forward tenor 의 가격 차이. 운영자가 tenor × pair 그리드에서 직접 입력. |

### 2.5 시세 잠금 (Quote Lock) / Quote ID

매매 직전에 고객에게 보여준 호가가 실제 매매에도 동일하게 적용되어야 한다 (slippage 방지). 이를 위해 시세 한 시점을 "잠그고" 매매 AP 에 잠금 id 를 첨부한다.

| 용어 | 뜻 |
|---|---|
| **quote_id** | 한 시점의 호가 snapshot 의 식별자. spot/forward/swap 의 leg 마다 1개씩 발급. format `dev-mqbu...-a3`. |
| **swap_id** | swap 거래(near + far 2-leg) 의 묶음 식별자. `SW-` prefix. ValidateSwap 시 인자로 사용. | 
| **Registry** | quote_id ↔ Record 매핑 저장소. DevMode 는 `MemoryRegistry` (in-process), 운영은 `RedisRegistry` (active-active 공유). |
| **SwapIndex** | swap_id ↔ (near_id, far_id) 매핑. Registry 의 optional 확장. 미구현 store 면 swap endpoint 미등록. |
| **Validity** | quote_id 가 살아있는 시간 (보통 500ms). 이후 ValidateSwap/ Validate 가 `EXPIRED` 반환. |
| **MarkConsumed** | 매매가 한 번 성공하면 quote_id 를 소진 처리 (중복 거래 차단). 한 번 더 ValidateSwap 하면 `ALREADY_CONSUMED`. |
| **Engine** | quote_id 검증을 호출하는 매매 AP 의 식별자. RBAC 으로 어느 engine 이 어느 endpoint 를 부를 수 있는지 제한. |

### 2.6 라우팅 (Routing) / Transaction

| 용어 | 뜻 |
|---|---|
| **alias** | "트랜잭션 이름" — 운영자가 부르기 쉬운 이름. 예: `WECHO_PING`. |
| **Exchange** | broker 의 routing 키 단위. alias 가 resolve 되는 대상의 일부. 예: `ECHOSVC`. |
| **Routing Key** | Exchange 안의 세부 라우팅. 예: `PING`. |
| **Routing Rule** | `alias → exchange + routing_key` 매핑. etcd `wtg/routes/` 에 저장. mci-admin write, mci-api watch. |
| **ckey** | 100B `mqhdr` 의 16B correlation id. broker 가 응답에 echo back 하므로 단일 connection 으로 동시 RPC 가능. |
| **mymq wire** | C 매매 엔진의 자체 binary protocol. 100B mqhdr + navi[] + 가변영역, 4B length-prefix framing, BE network byte order. |
| **Channel (broker)** | mymq 의 채널 — 위의 사용자 Channel 과 다른 개념. `ChannelWeb`, `ChannelAdmin` 등 broker side 분류. broker 가 client 등록 시 사용. |

### 2.7 인프라

| 용어 | 뜻 |
|---|---|
| **etcd** | 라우팅 룰 + 정책 + 시세 카탈로그(symbols / pricing / profiles) 저장소. mci-admin write, 다른 서비스 watch. DevMode 는 embedded etcd 자동 기동. |
| **TimescaleDB / PostgreSQL** | 차트 봉 (`quote_bars`) 영속. mci-price.Archiver INSERT, mci-chart.Repository SELECT. |
| **Prometheus** | 운영 모니터링용 시계열 DB. `wtg_*` 메트릭을 모든 서비스가 `/metrics` 로 노출. |
| **Grafana** | Prometheus 위의 시각화/알람. firing alert 가 admin UI 의 운영 모니터링 페이지에 표시. |
| **OpenTelemetry** | 분산 추적 (trace_id 가 mymq mqhdr 의 `trcid[16]` 에 박혀 broker 도 거쳐 흐른다). |

---

## 3. 사이드바 한눈에 (Cheat Sheet)

| 카테고리 | 페이지 | 무엇 |
|---|---|---|
| **0. 대시보드** | 📊 대시보드 | 핵심 KPI 카드 모음 — 한 화면에 상태 보기 |
| **1. 운영 정책** | 🔀 라우팅 룰 | alias → exchange/routing_key 매핑 CRUD |
|  | 🛡 정책 엔진 | kill switch / 정비창 / 차단 심볼 |
|  | 🚦 Rate Limit 정책 | 서비스별 rate-limit 룰 CRUD |
| **2. 시세 마스터** | 🪙 통화 마스터 | currency 코드/이름/소수점 자릿수 |
|  | 🔗 통화쌍 마스터 | 활성 pair, 거래시간, swap_point 기본값 |
|  | 📅 선물환 스왑 | tenor × pair 그리드의 swap_point 편집 |
|  | 📆 휴일 캘린더 | 결제일 계산용 휴일 |
|  | 🧩 프로파일 | Channel.Site.Tier 묶음 목록 |
|  | 👥 사용자 프로파일 | 운영자/사용자 계정 + 권한 |
| **3. 마진 정책** | 💰 마진 테이블 | PricingTable (HQ/Site/Customer/Window/Swap) 편집 |
|  | 📜 마진 정책 명세 | 마진 정책 문서/SOP |
|  | 🔬 마진 계산기 (5-Layer) | raw → final 변환 단계별 분해 |
|  | 🧮 마진 재계산 | 정책 변경 시 일괄 재계산 트리거 |
|  | 🪄 마진 변경 미리보기 | 변경 전후 비교 |
| **4. QuoteID** | 🔑 QuoteID 엔진 | 검증 호출자(매매 AP) 등록/권한 |
|  | 🔍 QuoteID 조회 | quote_id 한 건 lookup |
|  | 📊 Validate 통계 | Validate/Consume 누적 카운터 |
| **5. 검증/호출** | 📋 서비스 명세 | broker AP 의 transaction 카탈로그 |
|  | 🔌 API 테스터 | `POST /v1/tx` 수동 호출 |
|  | 🖥 브로커 명령 | broker 직접 명령 (kill/reload 등) |
|  | 📡 WS 모니터 | mci-push / edge-price 등 ws endpoint 5종 동시 감시 |
|  | 💱 시세 (라이브 ws) | 호가창 + 통계 카드 — edge-price ws 실시간 |
| **6. 통계/감사** | 📊 운영 모니터링 | Prometheus PromQL 카드 + sparkline + Grafana alerts |
|  | 📊 시세 통계 (mci-price) | price-stats + best-stats 통합 |
|  | 📈 매매 통계 (alias × tier) | 매매 transaction 분포 |
|  | 📈 차트 통계 (mci-chart) | mci-chart 의 query 카운터 |
|  | 📜 매매 감사 (최근 N건) | 최근 트랜잭션 N건 |
|  | 📜 감사 로그 | mci-admin 의 운영 변경 이력 |
|  | 🔌 구독자 (gRPC) | mci-price 의 gRPC stream subscriber 목록 |
|  | 🔗 연결 (ws) | mci-edge-price 의 외부 ws 클라이언트 목록 |
|  | 🔍 Customer 검색 | customer_id 로 cross-reference |
|  | 📡 forwarder 통계 | quote-forwarder /stats |
|  | ⚠️ Backpressure 이력 | fan-out 채널 80% 도달 누적 |
|  | 🔁 FX swap 잠금 통계 | swap_lock 카운터 (requests/successes/fail/revoke) |
| **7. 도움말** | 📖 운영 가이드 | docs/ 의 문서 인라인 표시 |
|  | 🧰 운영 도구 (외부 링크) | Prometheus / Grafana / mci-chart 등 외부 사이트 점프 |
| **외부 도구** | 📈 차트 (mci-chart) | localhost:8086 (직접) — 새 탭 |
|  | 📈 차트 (mci-edge-chart) | localhost:8087 (DMZ proxy) — 새 탭 |

---

## 4. 대시보드 (📊)

### 무엇
운영 시작 시 첫 화면. 한눈에 "현재 시스템이 살아있나" 를 판단하는 KPI 카드 모음.

### endpoint
- `/v1/admin/stream` (SSE) — 핵심 카운터 실시간 push
- `/v1/admin/audit` (최근 감사 로그 일부)
- 그 외 카드는 다른 페이지의 일부 데이터를 가져옴

### 언제 보나
- 출근 후 첫 1분 — 어제 밤 사이 죽은 게 있는지
- 알림 받은 직후 — 어디가 문제인지 빠르게 좁히기 위해
- 다른 페이지로 가기 전 "오늘 상태가 평소와 다른가" 확인

### 화면 요소
| 카드 | 의미 |
|---|---|
| **mode** | DevMode / Prod / Standalone 등 운영 모드 표시 |
| **broker 상태** | mymqd 와의 연결 상태 — `● 연결됨` / `● 끊김` |
| **시세 rate** | mci-price 의 received tick/s |
| **subscribers** | gRPC stream 구독자 수 (= 거의 mci-edge-price 인스턴스 수) |
| **connections** | mci-edge-price 에 붙은 외부 ws 클라이언트 수 |
| **최근 감사** | 최근 N건 운영자 변경 이벤트 |

### 예시
```
mode: dev / no-broker
broker: ● disconnected  (--no-broker 정상)
시세 rate: 28 tick/s
subscribers: 3  (tick/quote/customer 각 1건씩)
connections: 1
```
DevMode 의 정상 상태. `broker: disconnected` 가 빨갛게 보일 수 있는데 `--no-broker` 옵션 때문이라 정상.

### 주의
- 모든 카드가 동시에 0 으로 떨어지면 mci-admin 자체나 etcd 가 끊겼다는 신호 — 다른 페이지로 이동해도 아무것도 안 보인다.
- "broker 끊김" 이 운영에서 보이면 즉시 broker 페이지로 가서 reconnect 시도.

---

## 5. 운영 정책 (Operational Policy)

이 카테고리의 변경은 **즉시 운영에 영향** 을 준다. 모든 변경은 etcd 에 commit 되고 mci-api / mci-price 등이 watch 로 즉시 반영한다. 변경 전에 항상 "감사 로그에 누가 변경했는지 남는다" 는 점을 의식할 것.

### 5.1 🔀 라우팅 룰 (`routes`)

#### 무엇
"alias 라는 별명" 을 "broker 의 exchange + routing_key" 로 변환하는 룰을 관리. 매매 transaction 의 분기 지점.

#### endpoint
- `GET /v1/admin/routes` — 전체 룰 목록
- `POST /v1/admin/routes` — 새 룰 추가
- `PUT /v1/admin/routes/:alias` — 룰 수정
- `DELETE /v1/admin/routes/:alias` — 룰 삭제
- etcd `wtg/routes/` 에 저장 → mci-api 가 watch.

#### 언제 보나
- 새 매매 transaction 을 도입할 때 alias 등록
- broker AP 가 옮겨가서 exchange 이름이 바뀔 때 routing_key 만 갱신
- "왜 어떤 transaction 이 503 으로 떨어지는가" 디버깅 — 룰이 없으면 mci-api 가 reject

#### 화면 요소
| 열 | 의미 | 예시 |
|---|---|---|
| **alias** | 운영자가 부르는 이름 | `WECHO_PING` |
| **exchange** | broker exchange 이름 | `ECHOSVC` |
| **routing_key** | exchange 안의 세부 키 | `PING` |
| **created/updated** | 변경 이력 (감사용) | `2026-06-13 14:32` (`cr01`) |

상단 검색창 + "➕ 새 룰" 버튼. 각 행 끝의 ✏️ 가 수정, 🗑 가 삭제.

#### 예시
```
alias=WECHO_PING  exchange=ECHOSVC  routing_key=PING
alias=W_QUOTE     exchange=QUOTESVC routing_key=GET
```
브라우저에서 `POST /v1/tx` body 가 `{"alias":"WECHO_PING","data":""}` 일 때 위 룰이 `{"exchange":"ECHOSVC","routing_key":"PING"}` 로 resolve 되어 broker 로 간다.

#### 주의
- alias 변경/삭제는 즉시 운영 영향. **모든 클라이언트가 그 alias 를 안 쓰는 게 확인되기 전에는 삭제 금지.**
- exchange/routing_key 가 broker AP 와 일치하지 않으면 mci-api 호출이 broker 에서 "navigation 없음" 또는 "queue 없음" 으로 떨어진다.
- DevMode 에선 mci-admin 부팅 시 "dev-seed" 룰 6개가 자동 들어간다.

### 5.2 🛡 정책 엔진 (`policy`)

#### 무엇
**비상 정지 / 정비창 / 특정 심볼 차단** 같은 거시 정책 게이트. mci-api / mci-price 가 매 요청마다 본 정책을 참고해 reject 한다.

#### endpoint
- `GET /v1/admin/policy` — 현재 정책 doc
- `PUT /v1/admin/policy` — 정책 갱신
- etcd `wtg/policy` 에 단일 doc.

#### 언제 보나
- **사고 발생** : 즉시 거래 중단 → `kill_switch=true`
- **시스템 정비** : 새벽 작업 전에 `maintenance_window` 설정해서 거래 reject
- **특정 통화 의심 거래 차단** : `blocked_symbols` 에 USDKRW 추가

#### 화면 요소
| 항목 | 의미 |
|---|---|
| **Kill switch** | 토글 한 개로 전체 거래 즉시 중단. ON → 모든 transaction이 503 with `policy_blocked` |
| **정비창** | 시작/종료 시각 (UTC). 이 안에 들어오는 거래는 reject |
| **차단 심볼** | 콤마 구분 list (예: `USDKRW, EURUSD`) |
| **현재 상태 배지** | 운영 ON / 정비 / kill 등 상태 색상 표시 |

#### 예시
```
Kill switch: OFF
정비창: (없음)
차단 심볼: (없음)
→ 정상 운영
```
사고 시:
```
Kill switch: ON  (cr01 이 2026-06-13 14:50 변경)
→ 모든 거래 즉시 503 응답
```
브라우저에서 `POST /v1/tx` 요청이 `{"error":"policy_blocked","reason":"kill_switch"}` 응답.

#### 주의
- **Kill switch 는 곧바로 운영 거래를 막는다** — 누가 켰는지 항상 감사 로그에 남는다.
- 정비창은 UTC 로 입력. 한국시간 02:00 정비창이면 17:00 UTC 부터.
- 본 페이지 자체가 안 뜨면 etcd 가 안 떠 있을 가능성.

### 5.3 🚦 Rate Limit 정책 (`ratelimit`)

#### 무엇
"분당 N건" / "초당 M건" 같은 호출 빈도 제한 룰을 서비스별로 관리.

#### endpoint
- `GET /v1/admin/ratelimit/:service` — 한 서비스의 룰
- `PUT /v1/admin/ratelimit/:service` — 갱신
- `DELETE` — 룰 한 줄 삭제
- 서비스: `edge-api`, `edge-price`, `edge-push`, `edge-chart`

#### 언제 보나
- 외부 클라이언트의 비정상 호출 폭주 — 특정 endpoint 만 더 빡세게 조이기
- 새 API 추가 후 default rate 가 적합한지 검증
- 봇/스크래퍼 의심 IP 의 rate 만 따로 낮추기

#### 화면 요소
- 상단 토글 — 서비스 선택 (`edge-api`, `edge-price`, ...)
- 표 — 각 룰의 `rule_id` (path/IP/사용자 기반 매칭) + limit + window + action
- "✏️ 룰 추가" 모달 — rule_id, limit (예: `100`), window (예: `1s`/`1m`), action (`deny`/`throttle`)
- "총 거부 건수" 카드 — 최근 5분간 본 서비스에서 reject 된 요청 수

#### 예시
```
service: edge-api
─────────────────────────────────────────────────────────
rule_id              limit  window  action  reject(5m)
POST /v1/login        5      1m      deny     0
POST /v1/tx          100     1s      throttle 12
GET  /v1/quote       500     1s      throttle 0
```
`POST /v1/login` 이 분당 5건이라 누가 로그인 5회 실패 후 6번째에 즉시 reject. throttle 은 거부 대신 큐에 대기.

#### 주의
- 너무 빡세게 잡으면 정상 트래픽도 차단 — 변경 후 "총 거부 건수" 카드를 5분 정도 지켜볼 것.
- DevMode 에선 룰이 거의 비어 있는 경우가 많음.

---

> 본 매뉴얼은 §6 시세 마스터 → §7 마진 정책 → §8 QuoteID → §9 검증/호출 → §10 통계/감사 → §11 도움말/외부도구 순으로 이어집니다. 각 카테고리도 위와 동일한 6칸 (무엇 / endpoint / 언제 / 화면 요소 / 예시 / 주의) 으로 작성됩니다.

## 6. 시세 마스터 (Quote Master)

운영 환경에서 시세 파이프라인의 "정적인 카탈로그" 를 관리. 거의 모든 항목이 **etcd → mci-price watch → 즉시 hot reload** 라 변경이 곧바로 운영 시세에 반영된다. 변경 전 항상 영향 범위를 의식할 것.

### 6.1 🪙 통화 마스터 (`currency`)

#### 무엇
USD/KRW/EUR/JPY 등 **통화 코드** 한 줄짜리 마스터. 통화의 표기명, 소수점 자릿수, 활성 여부. 통화쌍/마진/봉 모든 곳의 기초.

#### endpoint
- `GET <base>/v1/currency` — 전체 통화 목록
- `POST <base>/v1/currency` — 새 통화 등록
- `PUT/DELETE` — 갱신/삭제
- 운영에선 `fx-sync` CLI 가 외환 운영 DB(Oracle) → etcd 미러링. mci-admin 페이지는 그 결과를 보고 수동 보정.

#### 언제 보나
- 신규 통화 도입 (예: TRY, BRL) — 거래 가능하게 만들기 전 등록 필요
- 소수점 자릿수 잘못 들어가서 차트 / 호가 표시가 이상할 때 (예: JPY 가 2자리인데 4자리로 설정)
- 비활성 통화 정리

#### 화면 요소
| 열 | 의미 | 예시 |
|---|---|---|
| **code** | ISO 4217 3자 코드 | `USD`, `KRW` |
| **name** | 사람용 이름 | `미국 달러` |
| **decimals** | 호가/봉 표시 자릿수 | `2` (USDKRW 의 KRW), `4` (EURUSD 의 USD) |
| **active** | 거래/시세 활성 여부 | `true` |
| **updated** | 마지막 갱신 시각 + 변경자 | `2026-06-13 (fx-sync)` |

상단 검색창 + "➕ 새 통화" 버튼.

#### 예시
```
code  name        decimals  active   updated
USD   미국 달러     4         true    2026-06-13 (fx-sync)
KRW   한국 원       2         true    2026-06-13 (fx-sync)
JPY   일본 엔       2         true    2026-06-13 (fx-sync)
TRY   터키 리라     4         false   2025-12-01 (cr01)
```
TRY 가 비활성 — 차트/호가에 안 나오고 매매도 reject.

#### 주의
- `decimals` 변경은 **이미 저장된 봉/호가 데이터의 자릿수를 바꾸지 않음** — 신규 데이터만 영향. 과거 차트가 들쭉날쭉 보일 수 있음.
- 통화 비활성화는 그 통화를 쓰는 모든 pair 도 자동으로 거래 불가가 된다.

### 6.2 🔗 통화쌍 마스터 (`pair-master`)

#### 무엇
거래 가능한 **통화쌍** 마스터. 어떤 통화쌍을 활성화할지, 거래시간은 언제인지, swap_point 기본값은 얼마인지.

#### endpoint
- `GET <base>/v1/pairs` — 전체 통화쌍 목록
- `POST/PUT/DELETE <base>/v1/pairs/{symbol}`

#### 언제 보나
- 신규 통화쌍 거래 시작 (예: NZD/KRW)
- 시즌별 거래시간 변경 (예: 동/하절기)
- swap_point 기본값 변경

#### 화면 요소
| 열 | 의미 |
|---|---|
| **symbol / pair** | `USDKRW` / `USD/KRW` |
| **base / quote** | base = `USD`, quote = `KRW` (USDKRW 의 경우) |
| **active** | 거래 활성 여부 |
| **trade_window** | 거래 가능 시간 (UTC, day-of-week 별) |
| **default_swap** | tenor 별 swap_point 기본값 (1W/1M/3M/...) |
| **min/max amount** | 거래 한도 (감사용) |

#### 예시
```
symbol  pair      base quote active  trade_window     default_swap (1M)
USDKRW  USD/KRW   USD  KRW   true   24h              +0.85
EURKRW  EUR/KRW   EUR  KRW   true   24h              +1.10
JPYKRW  JPY/KRW   JPY  KRW   true   24h              -0.02
NZDKRW  NZD/KRW   NZD  KRW   false  -                -
```
NZDKRW 가 비활성 — 매매 reject, 시세 fan-out 도 안 됨.

#### 주의
- pair 비활성은 즉시 모든 다운스트림에 반영 — 영업시간에 변경하면 라이브 ws 연결이 그 pair 만 사라진다.
- default_swap 은 "기본값" — 실제 매매에서는 `swap` 페이지의 운영자 편집값(spread / skew) 이 우선.

### 6.3 📅 선물환 스왑 (`swap`)

#### 무엇
**tenor × pair 그리드** 형태로 forward 의 swap_point (bid/ask) 를 운영자가 직접 편집. PricingTable 의 `swap` layer 의 본체.

#### endpoint
- `GET /v1/admin/pricing/table` — 전체 PricingTable 가져옴 (다른 layer 보존용)
- `PUT /v1/admin/pricing/table` — swap layer 만 patch 한 doc 으로 덮어쓰기
- etcd `wtg/pricing/table` 에 단일 doc.

#### 언제 보나
- 시장 변동 시 forward curve 수동 조정
- 신규 tenor 추가 (예: `2Y`)
- 신규 pair 의 swap_point 초기 셋업

#### 화면 요소
- 상단 — `[🔄 다시 로드]` `[💾 저장]` `[↩ 취소]` `[➕ pair 추가]` `[➕ tenor 추가]` 버튼
- 표 — 가로 = tenor (SPOT, 1W, 1M, 3M, 6M, 1Y, ...), 세로 = pair (USDKRW, EURKRW, ...)
- 각 셀 — `bid / ask` 두 칸. 편집하면 baseline 과 비교해 dirty 면 노란 배경
- 하단 카운터 — 변경된 셀 수, 저장 시 보낼 patch

#### 예시
```
              SPOT      1W        1M        3M        6M        1Y
USDKRW    0.00/0.00  0.10/0.20  0.85/0.95  2.40/2.55  4.80/5.00  9.50/10.00
EURKRW    0.00/0.00  0.05/0.15  1.10/1.30  3.20/3.50  6.50/6.80  ...
JPYKRW    0.00/0.00 -0.01/-0.01 -0.02/-0.02 -0.06/-0.06 -0.12/-0.12 ...
```
SPOT 은 보통 0 (forward 가 아님), tenor 가 길수록 swap_point 절대값 ↑.

#### 주의
- **다른 layer (HQ/Site/Customer/Window) 는 본 페이지가 건드리지 않음.** GET 으로 전체 doc 받아서 swap layer 만 patch → PUT. 동시에 다른 운영자가 다른 layer 편집하면 race 가능성 (마지막 PUT 이 이긴다).
- 저장 즉시 mci-price 가 watch 로 받아 5-Layer 마진 계산에 반영. 라이브 호가가 한 박자 바뀐다.
- "취소" 누르면 dirty 셀이 baseline 으로 복귀.

### 6.4 📆 휴일 캘린더 (`holidays`)

#### 무엇
통화별 **결제일 계산용 휴일** 마스터. spot/forward 의 value_date 가 휴일이면 다음 영업일로 roll-forward.

#### endpoint
- `GET <base>/v1/holidays` — 통화별 휴일 목록
- `POST/DELETE` — 휴일 추가/삭제

#### 언제 보나
- 매년 말 다음 연도 휴일 일괄 입력
- 임시 휴일 (예: 미국 추도일) 추가
- "왜 SPOT value_date 가 어제로 안 잡히고 모레로 잡혔지?" — 휴일 확인

#### 화면 요소
| 열 | 의미 |
|---|---|
| **currency** | 통화 코드 (USD, KRW, EUR ...) |
| **date** | YYYY-MM-DD |
| **label** | 휴일 이름 (예: `Independence Day`) |
| **kind** | `public` / `bank` 등 분류 |

#### 예시
```
currency  date         label                kind
USD       2026-07-03   Independence Day     public
USD       2026-11-26   Thanksgiving         public
KRW       2026-01-01   신정                  public
KRW       2026-09-17   추석                  public
```
USDKRW 의 spot (T+2) 거래가 7/1 에 발생하면 7/3 이 USD 휴일이므로 value_date 가 7/6 으로 roll.

#### 주의
- 휴일이 빠지면 **결제일이 잘못 계산되어 거래 정산 실패** 위험. 매년 말 점검 필수.
- 통화별 휴일이 합쳐져 결제일 계산 — USDKRW 면 USD 휴일 ∪ KRW 휴일 둘 다 회피.

### 6.5 🧩 프로파일 (`profiles`)

#### 무엇
**Channel.Site.Tier 묶음 목록.** Profile 페이지는 운영자가 사용할 Profile 의 정의 자체를 등록/삭제하는 곳. 예시 형태로 같은 묶음을 PricingTable / 시세 fan-out / 마진 계산에 모두 쓴다.

#### endpoint
- `GET /v1/admin/profiles` — 전체 Profile
- `POST /v1/admin/profiles` — 추가
- `DELETE /v1/admin/profiles/:key` — 삭제 (key = `WEB.BRANCH.VIP`)

#### 언제 보나
- 새 영업 채널 (예: 신규 모바일 앱) 도입 시 Profile 신설
- 등급 체계 변경 (예: `GOLD` 추가/삭제)
- 사용 안 하는 Profile 정리

#### 화면 요소
| 열 | 의미 | 예시 |
|---|---|---|
| **channel** | `WEB` / `MOB` / `CS` | `WEB` |
| **site** | `HQ` / `BRANCH` / `OUTSRC` | `BRANCH` |
| **tier** | `VIP` / `GOLD` / `STD` | `VIP` |
| **key** | 결합 (`Channel.Site.Tier`) — broker routing-key prefix | `WEB.BRANCH.VIP` |
| **created** | 등록일 + 등록자 |  |

상단 "➕ Profile 추가" 모달 — 3개 dropdown 으로 입력.

#### 예시
```
channel  site    tier   key
WEB      BRANCH  VIP    WEB.BRANCH.VIP
WEB      BRANCH  GOLD   WEB.BRANCH.GOLD
WEB      BRANCH  STD    WEB.BRANCH.STD
WEB      HQ      VIP    WEB.HQ.VIP
MOB      BRANCH  VIP    MOB.BRANCH.VIP
CS       HQ      VIP    CS.HQ.VIP
```
이 10개 Profile 이 그대로 PricingTable 의 row 가 되고, 시세 fan-out 의 routing-key 가 된다.

#### 주의
- **Profile 삭제 = 그 Profile 의 마진 row 도 의미 없어짐.** 삭제 전 PricingTable 에서 해당 row 도 비워둘 것.
- Profile.Key() 가 routing-key prefix 라 길이 제한이 있음 — Channel 은 3자 이내 권장.

### 6.6 👥 사용자 프로파일 (`users`)

#### 무엇
**실제 사용자 (Customer / Usid) 와 그가 속한 Profile 의 매핑.** 어떤 customer 가 어떤 Channel.Site.Tier 로 분류되는지.

#### endpoint
- `GET /v1/admin/user-profiles`
- `POST /v1/admin/user-profiles`
- `DELETE /v1/admin/user-profiles/:usid`

#### 언제 보나
- 신규 가입 사용자에게 Profile 부여
- 등급 승급/강등 (예: VIP → GOLD)
- 영업점 이관 (예: BRANCH-A → BRANCH-B)
- "왜 이 사용자가 마진을 잘못 받는가?" — Profile 매핑 확인

#### 화면 요소
| 열 | 의미 | 예시 |
|---|---|---|
| **usid** | 사용자 식별자 (이메일 또는 회원번호) | `crlee123@gmail.com` |
| **profile_key** | 매핑된 Profile | `WEB.BRANCH.VIP` |
| **roles** | 운영자 권한 (있을 때) — `admin`, `viewer` | `[]` 또는 `["admin"]` |
| **created/updated** | 변경 이력 |  |

상단 "➕ user 추가" 모달.

#### 예시
```
usid                      profile_key        roles
crlee123@gmail.com        WEB.BRANCH.VIP     ["admin"]
trader-01@partner.co.kr   WEB.BRANCH.GOLD    []
mobile-12345              MOB.BRANCH.STD     []
```
DevMode 에서 `x_wtg_user=crlee123@gmail.com` 으로 진입하면 위 매핑이 적용되어 Profile=WEB.BRANCH.VIP 로 시세/마진 수신.

#### 주의
- 사용자 Profile 변경은 **즉시 마진 계산에 영향** — 영업시간 중 바꾸면 호가가 갑자기 달라진다.
- `roles=["admin"]` 이 있어야 운영 페이지의 변경 버튼이 활성. 운영 시 권한 부여 신중.

---

## 7. 마진 정책 (Margin Policy)

마진은 WTG 의 비즈니스 핵심이다. 운영자가 PricingTable 을 잘못 만지면 모든 고객 호가가 즉시 어긋난다. 본 카테고리의 5 페이지는 **계산 / 정의 / 디버깅 / 일괄 재계산 / 미리보기** 의 5 단계 도구다.

### 7.1 💰 마진 테이블 (`pricing`)

#### 무엇
PricingTable 전체를 **layer 별 탭** 으로 보여주고 편집. HQ / Site / Customer / Window / Swap 5 layer + 통합 미리보기.

#### endpoint
- `GET /v1/admin/pricing/table` — 전체 doc
- `PUT /v1/admin/pricing/table` — 전체 doc 덮어쓰기
- etcd `wtg/pricing/table`

#### 언제 보나
- 신규 마진 정책 셋업 (초기 운영)
- 분기/월별 정책 갱신
- 운영 사고 후 마진 원복

#### 화면 요소
- 상단 layer 탭 — `HQ` / `Site` / `Customer` / `Window` / `Swap` / `미리보기`
- 각 탭 — pair × tenor 그리드 (Swap 과 동일 형식)
- HQ 탭 — pair 별 단일 spread/skew (Profile 무관)
- Site 탭 — Profile × pair 그리드 (Profile 별 차등)
- Customer 탭 — Customer × pair (개별 우대)
- Window 탭 — 시간 윈도우 × pair (예: 09-18 / 18-09)
- Swap 탭 — 위 6.3 페이지와 동일
- 미리보기 탭 — 임의 Profile/Customer 입력 시 통합된 최종 bid/ask 카드 표시

#### 예시 (Site 탭)
```
                    USDKRW       EURKRW       JPYKRW
WEB.BRANCH.VIP     -0.07/+0.07  -0.10/+0.10  -0.005/+0.005
WEB.BRANCH.GOLD    -0.15/+0.15  -0.20/+0.20  -0.010/+0.010
WEB.BRANCH.STD     -0.30/+0.30  -0.40/+0.40  -0.020/+0.020
```
VIP 가 spread 가장 좁고 STD 가 가장 넓음 — 같은 BEST 호가 1380.5 에서 VIP 는 1380.43/1380.57, STD 는 1380.20/1380.80 받음.

#### 주의
- **저장 전 항상 미리보기 탭으로 검증.**
- 모든 layer 가 row 가 비어있으면 그 Profile/Customer 는 BEST 호가 그대로 받음 (마진 0).
- 변경 즉시 운영 반영 — 사고 위험이 가장 큰 페이지.

### 7.2 📜 마진 정책 명세 (`margin-policy`)

#### 무엇
PricingTable 의 **읽기 전용 문서/SOP**. 정책의 의미와 운영 절차를 본 페이지에서 인라인으로 본다.

#### endpoint
- `fetch("docs/margin-policy.md")` (정적 마크다운 인라인)

#### 언제 보나
- 신규 운영자 인수인계
- 정책 의미가 헷갈릴 때 (예: skew 의 부호 규칙)
- 사고 후 reason 작성 시 정책 인용

#### 화면 요소
- 상단 — 마크다운 렌더링 영역
- 우측 사이드바 — TOC (목차)
- 하단 — "운영 가이드 전체 보기" 링크

#### 예시
> 본 페이지가 보여주는 마크다운 = `docs/margin-policy.md` 의 내용.

#### 주의
- 본 페이지는 정책 자체가 아니라 정책 **설명서**. 정책 변경은 `margin-policy.md` 파일 + 본 `pricing` 페이지에서 동시 진행.

### 7.3 🔬 마진 계산기 (5-Layer) (`margin-calc`)

#### 무엇
"임의 입력 (Profile + Customer + pair + tenor + 시각) 에 대해 5 layer 마진이 어떻게 적용되어 최종 bid/ask 가 나오는지 **단계별 분해**". 디버깅의 핵심.

#### endpoint
- `POST /v1/admin/pricing/preview` — body 에 입력, 응답에 단계별 결과

#### 언제 보나
- 운영자가 PricingTable 변경 전 영향 시뮬레이션
- "왜 이 사용자가 X 의 호가를 받았지?" 사후 분석
- 신규 마진 정책 검증

#### 화면 요소
상단 **Inputs**:
- pair (dropdown)
- tenor (dropdown 또는 직접 입력)
- profile_key (dropdown)
- customer_id (선택)
- timestamp (default = now)
- raw bid/ask (선택, 빈값이면 현재 BEST)

본문 **5 단계 카드**:
| 카드 | 의미 |
|---|---|
| **0. Inputs** | 위 입력 echo |
| **1. Active TimeWindows** | 시각에 활성인 윈도우 라벨 |
| **1.5 Value Date 보간** | broken-date 일 때 양 옆 tenor 가중평균 |
| **2. Swap** | swap_point 적용 후 bid/ask |
| **3. HQ** | HQ spread/skew 적용 |
| **4. Site** | Site spread/skew 적용 |
| **5. Customer** | Customer 우대 적용 |
| **6. Final 산식** | 최종 customer bid/ask + 총 spread |

#### 예시
```
Inputs: USDKRW / 1M / WEB.BRANCH.VIP / crlee123 / 2026-06-13 14:00 / raw bid=1380.5 ask=1381.0

1. TimeWindows: [DAY 09-18]
2. Swap (1M): +0.85/+0.95 → bid=1381.35 ask=1381.95
3. HQ: spread ±0.05 → bid=1381.30 ask=1382.00
4. Site (VIP): ±0.07 → bid=1381.23 ask=1382.07
5. Customer (cr): -0.02/+0.00 → bid=1381.21 ask=1382.07
Final: bid=1381.21 ask=1382.07  (총 spread 0.86)
```

#### 주의
- 본 페이지는 read-only 시뮬레이션 — PricingTable 을 바꾸지 않는다.
- raw 입력 비우면 현재 BEST 호가를 잠깐 가져와 계산.
- DevMode 에선 layer 일부가 비어있을 수 있음 — "계산 안 됨" 표시.

### 7.4 🧮 마진 재계산 (`margin`)

#### 무엇
PricingTable 변경 후 **저장된 5-Layer customer quote 캐시를 한 번에 다시 계산** 하도록 mci-price 에 트리거. 정책 변경 직후 호가가 즉시 새 값으로 fan-out 되게 한다.

#### endpoint
- `POST /v1/admin/margin/recompute` — body 로 옵션 (전체 / 특정 Profile 만 / 특정 customer 만)
- mci-price 의 PricingConsumer 가 트리거 받아 캐시 무효화 + 즉시 재계산

#### 언제 보나
- PricingTable 큰 변경 후
- 운영 사고 복구 (잘못된 마진 캐시 강제 갱신)
- Customer layer 추가/삭제 후

#### 화면 요소
- 상단 **선택 입력** — Profile / Customer / "전체" 토글
- 본문 **▶ 재계산 실행** 버튼
- 하단 — 진행 상태 + 결과 카드 (`재계산 row 수`, `소요 시간`)
- 최근 트리거 이력 표 (누가 언제 무엇)

#### 예시
```
선택: 전체
▶ 재계산 실행

결과: 10 Profile × 6 pair = 60 row, 1,234 customer quote 갱신, 소요 247ms.

이력:
2026-06-13 14:32  cr01  전체   60 row   247ms
2026-06-13 13:00  cr02  VIP    18 row    82ms
```

#### 주의
- 큰 customer 수에서 "전체" 트리거는 mci-price 부하 spike. 가능하면 Profile 단위로 쪼개기.
- 본 트리거는 mci-price 의 캐시만 무효화 — PricingTable 자체는 이미 etcd 에 있어야.

### 7.5 🪄 마진 변경 미리보기 (`margin-wizard`)

#### 무엇
"PricingTable 을 **이렇게 바꾸면**" 의 가정 하에 현재 운영 중인 모든 customer 의 호가가 **before/after 어떻게 달라지는지** 매트릭스로 보여준다. 일종의 dry-run.

#### endpoint
- `POST /v1/admin/pricing/preview-matrix` — body 에 변경 patch, 응답에 before/after 비교

#### 언제 보나
- 큰 정책 변경 직전 — 영향받는 customer/Profile 수 + 평균 호가 변화 측정
- 정책 회의 자료 만들기 (변경 시 누가 얼마나 영향)

#### 화면 요소
- 좌측 — 변경 patch 입력 (어떤 layer 의 어느 cell 을 얼마로 바꿀지)
- 우측 표 — Profile × pair 그리드, 각 셀에 `Δbid / Δask` (변화량)
- 하단 KPI 카드 — `영향 customer 수`, `평균 Δspread`, `최대 Δspread (어느 Profile)`

#### 예시
```
Patch: WEB.BRANCH.VIP / USDKRW / spread +0.03

           USDKRW         EURKRW         JPYKRW
WEB.BRANCH.VIP    Δ-0.03/+0.03    0/0      0/0
WEB.BRANCH.GOLD       0/0         0/0      0/0
...

영향 customer: 247명 (VIP)
평균 Δspread: +0.06
최대 Δspread: +0.06 (단일 Profile)
```

#### 주의
- 본 페이지는 **저장하지 않음.** 미리보기만. 실제 변경은 `pricing` 페이지에서.
- 큰 patch (전체 layer 교체) 는 응답 크기가 커서 1-2초 걸린다.

---

> §8 QuoteID → §9 검증/호출 → §10 통계/감사 (12 페이지) → §11 도움말/외부도구 순서로 이어집니다.

## 8. QuoteID

매매 직전에 호가를 "잠그고" 매매 AP 가 그 잠금 id 로 호가를 다시 검증하는 트랙. 이 카테고리는 그 잠금 시스템의 **호출자(엔진) 권한 / 한 건 추적 / 누적 통계** 의 3 페이지.

용어 복습: `quote_id` = 한 시점 호가 snapshot 의 id, `Registry` = id↔Record 저장소, `Validity` = 보통 500ms, `MarkConsumed` = 한 번 매매 후 소진 처리, `Engine` = 검증을 호출하는 매매 AP 식별자.

### 8.1 🔑 QuoteID 엔진 (`quoteid-engines`)

#### 무엇
**매매 AP (= 엔진) 를 등록 / 권한 부여** 하는 RBAC 페이지. 어떤 엔진이 어떤 endpoint (Validate / MarkConsumed / ValidateSwap / ConsumeSwap) 를 호출할 수 있는지 화이트리스트.

#### endpoint
- `GET /v1/admin/quoteid-engines` — 전체 엔진 목록
- `POST /v1/admin/quoteid-engines` — 새 엔진 등록
- `PUT /v1/admin/quoteid-engines/:engine_id` — 권한 갱신
- `DELETE /v1/admin/quoteid-engines/:engine_id`

#### 언제 보나
- 신규 매매 AP (예: WECHO_V2) 도입 시 엔진 등록
- 권한 조정 (예: 어떤 엔진에서 ConsumeSwap 만 제거)
- "이 엔진이 왜 Validate 가 403 으로 떨어지나?" — 권한 확인

#### 화면 요소
| 열 | 의미 |
|---|---|
| **engine_id** | 매매 AP 식별자 (자기 자신 호출 시 인증에 사용) | `wecho-1` |
| **description** | 사람용 설명 | `메인 매매 엔진 #1` |
| **allowed_ops** | 호출 가능한 RPC 목록 — `validate`, `mark_consumed`, `validate_swap`, `consume_swap` 중 일부 | `["validate","mark_consumed"]` |
| **secret_hash** | 호출 시 인증할 secret 의 hash (저장은 hash 만) | `sha256:...` |
| **created/updated** | 변경 이력 |  |

상단 "➕ 엔진 추가" 모달 — engine_id, description, allowed_ops 체크박스, secret 입력 (저장 시 hash 만 유지).

#### 예시
```
engine_id   description           allowed_ops                                        secret_hash
wecho-1     메인 매매 엔진 #1     ["validate","mark_consumed","validate_swap","consume_swap"]  sha256:abc...
wecho-2     보조 매매 엔진        ["validate","mark_consumed"]                          sha256:def...
audit-bot   감사 봇 (read-only)   ["validate"]                                          sha256:ghi...
```
audit-bot 은 Validate 만 가능 — MarkConsumed 호출 시 403.

#### 주의
- **secret 은 등록 후 한 번만 화면에 표시.** 잊으면 새로 발급해야 함.
- engine_id 삭제 = 그 엔진의 매매 호출이 즉시 reject. 운영 중 삭제는 반드시 사전 공지.
- DevMode 에선 보통 비어있거나 dev-seed 1-2건.

### 8.2 🔍 QuoteID 조회 (`quoteid-lookup`)

#### 무엇
**quote_id 한 건 lookup** — Registry 에 등록된 Record 의 모든 필드 (호가/Profile/customer_id/issued/valid_until/소진 여부) 를 표시. 사후 분석의 핵심 도구.

#### endpoint
- `GET /v1/admin/quoteid-lookup?id=<quote_id>` — Registry 에서 조회

#### 언제 보나
- 매매 실패 분석 — 어떤 quote_id 로 매매했는지 + 호가가 어땠는지
- "왜 ValidateSwap 이 EXPIRED 라고 돌려보냈지?" — issued/valid_until 확인
- 운영 incident report 작성용 evidence

#### 화면 요소
- 상단 — quote_id 입력 + "▶ 조회"
- 결과 카드:
  - **id** — 입력한 quote_id
  - **status** — `active` / `expired` / `consumed` / `not_found`
  - **호가** — bid/ask + raw_bid/raw_ask (마진 차이 보기)
  - **Profile** — 어느 Profile 로 발급되었나
  - **customer_id** — (있을 때)
  - **issued** — 발급 시각 (절대 + 상대 "0.4s ago")
  - **valid_until** — 만료 시각
  - **consumed_by / consumed_at** — 소진 정보 (있을 때)
  - **issuer** — 어느 mci-price 인스턴스가 발급했나
  - **raw JSON** — 토글 가능한 전체 Record dump

#### 예시
```
입력: dev-mqbu1234-a3

id:           dev-mqbu1234-a3
status:       active (만료까지 0.32s)
bid/ask:      1380.43 / 1381.27   (raw 1380.50 / 1381.20)
Profile:      WEB.BRANCH.VIP
customer_id:  crlee123@gmail.com
issued:       2026-06-13 14:32:15.001  (0.18s ago)
valid_until:  2026-06-13 14:32:15.501
issuer:       dev (mci-price@local)
```
0.32s 만 더 살아있음 — 매매 AP 가 ValidateSwap 호출하기 직전 timing race 디버깅 시 결정적.

#### 주의
- DevMode (MemoryRegistry) 는 mci-price 재시작 시 모든 quote_id 가 날아감 — "방금 발급한 id 가 not_found" 면 그 사이 재시작 의심.
- 운영 (RedisRegistry) 은 active-active 공유. lookup 결과가 모든 mci-price 인스턴스에서 동일해야.
- 보안 — 사용자가 다른 사람의 quote_id 를 조회 못 하도록 운영에선 권한 체크.

### 8.3 📊 Validate 통계 (`quoteid-stats`)

#### 무엇
Validate / MarkConsumed / ValidateSwap / ConsumeSwap 의 **누적 카운터** + 상태별 분포 (`ok`, `denied`, `not_found`, `expired`, `already_consumed`).

#### endpoint
- `GET /v1/admin/quoteid-stats` — 전체 통계

#### 언제 보나
- 매매 호출 성공률 모니터링
- "denied 가 갑자기 증가" — 엔진 권한 문제 or Registry 장애
- "already_consumed 비율 증가" — 클라이언트 중복 호출 의심

#### 화면 요소
| 카드 | 의미 |
|---|---|
| **Validate (단일)** | total / ok / denied / not_found / expired / already_consumed 분포 |
| **MarkConsumed (단일)** | 같은 카테고리 |
| **ValidateSwap** | swap 단위, 추가로 "둘 다 OK" / "한 leg fail" 분류 |
| **ConsumeSwap** | 같음 + partial-race 카운터 (두 mci-price 동시 ConsumeSwap → 한 leg ALREADY) |
| **rate (5m)** | 각 RPC 의 분당 rate |

#### 예시
```
Validate          total 12,847  ok 12,701 (98.86%)  denied 0  not_found 24  expired 110  already_consumed 12
MarkConsumed      total 12,700  ok 12,698  already_consumed 2
ValidateSwap      total    340  ok    332 (97.65%)  one_leg_fail 6  both_fail 2
ConsumeSwap       total    332  ok    332  partial_race 0

rate (5m):
  Validate     ≈ 42/s
  ValidateSwap ≈ 1.1/s
```
expired 가 110 건 — 클라이언트가 너무 늦게 호출하는 케이스. validity 늘리거나 클라이언트 retry timing 점검.

#### 주의
- 본 카운터는 mci-price 재시작 시 리셋. 장기 추세는 Prometheus 의 `wtg_quoteid_op_total` 메트릭에서.
- `denied` 가 0 이 아니면 즉시 §8.1 엔진 페이지에서 권한 확인.

---

## 9. 검증 / 호출 (Verification & Manual Call)

운영자가 **직접 손으로** broker / 매매 AP / ws endpoint 를 두드려보는 도구들. 운영 중 사고 진단 + 신규 transaction 도입 시 사전 검증의 핵심.

### 9.1 📋 서비스 명세 (`svcio`)

#### 무엇
broker 의 매매 AP 가 제공하는 **transaction 카탈로그**. 어떤 exchange × routing_key 가 있고, 각각의 입력/출력 schema 가 무엇인지.

#### endpoint
- `GET /v1/admin/svcio/services` — 전체 service 목록
- `GET /v1/admin/svcio/services/:exchange/transactions/:rkey` — 한 transaction 의 schema

#### 언제 보나
- 신규 transaction 을 routing rule 에 등록하기 전 schema 확인
- API 테스터로 호출하기 전 input 형식 잡기
- "이 응답 필드가 무슨 의미지?" 사후 해석

#### 화면 요소
- 좌측 — exchange 목록 (트리)
- 우측 — 선택된 transaction 의 schema 카드:
  - **request fields** (이름/타입/필수/설명)
  - **response fields** (이름/타입/설명)
  - **example request/response** (JSON)
  - **deprecated 표식** (있을 때)

#### 예시
```
Exchange: ECHOSVC
  PING       (request: empty,  response: {pong: bool})
  ECHO       (request: {msg},  response: {echo: string})

Exchange: QUOTESVC
  GET        (request: {symbol}, response: {bid, ask, ts})
  SUBSCRIBE  (request: {symbols[]}, response: stream)
```
운영자가 `routes` 페이지에서 `alias=W_QUOTE_GET` 을 `QUOTESVC/GET` 으로 매핑하려고 할 때 본 페이지로 schema 확인.

#### 주의
- 본 페이지의 카탈로그는 broker 의 ApplName 컨벤션 (`pkg/mymq/conventions.go`) 와 동기화되어야. 어긋나면 호출 실패.
- DevMode 에서 broker 없이는 비어있거나 정적 seed 만 표시.

### 9.2 🔌 API 테스터 (`tester`)

#### 무엇
운영자가 **`POST /v1/tx` 를 손으로 호출** — alias + body 입력해서 응답 확인. broker 까지 round-trip.

#### endpoint
- `POST /v1/tx` (mci-api 의 generic envelope)

#### 언제 보나
- 신규 alias 등록 직후 동작 검증
- broker AP 변경 후 호환성 테스트
- 사용자가 보고한 에러 재현
- 운영 사고 시 broker reachability 확인

#### 화면 요소
- 상단 **입력 form**:
  - alias (dropdown, routes 에서 가져옴)
  - 또는 raw (exchange + routing_key)
  - request body (JSON 에디터)
  - "▶ 호출" 버튼
- 본문 **응답 카드**:
  - status (`200` / `4xx` / `5xx`)
  - latency (ms)
  - response body (JSON, pretty-print)
  - error code (`mymq.Error.errn` — broker 가 reject 시)
  - "↗ 라우팅 룰 보기" 링크
- 하단 **최근 호출 N건 이력**

#### 예시
```
alias: WECHO_PING
body:  ""
▶ 호출

→ 200 (latency 12ms)
   { "pong": true, "echo_seq": 47, "received_at": "2026-06-13T05:32:15Z" }
```
ckey echo back 까지 잘 됐다는 신호 — Phase 1 의 GO/NO-GO.

#### 주의
- DevMode `--no-broker` 면 모든 호출이 503 `broker disconnected`. mci-test 의 ckey-echo 와 같은 역할이지만 broker 가 살아야 의미 있음.
- body 가 JSON 이 아니어도 됨 — broker AP 가 string/binary 받으면 그대로 전송.

### 9.3 🖥 브로커 명령 (`broker`)

#### 무엇
broker (mymqd) 에 **운영 명령** (재로드, 통계 조회, 특정 큐 비우기 등) 직접 발사. broker AP 의 admin transaction 을 호출.

#### endpoint
- `POST /v1/admin/broker/command` — mci-api 가 ChannelAdmin 으로 broker 호출

#### 언제 보나
- broker 가 이상 상태 — 재로드/통계 확인
- 특정 큐 backlog 확인
- broker 측 정책 hot reload

#### 화면 요소
- 상단 **사전 정의된 명령 버튼** — `reload-config`, `dump-queues`, `dump-subscriptions`, `flush-queue (이름 입력)` 등
- 상단 banner — broker 연결 상태 (`● 연결됨` / `● 끊김`)
- 본문 **응답 표시 영역** — raw response or 파싱된 통계 카드
- 모든 버튼은 broker 끊김 상태에서 disabled

#### 예시
```
● broker 연결됨 (mymqd 127.0.0.1:11217)

[reload-config]
→ ok (config v17 → v18, 0.3s)

[dump-queues]
→ ECHOSVC.PING       depth 0  oldest 0s
  QUOTESVC.GET       depth 2  oldest 0.04s
  ...
```

#### 주의
- **운영에서 admin 명령은 다른 운영자에게 영향**. 항상 확인 모달 거치도록.
- DevMode `--no-broker` 면 모든 버튼 disabled.

### 9.4 📡 WS 모니터 (`wsmon`)

#### 무엇
**5개 ws endpoint 를 동시에 감시**. 각 endpoint 별로 독립 WebSocket 을 열어 메시지 도착/heartbeat/끊김을 한 화면에서 확인.

#### endpoint (각 ws 가 직접 연결)
- `ws://localhost:8081/v1/push` (mci-push)
- `ws://localhost:8083/v1/subscribe` (mci-edge-price)
- `ws://localhost:8084/v1/push` (mci-edge-push)
- `ws://localhost:8086/v1/chart/stream` (mci-chart)
- `ws://localhost:8087/v1/chart/stream` (mci-edge-chart)
- DevMode 의 자동 query 첨부 (`?x_wtg_user=...`), 운영의 JWT 첨부 자동.

#### 언제 보나
- 새 endpoint 도입 후 라이브 동작 확인
- 운영 중 "특정 endpoint 만 끊긴 것 같다" 판별
- 부하 테스트 중 4가지 endpoint 가 동시에 살아있는지 확인

#### 화면 요소
- 5개 endpoint 카드 (가로 나열). 각 카드:
  - URL + 상태 (`● idle / connecting / live / closed / error`)
  - 1초 rate (수신 message/s)
  - 최근 메시지 1건 (raw JSON 일부)
  - "▶ 연결" / "■ 종료" 버튼
- 상단 **공통 토글** — DevMode 의 user 입력칸

#### 예시
```
mci-push        ● closed       0 msg/s     ▶ 연결
mci-edge-price  ● live         87 msg/s    {sym:"USDKRW",bid:1380.5,ask:1381.7,...}
mci-edge-push   ● error        ws connect failed: ECONNREFUSED 8084
mci-chart       ● connecting   -
mci-edge-chart  ● closed       0 msg/s     ▶ 연결
```
edge-price 만 연결되어 있고 push/chart 는 미연결 — push/edge-chart 서비스가 안 떴거나 endpoint 가 다른 상태.

#### 주의
- 카드를 동시에 다 켜면 envelope rate 가 합쳐져 브라우저 부하 ↑. 부하가 큰 endpoint 는 필요한 것만 연결.
- 본 페이지를 떠나도 ws 가 자동으로 close 됨 (페이지 unload).

### 9.5 💱 시세 (라이브 ws) (`quote`)

#### 무엇
**호가창 + 통계 + 5-Layer 마진 시각화** 한 화면. mci-edge-price 의 `/v1/subscribe` 에 연결해 실시간 호가를 받아 pair 별 카드로 표시.

#### endpoint
- `ws://localhost:8083/v1/subscribe?x_wtg_user=<u>&profile=<p>` (mci-edge-price)
- DevMode 는 query 로 user/profile 자동 첨부. 운영은 JWT 안에 claim.

#### 언제 보나
- 시세 파이프라인이 살아있는지 시각적 확인
- 마진 적용 결과 직접 보기 (BEST 호가 vs Profile customer 호가 vs 5-Layer customer 호가)
- 라이브 ws 부하 테스트 시각 검증
- 사용자가 "내 화면이 안 움직인다" 보고 시 운영자 재현

#### 화면 요소
- **상단 입력** — URL (default `ws://localhost:8083/v1/subscribe`), 통화쌍 필터, Profile (default `WEB.BRANCH.VIP`), Customer ID (DevMode 의 x_wtg_user)
- **상태 배지** — `● idle / connecting / live / closed / error`
- **연결 버튼** — `▶ 연결` / `■ 종료`
- **통계 카드 4종**:
  - **총 메시지** — 전체 envelope 수
  - **체결** — trade entries 수 (dev tick 에선 0)
  - **customer quote** — Profile-only quote 수 (SubscribeQuote 경로)
  - **5L quote** — 5-Layer customer quote 수 (SubscribeCustomerQuote 경로)
  - **rate** — 1초 윈도우
- **호가창 grid** — 통화쌍 별 row:
  - sym, feed (보통 `BEST`), ts
  - **BEST**: bid / ask (raw)
  - **CustomerQuote (Profile)**: bid / ask
  - **CustomerQuote (5L)**: bid / ask + customer_id
  - 변화 화살표 (↑/↓), 1초 시계열 sparkline
- **최근 체결 표** (TRADE_LIMIT=50)

#### 예시
```
URL: ws://localhost:8083/v1/subscribe   profile: WEB.BRANCH.VIP
상태: ● live      rate: 87/s
─────────────────────────────────────────────────────────────────────────────
총 메시지 1,234   체결 0   customer quote 411   5L quote 411

sym       BEST            Profile         5L (cr01)
USDKRW    1380.50/1381.20 1380.43/1381.27 1380.41/1381.29
EURKRW    1500.15/1501.65 1500.08/1501.72 1500.06/1501.74
JPYKRW    9.498/9.518     9.491/9.525     9.489/9.527
...
```
BEST → Profile → 5L 로 갈수록 spread 가 넓어진다 — 마진 정책 정상 작동.

#### 주의
- **queue_cap=256 이 작아 부하 (~1000 env/s) 가 들어오면 mci-edge-price 가 backpressure close** — 본 문서의 §10.10 backpressure 이력 참고.
- DevMode 에서 ws 가 안 붙으면 `--no-broker` 와 무관, `mci-edge-price` 자체 미기동 가능성 — `/tmp/wtg-dev-status.sh` 로 확인.
- Profile 입력값이 `profiles` 페이지에 등록 안 된 값이면 Profile customer quote 가 안 옴 — 카드 카운터가 0 유지.

---

> §10 통계/감사 (12 페이지) → §11 도움말/외부도구 → 마무리 (운영 시나리오 모음) 순서.

## 10. 통계 / 감사 (Statistics & Audit)

12 페이지로 가장 큰 카테고리. 운영 중 "지금 시스템이 어떤지" 보는 모든 카드/표/차트가 여기 있다. 운영 중 가장 자주 여는 카테고리.

본 카테고리의 페이지는 크게 4 묶음:
- **§10.1~10.5** — 시세/매매/차트의 **시계열 통계**
- **§10.6** — 운영 변경 **감사 로그**
- **§10.7~10.10** — 라이브 시스템 진단 (구독자/연결/customer/forwarder)
- **§10.11~10.12** — 부하/사고 (backpressure / swap_lock)

### 10.1 📊 운영 모니터링 (`monitoring`)

#### 무엇
**Prometheus PromQL 카드 + sparkline + Grafana firing alerts** 한 화면. 운영 사고 시 가장 먼저 여는 페이지.

#### endpoint
- `GET /v1/admin/prom-query?path=query&query=<PromQL>` — 인스턴트 값
- `GET /v1/admin/prom-query?path=query_range&query=...&start=&end=&step=` — sparkline 용 시계열
- `GET /v1/admin/grafana-alerts` — firing alerts (`wtg-*` 그룹만)
- mci-admin 의 `--prom-url` 와 `--grafana-url` 가 채워져야 활성. 비어있으면 페이지 상단 **`#mon-banner`** 가 "Prometheus 미구성" 안내.

#### 언제 보나
- 출근 후 첫 화면 — 어제 밤 사이 알람 있었는지
- 알림 받은 직후 — 어느 카드가 spike 인지
- 정책 변경 직후 — 카운터가 의도대로 움직이는지 (예: ratelimit 변경 후 denied 가 증가/감소)

#### 화면 요소
- **상단 banner 2종**:
  - `#mon-banner` — Prometheus 미구성 (admin 에 `--prom-url` 없음)
  - `#mon-alert-banner` — firing alert 있음 (Grafana → `wtg-*` 그룹)
- **window 선택** — `5m` / `15m` / `1h` (default 5m). 모든 PromQL 의 `[__W__]` 가 이 값으로 치환.
- **MON_CARDS** 카드 그리드:
  - 각 카드 — 카드명 + 인스턴트 값 + 1초 dot (rate) + 우측 sparkline (40 포인트, 윈도우의 2배 길이)
  - 상태 dot — 임계값 미만 회색, 초과 노랑/빨강
- **카드 목록**:

| 카드 | PromQL | 의미 |
|---|---|---|
| HTTP 요청 rate | `sum(rate(wtg_http_requests_total[__W__]))` | 전체 서비스 HTTP RPS |
| HTTP 5xx rate | `sum(rate(wtg_http_requests_total{status=~"5.."}[__W__]))` | 5xx 발생률 — 즉시 0 유지가 정상 |
| RateLimit denied | `sum(rate(wtg_ratelimit_denied_total[__W__]))` | rate-limit 차단률 |
| Login RL denied | `sum(rate(wtg_ratelimit_denied_total{rule="POST /v1/login"}[__W__]))` | 로그인 brute force 의심 |
| Broker disconnects | `sum(rate(wtg_broker_disconnects_total[__W__]))` | broker 끊김 빈도 |
| Broker inflight aborted | `sum(rate(wtg_broker_inflight_aborted_total[__W__]))` | 미완 RPC 가 broker 끊김으로 강제 종료된 수 |
| QuoteID denied | `sum(rate(wtg_quoteid_op_total{status="denied"}[__W__]))` | 엔진 권한 차단 |
| QuoteID already_consumed | `sum(rate(wtg_quoteid_op_total{status="already_consumed"}[__W__]))` | 중복 호출 |

- **하단 알람 표** — firing alert 의 name, severity, summary, started_at

#### 예시
```
window: 5m

HTTP 요청 rate          5.47/s   ▁▁▂▂▃▃▃▂▁▂▃▄ ▄▄
HTTP 5xx rate           0        ──────────────
RateLimit denied        0        ──────────────
Broker disconnects      0        ──────────────
QuoteID denied          0        ──────────────
QuoteID already_consumed 0       ──────────────

firing alerts: (없음)
```
정상 운영. 5xx 또는 broker disconnects 가 0 이 아니면 즉시 다른 페이지로 가서 원인 추적.

#### 주의
- **`(empty)` = 메트릭 자체가 0 회 increment 라 series 가 없는 상태.** JS 가 `parseFloat(...??"0")||0` 로 0 처리. 카드는 0 으로 보이지만 실제 의미는 "한 번도 안 발생".
- Prometheus scrape 간격이 5s 면 카드 sparkline 의 첫 데이터까지 5-10초 지연.
- DevMode 에서 `--prom-url` 안 주면 모든 카드 ERR + banner 표시.

### 10.2 📊 시세 통계 (mci-price) (`pricestats`)

#### 무엇
mci-price 의 `/v1/price-stats` + `/v1/best-stats` 통합 — **시세 파이프라인의 누적 카운터와 per-pair BEST 호가**.

#### endpoint
- `GET /v1/admin/price/price-stats` → mci-price `/v1/price-stats`
- `GET /v1/admin/price/best-stats` → mci-price `/v1/best-stats`

#### 언제 보나
- 시세가 정상으로 흐르는지 빠르게 확인 (received/matched/dropped)
- pair 별 best_bid/best_ask 가 정상 범위인지
- conflation 효과 측정 (Updates vs Swaps)
- latency 분포 점검

#### 화면 요소
**price-stats 카드**:
| 필드 | 의미 |
|---|---|
| **received** | broker / UDP / DevTick 으로 들어온 raw tick 총수 |
| **matched** | exchange 필터 통과한 tick (`PRICE` exchange 만 처리) |
| **dropped** | 디코딩 실패 등 |
| **sub_drops** | broker subscribe 채널 backpressure drop |
| **sub_buffer_size** | 현재 채널 buffer 사용량 |
| **conflation.Symbols** | 캐시에 들어있는 symbol 수 |
| **conflation.Updates** | 캐시 갱신 시도 총수 |
| **conflation.Swaps** | 직전 값이 swap (덮어쓰기) 된 횟수 = conflation 효과 |
| **latency.avg_ms / max_ms** | tick 도착 ~ fan-out 까지 시간 |
| **latency bucket** | <1ms / <10ms / <100ms / <1s / ≥1s 분포 |
| **latency.negative_count** | timestamp 가 음수 (서버 시각 동기 문제) 횟수 |

**best-stats 표** (per-pair):
| 열 | 의미 |
|---|---|
| symbol | USDKRW 등 |
| active_sources | 동시에 호가를 보내는 source 수 |
| best_bid / best_ask | 합성 BEST 호가 |
| crossed_fallback | 두 source 의 호가대가 달라 best_bid > best_ask 가 발생 → 최신 source 호가로 fallback |

#### 예시
```
price-stats:
  received     91,860
  matched      91,860
  dropped           0    ← 모든 tick 정상 디코딩
  sub_drops         0    ← backpressure 없음
  conflation:
    Symbols 6  Updates 356,308  Swaps 356,302   ← 거의 모든 tick 이 conflate (latest 만 유지)
  latency: avg 0.4ms  max 12ms  (<1ms 99.8%)

best-stats:
  USDKRW   sources=2  bid=1380.48  ask=1381.68
  EURKRW   sources=2  bid=1499.89  ask=1501.39
  JPYKRW   sources=2  bid=9.504    ask=9.524   crossed_fallback=true
  ...
```
JPYKRW 가 crossed_fallback — 두 source 의 호가 사이 mismatch. 일시적 (시장 변동) 이면 OK, 지속되면 source 하나가 stale.

#### 주의
- `sub_drops > 0` 이면 broker 의 subscribe rate 가 너무 빠르거나 mci-price 가 느림 — `SubBufferSize` 늘리기.
- `latency.negative_count` 가 늘면 서버 시각 동기 (NTP) 점검.
- DevMode 에선 dev tick 만으로 `active_sources=1`. UDP forwarder 추가하면 2.

### 10.3 📈 매매 통계 (alias × tier) (`aliasstats`)

#### 무엇
**매매 transaction (alias) 별 + Tier 별 호출 분포**. 어떤 사용자 등급이 어떤 매매를 얼마나 호출하는지.

#### endpoint
- `GET /v1/admin/alias-stats` — mci-api 가 누적한 카운터

#### 언제 보나
- 새 매매 alias 의 트래픽이 정상 분포인지
- "VIP 사용자 매매가 줄었나?" — 영업 분석
- 봇 의심 — 특정 alias × tier 만 spike

#### 화면 요소
- 표 — alias × tier 매트릭스 (행 = alias, 열 = VIP/GOLD/STD/CS)
- 각 셀 — 호출 수 + 평균 latency (ms) + 에러율 (%)
- 상단 — window 선택 (5m/1h/24h)
- 상단 KPI — 전체 RPS / 평균 latency / 에러율

#### 예시
```
window: 1h
total: 12,400 calls   avg 23ms   err 0.4%

                   VIP        GOLD       STD        CS
WECHO_PING        500 5ms 0%   200 7ms 0%   100 8ms 0%    0
W_QUOTE_GET     4,200 12ms 0% 2,100 14ms 0% 1,800 18ms 1% 50
W_ORDER_NEW       800 35ms 0%  400 42ms 0%  300 50ms 2% 10
W_ORDER_CANCEL    150 30ms 0%   80 35ms 0%   60 45ms 1%  0
...
```
STD 의 W_QUOTE_GET 에러율 1% — STD 만 영향 받는 무언가 (예: rate-limit) 확인.

#### 주의
- 본 카운터는 mci-api 재시작 시 리셋. 장기 추세는 Prometheus.
- 일별 누적은 별도 Grafana dashboard (운영 도구 페이지에서 링크).

### 10.4 📈 차트 통계 (mci-chart) (`chartstats`)

#### 무엇
**mci-chart 의 query 카운터 + ws hub 상태**. historical 조회 / 라이브 stream 의 health.

#### endpoint
- `GET /v1/admin/chart-stats` → mci-chart `/stats`

#### 언제 보나
- 차트 페이지가 느릴 때 — query latency / rows 확인
- 라이브 봉 stream 이 살아있는지
- TimescaleDB 쿼리 부하 점검

#### 화면 요소
| 카드 | 의미 |
|---|---|
| **requests** | historical query 누적 |
| **errors** | 실패 누적 |
| **rows** | 반환 row 총수 |
| **bars_received** | mci-price 로부터 SubscribeBar 로 받은 봉 수 |
| **hub.count** | 현재 ws 클라이언트 수 |
| **hub.sent** | ws 로 fan-out 한 메시지 수 |
| **hub.dropped** | ws queue overflow drop |

#### 예시
```
requests:        14    errors:  0     rows:    1,247
bars_received:   132   (1m 봉이 1초마다 1개 close 되는데 132개 = 2분 11초 가량)
hub: count=2  sent=264  dropped=0
```
ws 2명 (chart 페이지 연 사용자 2명) 이 정상 수신 중.

#### 주의
- `hub.dropped > 0` 이면 차트 페이지의 ws queue (default 256) 가 작거나 클라이언트가 느림.
- `errors > 0` 이고 `requests` 거의 같으면 DB 연결 끊김 — postgres / TimescaleDB 점검.
- DevMode 에선 DB 없이도 라이브 stream 만 동작 가능 (rows=0).

### 10.5 📜 매매 감사 (최근 N건) (`recenttx`)

#### 무엇
mci-api 가 처리한 **최근 N건 매매 transaction** 의 raw 흐름. trace_id + 입출력 + latency.

#### endpoint
- `GET /v1/admin/recent-tx?limit=100`
- mci-api 가 메모리 ring buffer 로 유지 (재시작 시 초기화)

#### 언제 보나
- 사용자가 "내 매매가 안 됐다" 보고 — usid + 시각으로 검색
- 운영 사고 직후 — 무엇이 마지막으로 들어왔는지
- 새 alias 도입 후 정상 흐름 확인

#### 화면 요소
- 상단 filter — usid, alias, status, 시각 범위
- 표 — 시각순 desc:

| 열 | 의미 |
|---|---|
| ts | 도착 시각 (ms 정밀) |
| usid | 사용자 |
| alias | transaction 이름 |
| status | http status |
| latency | total ms |
| trace_id | OTel trace |
| errn | broker 에러코드 (실패 시) |

- 한 행 클릭 → 상세 모달 (입력 body / 출력 body / mqhdr 일부 / 라우팅 결과)

#### 예시
```
14:32:15.234  crlee123  WECHO_PING      200  12ms  trace=...  -
14:32:15.180  crlee123  W_QUOTE_GET     200  18ms  trace=...  -
14:32:14.892  user02    W_ORDER_NEW     503  45ms  trace=...  policy_blocked
14:32:14.560  user03    W_ORDER_CANCEL  200  35ms  trace=...  -
```
user02 의 W_ORDER_NEW 가 policy_blocked 로 reject — `policy` 페이지에서 kill_switch 여부 확인.

#### 주의
- ring buffer 크기 (mci-api flag) 가 작으면 오래된 건 사라짐.
- 본 페이지는 transaction body 까지 보임 — 운영 환경에선 권한 체크 필수.
- trace_id 로 Grafana Tempo (또는 Jaeger) 와 연동되면 클릭으로 분산 trace 점프 가능.

### 10.6 📜 감사 로그 (`audit`)

#### 무엇
**운영자가 mci-admin 에서 한 변경 이력**. 누가 언제 어디서 무엇을 바꿨는지. 컴플라이언스 + 사고 분석.

#### endpoint
- `GET /v1/admin/audit?limit=...&since=...&actor=...` — ring buffer 또는 영속 store

#### 언제 보나
- 사고 분석 — "이상한 PricingTable 값이 언제부터?"
- 분기 감사 (audit) 보고서
- 새 운영자 교육 — "이전 운영자는 어떤 변경을 했었는지"

#### 화면 요소
- 상단 filter — actor (사용자), action (`update_route`, `update_pricing`, ...), 시각, kind
- 표 (시각 desc):

| 열 | 의미 |
|---|---|
| ts | 변경 시각 |
| actor | 운영자 usid |
| action | 변경 종류 (예: `pricing.update`, `route.create`, `policy.killswitch_on`) |
| target | 대상 key (예: `WECHO_PING`, `wtg/policy`) |
| diff_summary | 한 줄 요약 (예: `swap.USDKRW.1M.bid: 0.80 → 0.85`) |
| ip | 운영자 IP |

- 한 행 클릭 → 상세 모달 (full before/after JSON diff)

#### 예시
```
14:32:00  cr01  pricing.update         wtg/pricing/table   swap.USDKRW.1M.bid: 0.80→0.85
13:50:12  cr02  route.create           WECHO_NEW           +exchange=ECHOSVC routing=NEW_V2
13:32:48  cr01  policy.killswitch_on   wtg/policy          channels=[WEB]
12:14:55  cr03  user-profile.update    user02              tier: GOLD → VIP
```
12:14 에 user02 가 GOLD → VIP 로 승급, 13:32 에 WEB 채널 kill switch 활성 — 사고 timeline 의 결정적 단서.

#### 주의
- ring buffer 가 작으면 오래된 이력 손실 — 운영에선 영속 store (Postgres) 권장.
- **감사 로그는 절대 삭제 안 됨.** 사고 후 사후 분석의 마지막 보루.
- 운영 환경에서 본 페이지는 `audit` role 만 보이도록 권한 게이트.

---

> §10.7~10.12 (subscribers / connections / customer-search / fwdstats / backpressure / swaplock) 와 §11 (도움말/외부도구) 다음 응답에서 마무리.

### 10.7 🔌 구독자 (gRPC) (`subscribers`)

#### 무엇
mci-price 의 **gRPC PriceService 에 붙어있는 모든 stream 구독자** 목록. mci-edge-price 가 거의 유일한 정상 구독자.

#### endpoint
- `GET /v1/admin/price/subscribers` → mci-price `/v1/subscribers`

#### 언제 보나
- mci-edge-price 인스턴스가 살아있는지 (N6 다중 인스턴스의 fan-out 확인)
- queue_depth 가 어느 구독자에서 차고 있는지 — backpressure 직전 진단
- 새 mci-edge-price 인스턴스 배포 후 정상 등록 확인

#### 화면 요소
4개 stream 별 표:
- **tick_subscribers** — raw BEST tick 구독자 (id, srv_id, symbols filter, queue_depth/cap)
- **quote_subscribers** — Profile-routed CustomerQuote 구독자 (profiles filter, pairs filter)
- **bar_subscribers** — SubscribeBar 구독자 (mci-chart 가 주 사용자)
- **customer_quote_subscribers** — 5-Layer CustomerQuote (subscriber_id, customer_count)

각 row 의 `queue_depth / queue_cap` 비율 표시 — 80% 넘으면 노랑, 100% 넘으면 빨강.

#### 예시
```
tick_subscribers (1):
  id=2  srv_id=mci-edge-price@host  symbols=[]  queue 0/1024

quote_subscribers (1):
  id=3  srv_id=mci-edge-price@host  profiles=[]  pairs=[]  queue 0/1024

bar_subscribers (1):
  id=5  srv_id=mci-chart@host  queue 0/256

customer_quote_subscribers (1):
  id=4  subscriber_id=mci-edge-price@host  customer_count=12  queue 0/1024
```
4 stream 모두 정상 등록 + queue 비어있음.

#### 주의
- `symbols=[]` / `profiles=[]` 는 **필터 없음 = 전체 수신**. 의도된 상태.
- mci-edge-price 가 끊기면 본 페이지에서 row 가 사라짐 — `/tmp/wtg-dev-status.sh` 로 확인.
- N6 다중 인스턴스에선 srv_id 가 인스턴스별로 다름 — fan-out 분포 시각화.

### 10.8 🔗 연결 (ws) (`connections`)

#### 무엇
mci-edge-price 의 **외부 ws 클라이언트 목록**. 누가 어느 IP 에서 어떤 Profile 로 ws 를 잡고 있는지.

#### endpoint
- `GET /v1/admin/edge/connections?customer_id=&profile=` → mci-edge-price `/v1/connections` (N6 의 다중 instance 모두 합산)

#### 언제 보나
- "내 ws 가 안 받아져요" 보고 — 실제 연결되어 있는지
- backpressure close 직전 누가 queue 채우고 있는지
- 보안 — 의심 IP 발견 시 차단 결정
- 부하 분포 (N6 instance 별)

#### 화면 요소
- **상단 검색** — customer_id / profile_key 필터 + "🔄 새로고침" / "▶ 자동 polling 2s"
- **summary** — `N 연결 (필터 적용 후, M instance)` + `by_instance:` 칩 + `by_profile:` 칩
- **표** (행 = 연결):
  - id (edge 측 sub_id)
  - customer_id (DevMode = x_wtg_user, 운영 = JWT.usid)
  - profile_key (Channel.Site.Tier)
  - remote_addr (IP:port)
  - queue_depth / queue_cap (default 256)
  - pairs (subscribe 메시지로 필터 받은 통화쌍)
  - closed (true 면 곧 정리됨)
  - instance (어느 mci-edge-price)
- **instance_errors** — N6 에서 응답 못 한 instance 표시 (있을 때 빨강 banner)

#### 예시
```
1 연결 (필터 적용 후, 1 instance)
by_instance: [127.0.0.1:8083 : 1]
by_profile: [WEB.BRANCH.VIP : 1]

id=9  customer=cr  profile=WEB.BRANCH.VIP  remote=127.0.0.1:50742  queue 0/256  pairs=null  closed=false  instance=127.0.0.1:8083
```
정상. queue_depth 가 200+ 으로 올라가면 backpressure 진입 위험.

#### 주의
- 본 페이지의 표는 **edge-price 의 N6 fan-out 합산**. 한 인스턴스가 응답 못 해도 다른 인스턴스 결과는 보임.
- `queue_cap=256` 은 작은 값 — 부하 큰 클라이언트는 close 위험. mci-edge-price flag 로 늘릴 수 있음.
- ws 가 다른 페이지로 이동하면서 close 되면 row 가 1-2 초 안에 사라짐.

### 10.9 🔍 Customer 검색 (`customer-search`)

#### 무엇
customer_id 한 명에 대한 **cross-reference 진단** — 등록된 Profile / 활성 ws 연결 / 최근 매매 / quote_id 누적 한 화면.

#### endpoint
- `GET /v1/admin/price/customers/:customer_id` (mci-price)
- (페이지 내부에서) `/v1/admin/edge/connections?customer_id=...` 도 호출

#### 언제 보나
- 사용자 문의 — "내가 받는 호가가 이상하다"
- 사고 후 사용자별 영향 측정
- VIP 사용자 활동 모니터링

#### 화면 요소
- 상단 — customer_id 입력 + "▶ 검색"
- 결과 카드 묶음:
  - **등록 정보** — usid, profile_key, tier, registered_at
  - **활성 연결** — ws 연결 수, instance, queue 상태
  - **최근 시세 수신** — 마지막 ws message 시각, rate
  - **최근 매매 (N건)** — recenttx 의 사용자 필터 결과
  - **5-Layer quote 캐시** — 현재 캐시된 호가 (pair × bid/ask)

#### 예시
```
입력: crlee123@gmail.com

등록 정보:
  usid=crlee123@gmail.com  profile=WEB.BRANCH.VIP  tier=VIP
  registered_at=2026-06-13 13:50:21

활성 연결: 1
  instance=127.0.0.1:8083  remote=...  queue 0/256

최근 시세: 마지막 message 0.2s 전, rate 87/s

최근 매매 (3건):
  14:32:15  W_QUOTE_GET    200  12ms
  14:31:55  W_ORDER_NEW    200  35ms
  14:31:20  W_QUOTE_GET    200  10ms

5-Layer quote 캐시:
  USDKRW  bid=1380.41  ask=1381.29
  EURKRW  bid=1500.06  ask=1501.74
  ...
```

#### 주의
- customer 가 등록 안 되어 있으면 카드 모두 "(없음)" — usid 오타 확인.
- 5L quote 캐시는 mci-price 의 인메모리 — 재시작 시 비워짐.

### 10.10 📡 forwarder 통계 (`fwdstats`)

#### 무엇
quote-forwarder 의 **UDP FIX 수신/검증/publish 카운터**. 시세 파이프라인의 가장 첫 단계.

#### endpoint
- `GET /v1/admin/forwarder/stats` → quote-forwarder `/stats`
- admin 의 `--fwd-url` (default `http://127.0.0.1:9091`) 가 quote-forwarder metrics endpoint 와 일치해야

#### 언제 보나
- 시세가 끊겼을 때 — UDP 단계가 살아있는지
- invalid_quote 가 증가하면 — feed 의 schema 오류 가능성
- 부하 테스트 중 delivery / drop 측정

#### 화면 요소
- **feeds 표** — `label`, `addr` (UDP listen 주소), 인스턴스 정보
- **카운터 카드**:
  - received_total — UDP datagram 수
  - published_total — broker/gRPC 로 emit 한 envelope 수
  - publish_errors — emit 실패
  - queue_drop — reader → worker 채널 가득 차서 명시적 drop (kernel silent drop 회피)
  - uptime_sec
- **invalid_quotes breakdown** — `crossed_spread`, `missing_symbol`, `non_positive_price`, `not_a_quote`, `other`
- broker 연결 정보 (publish-mode 별)

#### 예시
```
feeds:
  SMB @ 127.0.0.1:30044

received_total       264,460
published_total      264,460   (delivery 100%)
publish_errors            0
uptime_sec              968

invalid_quotes: 0
broker: 127.0.0.1:11217  (mode=grpc)
```
정상. invalid_quote 가 누적되면 feed schema 변경 의심.

#### 주의
- delivery (`published/received`) 가 95% 미만이면 broker/gRPC publish 가 느림 — batch_max / publish-mode 조정.
- `crossed_spread` 가 자주 보이면 feed 호가의 시계 동기 / pair 매핑 점검.
- DevMode 에서 UDP source 없으면 received/published 모두 0 — 정상 0-state.

### 10.11 ⚠️ Backpressure 이력 (`backpressure`)

#### 무엇
mci-price / mci-edge-price 가 **fan-out 채널 80% 도달** 시 누적한 WARN 이력 (N7).

#### endpoint
- `GET /v1/admin/price/backpressure` → mci-price `/v1/backpressure`
- mci-edge-price 도 같은 endpoint 제공, admin proxy 가 합산

#### 언제 보나
- 부하 spike 후 누가 가장 먼저 backpressure 에 들어갔는지
- 새 클라이언트 (예: 봇) 도입 후 queue 가 차는지 모니터링
- 사고 timeline 의 한 부분

#### 화면 요소
- **summary** — `total_warnings`, `history_cap` (보통 100), 최근 발생 시각
- **표** (시각 desc, 최근 N건):
  - ts (when)
  - service (`mci-price` / `mci-edge-price`)
  - kind (`grpc-quote-fanout` / `ws-client` / `subscribe-bar` ...)
  - sub_id / customer_id / profile (있을 때)
  - queue_depth / queue_cap (예: 205/256 = 80%)
- 같은 sub_id 가 짧은 시간에 반복되면 그룹화

#### 예시
```
total_warnings: 2   history_cap: 100   latest: 2026-06-13 14:32:25

ts                  service          kind         sub_id  customer        profile          depth/cap
14:32:25.784        mci-edge-price   ws-client    12      (없음)          (없음)           205/256
14:32:10.222        mci-edge-price   ws-client    11      crlee123        WEB.BRANCH.VIP   205/256
```
sub_id 11 (crlee123) 가 80% 도달 후 close — load-gen + 3 stream 부하가 클라이언트 처리 한계를 넘은 케이스.

#### 주의
- WARN 만 누적 — 실제 close 는 100% 도달 시. 본 페이지에서 80% 자주 보이면 임계까지 가까움 → queue_cap 늘리거나 부하 줄이기.
- DevMode 에서 mci-price 재시작 시 이력 초기화.

### 10.12 🔁 FX swap 잠금 통계 (`swaplock`)

#### 무엇
`POST /v1/quote/swap/lock` 의 **누적 카운터 + 단계별 부분실패 + revoke 결과**.

#### endpoint
- `GET /v1/admin/price/swap-stats` → mci-price `/v1/quote/swap/stats`
- mci-price `--enable-swap-lock` + Registry 의 SwapIndex 구현이 있어야 endpoint 활성 (S3-b)

#### 언제 보나
- swap 거래 도입 후 성공률 모니터링
- 부분실패 (near.Put / far.Put / swap_index.PutSwap) 단계별 위험도 판단
- revoke 결과로 stale quote_id 잔존 위험 평가

#### 화면 요소
- **상단 toggle** — 자동 새로고침 (2초)
- **카드 4종**:
  - **requests (누적)**
  - **successes (누적)** — requests ≠ successes 면 amber, 정상 일치 emerald
  - **부분실패 합** — `fail_near + fail_far + fail_swap_index` 와 비율 (정상 < 0.1%, 위험 ≥ 1%)
  - **revoke 결과** — `revoke_ok / revoke_fail` — fail > 0 이면 stale quote_id 잔존 위험 (빨강)
- **단계별 분석 표** (위험도 ↑ 순서):
  - near.Put 실패 — 낮음 (다음 단계 진행 안 함, revoke 불필요)
  - far.Put 실패 — 중간 (near 1개 revoke 시도)
  - swap_index.PutSwap 실패 — 높음 (near+far 둘 다 revoke 시도)
- endpoint 미응답 시 회색 안내 메시지

#### 예시
```
requests: 3   successes: 3   ● emerald (1:1 일치)
부분실패: 0 (0.00%)   slate (정상)
revoke: 0 ok / 0 fail   slate (정상)

단계별 분석:
  near.Put 실패         0  낮음   다음 단계 진행 안 함. revoke 불필요.
  far.Put 실패          0  중간   near 1개 revoke 시도. revoke_fail 증가 시 stale near 잔존.
  swap_index 실패       0  높음   near+far 둘 다 revoke 시도. revoke_fail 발생 시 두 leg 모두 stale 가능.
```

#### 주의
- **본 카운터는 mci-price 재시작 시 0 리셋.** 장기 추세는 Prometheus 의 `wtg_swap_lock_*` (S3 후속).
- `endpoint 미응답` 안내가 뜨면: mci-price `--enable-swap-lock=true` + Registry 의 SwapIndex 주입 + pricingStore + best + quoteIDGen/Reg 모두 활성 필요.
- DevMode 의 MemoryRegistry 는 SwapIndex 구현. Redis 1차 (S3-c 이전) 는 미구현 → endpoint 미등록.

---

## 11. 도움말 / 외부 도구

### 11.1 📖 운영 가이드 (`guide`)

#### 무엇
`docs/` 디렉토리의 운영 문서를 **admin UI 안에서 마크다운 렌더링** 으로 보기. 별도 파일 열지 않고 SOP 참조.

#### endpoint
- `fetch("docs/<filename>.md")` — 정적 파일 (admin 바이너리에 embed)

#### 언제 보나
- 신규 운영자가 절차 익힐 때
- 사고 대응 SOP 참조
- 정책 명세 인용

#### 화면 요소
- 좌측 — 문서 목록 (mci-architecture / operations / conventions / margin-policy / observability / push-monitoring / ...)
- 우측 — 마크다운 렌더링 + TOC + 인쇄 친화
- 검색 (Ctrl+F)

#### 예시
- "operations.md" 클릭 → 운영 SOP 전체 인라인
- 모든 페이지 헤더의 `자세한 명세는 docs/...` 링크가 본 페이지로 점프

#### 주의
- admin 바이너리 재빌드 시 doc 도 같이 embed. 운영 중 docs 만 갱신은 불가능 (재배포 필요).

### 11.2 🧰 운영 도구 (외부 링크) (`tools`)

#### 무엇
admin UI 가 직접 표시하지 않는 **외부 시스템으로 점프하는 링크 모음**. Prometheus 의 graph, Grafana dashboard, mci-chart, etcd-keeper 등.

#### endpoint
- 각 항목은 `window.open(url, "_blank")` — admin 이 proxy 안 함

#### 언제 보나
- 운영 모니터링 카드보다 더 자유로운 PromQL 시각화 필요
- Grafana dashboard 의 panel 직접 보기
- TimescaleDB 직접 query 필요
- alert manager 의 silence 설정

#### 화면 요소
- 그리드 카드 — 각 외부 도구의 아이콘 + 이름 + 설명 + 새 탭 링크
- 운영자가 사용하는 모든 외부 도구를 한 곳에 정리:

| 카드 | URL 예시 | 용도 |
|---|---|---|
| Prometheus graph | `:9095/graph?g0.expr=...` | PromQL 직접 실행 |
| Prometheus targets | `:9095/targets` | scrape 상태 |
| Grafana dashboards | `:3000/dashboards` | 시계열 시각화 |
| Grafana alerts | `:3000/alerting/list` | firing alert + silence |
| Alertmanager | `:9093/#/alerts` | silence 관리 |
| mci-chart UI | `:8086/` | TradingView 라이브 차트 |
| mci-edge-chart UI | `:8087/` | DMZ 경유 차트 |
| etcd-keeper | (있을 때) | etcd key 직접 조회 |
| Loki / Tempo | (있을 때) | 로그 / trace |

#### 예시
```
[Prometheus — graph]  http://localhost:9095/graph?g0.expr=wtg_cross_emits_total
[Grafana — dashboards] http://grafana.internal/dashboards
[mci-chart — TradingView] http://localhost:8086/
```

#### 주의
- URL 은 환경별로 다름. mci-admin 의 도움말 페이지에 환경별 URL 매핑 표 있음.
- 외부 도구는 admin 의 인증과 별개 — Grafana 등은 자체 SSO 또는 Basic auth.

### 11.3 외부 도구 (사이드바 직접 링크)

사이드바 7. 외부 도구 섹션의 두 링크 — admin SPA 의 페이지가 아니라 새 탭으로 외부 사이트 열림:

- 📈 **차트 (mci-chart)** — `http://localhost:8086/` (사내 직접). TradingView lightweight-charts UI. 라이브 봉 + historical.
- 📈 **차트 (mci-edge-chart)** — `http://localhost:8087/` (DMZ proxy). 인증/IP CIDR/rate-limit 적용. 외부 노출용.

운영 환경에선 URL 이 사내 도메인 (예: `chart.internal/`) 으로 매핑됨. mci-admin 의 `chartLinkURL()` 함수가 환경별 변환.

---

## 12. 운영 시나리오 모음

자주 발생하는 운영 상황별로 **어느 페이지 → 어느 페이지 → 어떤 결정** 의 흐름을 정리.

### 12.1 출근 점검 (5분)

1. **대시보드** — broker 연결 / 시세 rate / subscribers / connections 가 평소 범위인지
2. **운영 모니터링** — `5xx rate`, `broker disconnects`, `ratelimit denied` 모두 0 인지 + sparkline 에 어제 spike 없었는지
3. **감사 로그** — 어제 밤 사이 변경 (kill switch / pricing) 있었는지
4. **시세 통계 (mci-price)** — sub_drops / latency.negative_count 가 0 인지
5. **연결 (ws)** — 정상 ws 클라이언트 수 (평소 대비)

이 5단계가 모두 평소면 정상. 어느 하나라도 어긋나면 즉시 해당 페이지의 디테일로.

### 12.2 사고 발생 (즉시 거래 중단)

1. **운영 모니터링** — 어디가 spike 인지 찾기
2. **정책 엔진** → Kill switch ON (전체 또는 특정 채널)
3. **감사 로그** — 사고 시작 시각 전후 변경 이력
4. **매매 감사 (최근 N건)** — 마지막으로 들어온 transaction 들
5. **Backpressure 이력** — backpressure 시작 시각이 사고와 일치하는지
6. 사고 원인 분석 후 Kill switch OFF + 정비 메시지

### 12.3 PricingTable 변경 (마진 정책 갱신)

1. **마진 정책 명세** — SOP 다시 읽기
2. **마진 계산기** — 변경 전 임의 시점 시뮬레이션 (현재 마진 확인)
3. **마진 테이블** — 변경 입력 (저장 전)
4. **마진 변경 미리보기** — 영향 customer 수 / 평균 Δspread 확인
5. **마진 테이블** → 저장 (immediate hot reload)
6. **마진 재계산** — 5-Layer customer quote 캐시 갱신 트리거
7. **시세 (라이브 ws)** — 호가가 새 값으로 흐르는지 시각 확인
8. **감사 로그** — 변경이 정상 기록됐는지

### 12.4 신규 매매 alias 도입

1. **서비스 명세** — broker AP 의 새 transaction schema 확인
2. **라우팅 룰** → alias 추가 (alias = `WECHO_NEW`, exchange = `ECHOSVC`, routing_key = `NEW_V2`)
3. **API 테스터** — 새 alias 로 손 호출 → 응답 확인
4. **매매 통계 (alias × tier)** — 실제 사용자 트래픽 들어오는지
5. **운영 모니터링** — `5xx` spike 없는지 30분 관찰
6. **감사 로그** — 변경 기록 확인

### 12.5 사용자 보고 디버깅 ("내 호가가 이상하다")

1. **Customer 검색** — usid 로 등록 정보 / 활성 연결 / 최근 매매 확인
2. **사용자 프로파일** — Profile 매핑 정상인지 (예상치 못한 tier 인지)
3. **마진 계산기** — usid 의 Profile/Customer 로 현재 호가가 어떻게 나와야 하는지 계산
4. **QuoteID 조회** — 사용자가 보고한 quote_id 가 있다면 lookup → 실제 발급된 호가 확인
5. **연결 (ws)** — 사용자의 ws 가 살아있는지, queue_depth 이상한지
6. 결론: 사용자 화면이 stale 인지, 마진 정책 오해인지, 실제 정책 버그인지 판별

### 12.6 신규 운영자 인수인계 (1일)

1. **운영 가이드** — operations / mci-architecture / margin-policy 정독
2. **DevMode 로 admin 띄움** — `./build/bin/mci-admin --dev --no-broker --listen :9090`
3. **시세 통계 / 시세 (라이브 ws)** — 시세 파이프라인 그림 잡기
4. **마진 계산기** — 5-Layer 가 어떻게 결합되는지 손으로 시뮬레이션
5. **API 테스터** — 안전한 transaction (예: WECHO_PING) 한 번 호출
6. 위의 12.1~12.5 시나리오를 dev 환경에서 한 번씩 따라하기

### 12.7 운영 사고 사후 (postmortem)

1. **감사 로그** — 사고 직전 1시간 변경
2. **매매 감사 (최근 N건)** — 사고 시작 시각의 transaction
3. **Backpressure 이력** — 부하 spike 가 사고와 일치했는지
4. **QuoteID 조회 / Validate 통계** — 검증 단계 reject 가 늘었는지
5. **운영 모니터링** sparkline — 어떤 카드가 spike 였는지
6. 위 모든 trace_id 모아 Tempo / Jaeger 에서 분산 trace 추적

---

## 부록 A. DevMode 빠른 기동

본 매뉴얼의 모든 예시는 다음 DevMode 스택 기준:

```bash
# 1. mci-admin (etcd 자동 embedded)
./build/bin/mci-admin --dev --no-broker --listen :9090 \
                     --prom-url http://127.0.0.1:9095 &

# 2. mci-price (gRPC 활성 + swap-lock 활성 + 정적 카탈로그)
./build/bin/mci-price --dev --no-broker --listen :8082 --grpc :50051 \
                     --enable-swap-lock \
                     --symbols etc/symbols.json \
                     --pricing etc/pricing.json \
                     --profiles etc/profiles.json &

# 3. mci-edge-price (3 stream 활성)
./build/bin/mci-edge-price --dev --listen :8083 --upstream 127.0.0.1:50051 \
                          --quote-stream --customer-stream &

# 4. (선택) quote-forwarder (broker 우회 gRPC publish)
./build/bin/quote-forwarder --listen 127.0.0.1:30044 \
                           --metrics 127.0.0.1:9091 \
                           --publish-mode grpc --price-grpc 127.0.0.1:50051 &

# 5. (선택) Prometheus (운영 모니터링 페이지용)
/opt/homebrew/bin/prometheus --config.file=logs/prometheus.yml \
                            --web.listen-address=127.0.0.1:9095 &

# 6. (선택) mci-chart (TimescaleDB 또는 plain Postgres)
./build/bin/mci-chart --listen :8086 --upstream 127.0.0.1:50051 \
                     --dsn postgres://localhost/wtg?sslmode=disable &

# 7. (선택) dev tick generator — 6 pair × 5/s
python3 /tmp/wtg-dev-tickloop.py &
```

상태 확인:
```bash
/tmp/wtg-dev-status.sh                  # 1회 snapshot
watch -tcn 2 /tmp/wtg-dev-status.sh     # 2초마다 갱신
```

브라우저 — `http://127.0.0.1:9090/` → DevMode 로그인 → ID `cr` 등 → "ID 만으로 입장".

## 부록 B. 자주 보는 빈 화면 / 0 카운터 원인

| 페이지 | 0 또는 빈 화면 | 원인 |
|---|---|---|
| 운영 모니터링 | 모든 카드 ERR + banner | mci-admin 에 `--prom-url` 미설정 |
| forwarder 통계 | received=0 published=0 | UDP source (load-gen 등) 미기동 |
| 차트 통계 | rows=0 hub.count=0 | mci-chart 미기동 또는 DB 없음 |
| 시세 통계 | received=0 | mci-price 가 broker/UDP/DevTick 어느 source 도 못 받음 |
| 연결 (ws) | count=0 | 브라우저 시세 페이지에서 ▶ 연결 안 누름 |
| 구독자 (gRPC) | 빈 표 | mci-edge-price / mci-chart 미기동 |
| swap 잠금 통계 | requests=0 | mci-price 재시작 직후 — 정상 0-state |
| swap 잠금 통계 | endpoint 미응답 안내 | mci-price `--enable-swap-lock=false` 또는 deps 부족 |

---

> 본 매뉴얼은 라이브 문서다. 새 페이지 / endpoint 가 추가되면 본 파일에 추가. 형식 (6칸) 은 유지.
