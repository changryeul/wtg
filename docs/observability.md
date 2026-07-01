# 관측성 / 운영 진단 가이드

본 문서는 WTG 의 **운영 진단 및 관측 (observability)** 을 다룹니다. 운영자가
"지금 누가 시세를 받고 있나 / 누구에게 문제가 생겼나 / 어디부터 봐야 하나"
에 답할 수 있도록 필요한 도구 (log / metric / endpoint / UI) 를 한 곳에 정리.

`docs/operations.md` 가 **무엇을 띄우고 설정하는가** 라면, 본 문서는 **떠
있는 상태에서 무엇을 보는가** 입니다.

---

## 0. 전체 그림

```
                                        [브라우저 사용자 / 클라이언트]
                                                ↓ ws
                                       mci-edge-price :8083 (N개 인스턴스)
                                                ↓ gRPC stream
                                       mci-price :8082 / :50051
                                          ↑                ↑
                  quote-forwarder :9091 ──┘                └── mci-chart :8086
                       ↑
                  UDP feed (cooker)
```

각 컴포넌트별 가시화 도구:

| 컴포넌트 | 무엇 | 어떻게 보는가 |
|---------|------|--------------|
| **quote-forwarder** | UDP 시세 → broker/gRPC forward | `/stats` `/metrics` + admin UI 「📡 forwarder 통계」 |
| **mci-price** | 핵심 시세 엔진 + 4 gRPC stream fan-out | `/v1/*` + admin UI 「🔌 구독자」 |
| **mci-edge-price** | 외부 ws fan-out (N 인스턴스) | `/v1/connections` + admin UI 「🔗 연결」 |
| **end-to-end** | 특정 customer 추적 | admin UI 「🔍 Customer 검색」 |
| **backpressure** | 큐 80% WARN 누적 + 이력 | admin UI 「⚠️ Backpressure 이력」 |
| **BestConsumer dedup** | same-price / below-tick 필터 (§3.5) | `/v1/best-stats.dedup` + `wtg_best_*` metric + admin UI 「BEST 산정」 배지 |

---

## 1. 부팅 시 자동 가드 (Silent 함정 방지)

mci-price 가 부팅 직후 운영자가 모를 silent 함정을 알람으로 즉시 노출.

### 1.1. SymbolMap 비어있음

```json
{
  "level": "WARN",
  "msg": "SymbolMap 비어있음 — Aggregator 가 모든 tick silent drop, quote_bars INSERT 0건",
  "mode": "etcd watch",
  "조치": "fx-sync --table=pair --source-dir=./etc/db-mirror 또는 mci-admin UI 로 wtg/pair/ 시드"
}
```

**왜**: Aggregator.OnTick 의 `SymbolMap.Lookup` 가 실패 시 silent return → tick
은 들어오지만 봉이 0개 생성 → quote_bars INSERT 0건. 부팅 로그가 `Aggregator
활성 symbols:0` INFO 한 줄만 찍어 운영 alert path 에서 잡히지 않던 함정.

### 1.2. ProfileSource 비어있음

```
"ProfileSource 비어있음 — PricingConsumer 가 fan-out 대상 0, 모든 마진 적용 quote silent drop"
조치: etcdctl put 또는 mci-admin UI 로 wtg/price/profiles/ 시드
```

**왜**: PricingConsumer 가 profile 0 으로 fan-out → 모든 quote stream 흐름
정지. 같은 silent 위험도.

### 1.3. PricingTable 비어있음

```
"PricingTable 비어있음 — 모든 마진 0 으로 raw 가격 publish (고객 quote 와 시장 best 동일)"
조치: fx-sync --table=hq_margin,site_margin,swap 으로 시드 또는 mci-admin UI
```

**왜**: SwapPoint/HQMargin/SiteMargin 모두 0 — silent drop 은 아니지만 마진
미적용 quote 가 운영 사고. 고객 가격이 시장 best 와 동일해짐.

### 운영 alert 패턴

Prometheus alertmanager / log shipper 에서:

```
expr: log_match{level="WARN", msg=~".*비어있음 —.*"}
```

부팅 5초 내 한 줄로 잡히면 시드 누락. 정상 운영에선 절대 발생 안 함.

---

## 2. quote-forwarder 진단

### 2.1. invalid_quote metric (reject 분류)

```
quote_forwarder_invalid_quote_total{feed="EBS", reason="non_positive_price"} 0
quote_forwarder_invalid_quote_total{feed="KMB", reason="not_a_quote"}        44430
```

| reason | 의미 | 운영 alert |
|--------|-----|----------|
| `non_positive_price` | bid 또는 ask 가 음수/0 | **즉시 alert** — cooker drift / scale 오류 |
| `crossed_spread` | ask < bid | cooker 의 spread 계산 오류 |
| `missing_symbol` | 55= (Symbol) 누락 | FIX 메시지 깨짐 |
| `not_a_quote` | 35=X trade 등 (bid+ask 둘 다 0) | silent skip 의도 — alert 대상 X |
| `other` | 위 외 | 보호용 fallback |

### 2.2. /stats breakdown

```bash
curl http://forwarder:9091/stats | jq .invalid_quotes
# {
#   "non_positive_price": 0,
#   "crossed_spread": 0,
#   "missing_symbol": 0,
#   "not_a_quote": 44430,
#   "other": 0
# }
```

### 2.3. reject sample 로그

reject 발생 시 per (feed, reason) 1분당 1샘플 raw FIX 로깅:

```json
{
  "level": "WARN",
  "msg": "reject sample",
  "feed": "KMB",
  "reason": "non_positive_price",
  "sym": "USDJPY",
  "bid": -4.66,
  "ask": -4.62,
  "fix": "8=FIX.4.4|9=80|35=W|49=KMB|56=SUB|55=USDJPY|268=2|269=0|270=-4.66|271=1000000|269=1|270=-4.62|271=1500000|10=000|"
}
```

운영자는 raw FIX 보고 즉시:
- 어떤 cooker (49=KMB) 가
- 어떤 symbol (55=USDJPY) 을
- 어떤 가격 (270=-4.66) 으로 publish 했는지 진단

### 2.4. admin UI 「📡 forwarder 통계」

사이드바 「📊 통계/감사」 그룹.

- 4 카드: received / published / reject% / uptime
- invalid_quotes reason 별 표 (Δ 2s) — non_positive_price 빨강 / crossed_spread
  노랑 / not_a_quote 회색
- UDP feeds 표 (label + addr)

**reject% 색상**:
- 0~5% : 정상
- 5~10% : 노랑 (notice)
- 10%+ : 빨강 (alert)

cooker artifact (음수 가격 등) 발생 시 이 페이지에서 즉시 인지.

---

## 3. mci-price 진단

### 3.1. `/v1/subscribers` — 4 카테고리 stream 카탈로그

```bash
curl http://mci-price:8082/v1/subscribers
```

| 카테고리 | 내용 | 누가 받나 |
|---------|------|----------|
| `tick_subscribers` | raw tick (마진 미적용 BEST) | mci-edge-price |
| `quote_subscribers` | Profile 별 마진 적용 quote | edge-price (profile_keys 명시 시) |
| `bar_subscribers` | 1s/1m/5m/15m/1h/1d 봉 | mci-chart |
| `customer_quote_subscribers` | customer 별 (Phase 4c) | edge-price (`--customer-stream`) |

각 entry: `{id, srv_id, symbols/profiles/pairs/timeframes, queue_depth, queue_cap}`

`queue_depth/queue_cap` 이 backpressure 신호. 80% 이상이면 자동 WARN (§5).

### 3.2. `/v1/customers` — CustomerRegistry digest

```bash
curl http://mci-price:8082/v1/customers
# { count: 1234, by_profile: {"WEB.BRANCH.VIP": 230, ...} }

curl 'http://mci-price:8082/v1/customers?include_sample=true&limit=100'
# 위 + sample: [{CustomerID, Profile}, ...]
```

### 3.3. `/v1/customers/{customerID}` — 단일 lookup

```bash
curl http://mci-price:8082/v1/customers/crlee123@gmail.com
# { customer_id, profile, profile_key, registered: true }
# 미등록 시 404 + {error: "not_registered"}
```

### 3.4. admin UI 「🔌 구독자 (gRPC)」

**중요**: 이 페이지의 id 들은 **internal service** 만 (mci-edge-price, mci-chart).
사용자(고객) 가 절대 보이지 않음. 사용자는 「🔗 연결 (ws)」 페이지.

큐 색상:
- 정상: 슬레이트
- 50%+: 노랑 (notice)
- 80%+: 빨강 (alert) — 자동 WARN 로그 동시 발생

### 3.5. `/v1/best-stats` — BestConsumer + dedup 필터

BestConsumer 는 raw 다중시장 tick 을 받아 `max(bid)` / `min(ask)` 로 합성 BEST tick
을 downstream (Aggregator, PricingConsumer, gRPC) 으로 fan-out. `--dedup-same-price`
로 활성 시 emit 전 이전 값과 비교해 same-price / below-tick 인 tick 을 필터해
downstream 부하를 낮춘다.

**flag** (mci-price):

```
--dedup-same-price                    # 필터 활성 (default off — 관측 우선)
--dedup-tick-size-multiplier=1.0      # 0=exact-match 만, 1.0=1 tick 미만 skip
```

tick_size 는 심볼 마지막 3자 (quote currency) 로 추정 — JPY/KRW/CNY/TWD/IDR/VND=0.01,
그 외=0.0001 (FX dealer convention). 정확한 값은 심볼 카탈로그 확장 (`SymbolEntry.TickSize`)
후속 검토.

**응답 (`dedup` 섹션 발췌)**:

```json
{
  "dedup": {
    "enabled": true,
    "tick_size_multiplier": 1.0,
    "emitted": 83,
    "dropped_same_price": 0,
    "dropped_below_tick": 19
  },
  "symbols": { ... }
}
```

drop rate = `(dropped_same + dropped_below) / (emitted + dropped_same + dropped_below)`.

**Prometheus metric** (P6Metrics, mci-price `/metrics`):

| metric | 의미 |
|---|---|
| `wtg_best_emitted_total` | downstream 으로 fan-out 된 tick 누적 |
| `wtg_best_dedup_dropped_same_price_total` | 이전 emit 과 완전 동일 값이라 skip |
| `wtg_best_dedup_dropped_below_tick_total` | 변화가 tick_size × multiplier 미만이라 skip |
| `wtg_best_rejected_quotes_total` | invariant 위반 (bid<=0 / ask<=0 / bid>ask) reject |
| `wtg_best_dedup_enabled` | 1=on, 0=off — dashboard 필터 편의 |

**Grafana panel query 예시**:

```promql
# drop rate (%) — quiet market 이면 급등
100 * (
  rate(wtg_best_dedup_dropped_same_price_total[1m])
  + rate(wtg_best_dedup_dropped_below_tick_total[1m])
) / (
  rate(wtg_best_emitted_total[1m])
  + rate(wtg_best_dedup_dropped_same_price_total[1m])
  + rate(wtg_best_dedup_dropped_below_tick_total[1m])
)

# emit rate (초당) — downstream 실제 부하
rate(wtg_best_emitted_total[1m])

# raw reject rate — cooker/forwarder sanity
rate(wtg_best_rejected_quotes_total[5m])
```

**admin UI**: 「BEST 산정」 카드 헤더에 배지 + 카운터가 실시간 표시. dedup off 이면
회색 "dedup OFF" 배지 + 활성 방법 안내. on 이면 초록 "dedup ON × <multiplier>" +
`emit / dropSame / dropBelow / dropRate` 한 줄.

**튜닝 지침**:

- 시작: default off 로 부팅 → dashboard 관측 → 정상 시장 시간대에도 drop rate
  10~30% 이상이면 활성해 볼 만.
- multiplier=1.0 (1 tick 미만 skip) 이 실무 안전. 0 (exact-match only) 은
  효과 작음.
- multiplier > 5 는 위험 — downstream 이 stale quote 위에서 결정할 수 있음.
- 활성 후 `wtg_price_delivery_latency*` (있으면) 또는 downstream (mci-edge-price)
  의 queue_depth 를 함께 관찰 — 낮아지면 dedup 이 유효.

---

## 4. mci-edge-price 진단 (다중 인스턴스)

### 4.1. `/v1/connections` (각 인스턴스)

```bash
curl http://edge-A:8083/v1/connections
curl http://edge-A:8083/v1/connections?customer_id=X
curl http://edge-A:8083/v1/connections?profile=WEB.BRANCH.VIP
```

각 connection: `{id, profile_key, customer_id, remote_addr, queue_depth/cap, pairs, closed}`

### 4.2. admin UI 「🔗 연결 (ws)」 — N 인스턴스 통합

admin 의 `--edge-urls` 에 등록된 모든 인스턴스를 병렬 호출 → 통합.

```json
{
  "count": 30000,
  "by_instance": {
    "edge-A:8083": 10000,
    "edge-B:8083": 10000,
    "edge-C:8083": 10000
  },
  "by_profile": {"WEB.BRANCH.VIP": 4500, ...},
  "connections": [
    {"instance": "edge-A:8083", "id": 1, "profile_key": "...", ...},
    ...
  ],
  "instance_errors": [
    {"instance": "edge-D:8083", "error": "connection refused"}  ← instance down
  ]
}
```

표에 `instance` 칼럼 — 그 사용자가 어느 edge 에 붙어있나 한 눈에.

`instance_errors` 가 비어있지 않으면 빨간 경고 박스로 표시 (instance down).

### 4.3. config — `--edge-urls`

```bash
# 단일
mci-admin --edge-urls=http://edge-A:8083
# 다중 (콤마)
mci-admin --edge-urls=http://edge-seoul-01:8083,http://edge-seoul-02:8083,http://edge-tokyo-01:8083
# 호환 — --edge-url (단수, 옛 스크립트 호환)
mci-admin --edge-url=http://edge-A:8083
```

### 4.4. Phase 4c (`--customer-stream`)

mci-edge-price 가 `--customer-stream` 옵션 없이 떠있으면 `customer_id` 가
빈값. customer 별 마진 (CustomerRule) 적용도 비활성. 「🔍 Customer 검색」 도
무의미해짐.

운영은 항상 `--customer-stream` 활성 권장.

---

## 5. Backpressure 자동 alert

### 5.1. checkBackpressure 의 동작

5개 fan-out path (mci-price 4 + edge-price 1) 가 매 enqueue 직후 큐 점유율
검사. 80% 이상이면 1분당 1회 / (sub_id, kind) 키로 WARN.

```json
{
  "level": "WARN",
  "msg": "backpressure 감지 — 큐 80% 도달",
  "sub_id": 42,
  "srv_id": "edge-seoul-02",   ← mci-price 측
  "kind": "quote",
  "queue_depth": 820,
  "queue_cap": 1024
}
```

`kind`:
- `tick` / `quote` / `bar` / `customer_quote` (mci-price)
- `ws` (mci-edge-price, label 은 `profile` 사용)

### 5.2. `/v1/backpressure` — 누적 + 최근 이력

```bash
curl http://mci-price:8082/v1/backpressure
# {
#   total_warnings: 42,
#   history_cap: 100,
#   recent: [{ts, sub_id, srv_id, kind, queue_depth, queue_cap}, ...]
# }

curl http://edge:8083/v1/backpressure
# 동일 형태 (srv_id 대신 profile_key)
```

### 5.3. admin UI 「⚠️ Backpressure 이력」

- 4 카드: mci-price 누적 / edge 누적 / 전체 정상 여부 / ring cap
- mci-price 최근 N건 표 (시각/kind/sub_id/srv_id/queue)
- mci-edge-price 최근 N건 표 (다중 instance 통합, **instance** 칼럼 포함)

**운영 흐름**:

```
1. backpressure WARN 로그 (또는 slow consumer 격리 로그) 발생
2. 「⚠️ Backpressure 이력」 → 직전 누가 정체됐는지 사후 추적
3. 「🔌 구독자」/「🔗 연결」 → 현재 상태 cross-check
4. 패턴 보고 capacity 증설 / 클라이언트 수정
```

---

## 6. 시나리오별 답하는 페이지

| 운영자 질문 | 어디서 답을 얻나 |
|------------|----------------|
| "지금 사용자 몇 명?" | UI 「🔗 연결」 의 count |
| "VIP 등급 몇 명?" | UI 「🔗 연결」 의 by_profile |
| "사용자가 어느 edge 에 붙어있나?" | UI 「🔗 연결」 의 instance 칼럼 |
| "edge-D 가 죽었나?" | UI 「🔗 연결」 의 `instance_errors` 박스 / `/v1/admin/edge/ping` |
| "edge-A 가 mci-price 와 stream 연결됐나?" | UI 「🔌 구독자」 의 Tick/Quote 카테고리에 srv_id 매칭 행 |
| "차트가 안 갱신된다" | UI 「🔌 구독자」 의 Bar 카테고리에 chart-X 행 |
| "고객 X 가 시세 못 받는다" | UI 「🔍 Customer 검색」 — 등록 + 연결 동시 |
| "어느 사용자가 slow?" | UI 「🔌 구독자」/「🔗 연결」 의 queue 색상 |
| "최근 backpressure 누가 났나?" | UI 「⚠️ Backpressure 이력」 |
| "음수 가격이 들어오나?" | UI 「📡 forwarder 통계」 의 invalid_quotes 카드 |
| "어떤 cooker 가 깨졌나?" | forwarder log 의 `"msg":"reject sample"` 라인 (raw FIX 포함) |
| "운영 카탈로그 시드 누락?" | mci-price 부팅 로그의 `*비어있음*` WARN |

---

## 7. 알람 운영 (요약 표)

| 신호 | 위치 | 의미 |
|------|------|-----|
| `msg: "*비어있음 —*"` (level=WARN) | mci-price 부팅 로그 | 카탈로그 시드 누락 |
| `quote_forwarder_invalid_quote_total{reason="non_positive_price"} > 0` | Prometheus | cooker drift/scale |
| `msg: "reject sample"` | forwarder 로그 | raw FIX 진단 sample |
| `msg: "backpressure 감지"` | mci-price / edge-price 로그 | 큐 80% 도달 |
| `msg: "slow * 격리"` (subscriber/quote/customer-quote) | mci-price 로그 | drop 발생 + stream 격리 |
| `msg: "slow consumer 격리"` (ws) | edge-price 로그 | ws send queue full |
| `instance_errors` 비어있지 않음 (admin UI 「🔗 연결」) | UI | edge instance down |
| `wtg_best_rejected_quotes_total > 0` | Prometheus | cooker/forwarder 가 invariant 위반 quote 발행 (bid<=0/ask<=0/bid>ask) — 데이터 sanity 점검 |
| `wtg_best_dedup_enabled=1` + drop rate 지속 0 | Prometheus / admin UI | 활성이지만 효과 없음 — tick_size 튜닝 여지 또는 tickloop stale |
| `wtg_best_dedup_enabled=1` + drop rate > 80% | Prometheus / admin UI | quiet market 또는 multiplier 너무 큼 — stale quote 위험, downstream 관측 (§3.5 튜닝 지침) |

---

## 8. 관련 코드 위치

| 파일 | 무엇 |
|------|------|
| `cmd/mci-price/main.go:340~` | SymbolMap 비어있음 가드 |
| `cmd/mci-price/main.go:370~` | ProfileSource / PricingTable 비어있음 가드 |
| `cmd/quote-forwarder/main.go` (`classifyInvalid`, `shouldLogRejectSample`) | invalid_quote 분류 + sample log |
| `internal/price/grpc.go` (`SubscribersSnapshot`, `checkBackpressure`, `SnapshotBackpressureStats`) | 진단 API + backpressure |
| `internal/price/customer_registry.go` (`Lookup`) | customer 단일 lookup |
| `internal/edge/price/registry.go` (`Snapshot`, `checkBackpressure`, `SnapshotBackpressureStats`) | edge ws 진단 + backpressure |
| `internal/admin/edge_proxy.go` (`EdgeConnectionsProxy`, `EdgeBackpressureProxy`, `EdgePingProxy`) | 다중 인스턴스 fan-out |
| `internal/admin/price_proxy.go` (`pricePathAllowlist`, `PriceCustomerLookupProxy`) | mci-price 통합 proxy |
| `internal/admin/forwarder_proxy.go` (`ForwarderStatsProxy`) | quote-forwarder /stats proxy |
| `internal/admin/ui/index.html` (page-subscribers / page-connections / page-customer-search / page-fwdstats / page-backpressure) | admin UI 5 신규 페이지 |
| `internal/price/best.go` (`DedupOptions`, `shouldDedupLocked`, `symbolQuoteTickSize`) | BestConsumer same-price / below-tick 필터 로직 |
| `internal/price/metrics_p6.go` (`P6MetricsOpts.Best`, `wtg_best_*` gauges) | dedup Prometheus 노출 |
| `internal/admin/ui/index.html` (`renderBestStats` 의 dedup 배지 + 카운터) | admin UI dedup 상태 실시간 |
