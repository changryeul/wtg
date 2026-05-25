# QuoteID — 운영 체크리스트 (active-active mci-price + Redis Registry)

이 문서는 [quoteid-validation-rfc.md](./quoteid-validation-rfc.md) §6 HA 토폴로지를
실 운영에 배포할 때 필요한 단계 / 모니터링 / 장애 대응을 정리한다.

## 배포 토폴로지

```
   ┌──────────────────────┐         ┌──────────────────────┐
   │  Redis Sentinel/      │         │  matching-engine     │
   │  Cluster (3+ nodes)   │◄────────┤  (gRPC client to     │
   │  prefix wtg:quoteid:* │         │   both mci-price A/B)│
   └──────────┬────────────┘         └────────┬─────────────┘
              │ Put / Get                     │ Validate RPC (round-robin)
   ┌──────────┴────────────┐                  │
   │                       │                  │
┌──┴──────────────┐    ┌──┴──────────────┐   │
│ mci-price (A)   │    │ mci-price (B)   │◄──┘
│ --quoteid-instance=A │ --quoteid-instance=B │
│ --grpc=:50051       │ --grpc=:50051        │
└──────────────────┘   └──────────────────┘
```

## mci-price 인스턴스별 설정

운영 권장 flag (각 인스턴스):

```bash
mci-price \
  --grpc=:50051 \
  --grpc-tls-cert=/etc/wtg/grpc.crt \
  --grpc-tls-key=/etc/wtg/grpc.key \
  --grpc-tls-client-ca=/etc/wtg/engine-ca.crt \      # mTLS — 매칭 엔진 CA
  \
  --quoteid-instance=A \                              # B 인스턴스는 "B"
  --quoteid-validity=500ms \                          # FX Global Code 권장 last-look 범위
  --quoteid-grace=1s \                                # validity 후 추가 보존
  --quoteid-reg-timeout=200ms \                       # Put p99 budget
  \
  --quoteid-redis=10.0.0.10:26379,10.0.0.11:26379,10.0.0.12:26379 \  # Sentinel addr 콤마구분 (v1.2+)
  --quoteid-redis-master=wtg-quoteid-master \         # Sentinel master 이름
  --quoteid-redis-pass=$REDIS_PASSWORD \
  --quoteid-redis-prefix=wtg:quoteid \
  --quoteid-redis-db=0 \
  \
  --etcd=... [기존 옵션]
```

또는 환경변수 — `WTG_PRICE_QUOTEID_INSTANCE`, `WTG_PRICE_QUOTEID_VALIDITY`,
`WTG_PRICE_QUOTEID_REDIS`, `WTG_PRICE_QUOTEID_REDIS_PASS`, ...

## Redis 셋업

### 옵션 1 — Sentinel (권장 — 단순)

```yaml
# sentinel.conf (3 sentinels, 1 master, 2 replicas)
sentinel monitor wtg-quoteid-master 10.0.0.10 6379 2
sentinel down-after-milliseconds wtg-quoteid-master 5000
sentinel failover-timeout wtg-quoteid-master 10000
sentinel parallel-syncs wtg-quoteid-master 1
```

`--quoteid-redis` 에 Sentinel 주소 콤마 구분 — v1.2 부터 자동으로
`redis.NewFailoverClient` 분기. master 이름은 `--quoteid-redis-master`
(default `wtg-quoteid-master`). master failover 시 client 가 자동
재연결, application 단 추가 처리 불필요.

단일 addr 만 주면 v1.0 처럼 직접 연결 — Sentinel 없는 dev / 1-node 운영
시나리오 호환.

### 옵션 2 — Cluster (대규모 / 멀티 DC)

slot 분산이 필요한 큐가 아니라서 v1 에서는 권장하지 않음. v2 (멀티 DC 배포)
에서 재검토.

### 데이터 영속성

QuoteID record 의 평균 lifetime 은 validity (500ms) + grace (1s) = **1.5초**.
Redis 가 죽었다 살아나도 모든 record 가 자동 만료되므로 AOF / RDB 부적합:

```redis
# Redis 설정 — 권장
save ""                    # RDB 비활성
appendonly no              # AOF 비활성
maxmemory 256mb            # QuoteID 메모리 한계 (record 당 ~300B × 1k qps × 1.5s ≈ 450KB)
maxmemory-policy allkeys-lru  # 메모리 부족 시 오래된 record 부터 evict (fail-safe)
```

## 매칭 엔진 client 설정

```go
// 두 mci-price 모두에 dial — gRPC round_robin resolver
conn, _ := grpc.NewClient(
    "wtg-price:50051",  // DNS 가 두 인스턴스 모두 resolve, 또는 manual list
    grpc.WithTransportCredentials(...),
    grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
)
client := wtgpb.NewQuoteValidationServiceClient(conn)

resp, err := client.Validate(ctx, &wtgpb.ValidateRequest{
    QuoteId:    order.QuoteID,
    EngineId:   "matching-engine-A",
    TsUnixNano: time.Now().UnixNano(),
})
// resp.Status 에 따라 OrdRejReason / 체결 분기.
```

## 모니터링

### 메트릭 (mci-price `/v1/stats` JSON)

| 키 | 의미 | 알림 임계 |
|---|------|-----------|
| `pricing_consumer.quote_register_errors` | Registry.Put 실패 | > 1% 이면 page |
| `quote_validation.total` | 검증 RPC 누적 | trend (drop = 엔진 연결 문제) |
| `quote_validation.ok` | OK 비율 | 평소 95%+ |
| `quote_validation.not_found` | 발행 안 됐거나 GC | > 5% 이면 clock skew / replication 검사 |
| `quote_validation.expired` | ValidUntil 도래 후 호출 | > 10% 이면 last-look 시간 정책 재검토 |
| `quote_validation.internal` | Redis 장애 등 | > 0 이면 즉시 알림 |

### 로그 (status != OK 만 INFO, 전체는 DEBUG — RFC §10.2)

```
{"level":"INFO","msg":"quote validation","quote_id":"A-mq4b-1f","engine_id":"matching-engine-A","status":"NOT_FOUND"}
```

### Redis 측

- `INFO replication` master_link_status 모니터링 (Sentinel)
- `INFO memory` used_memory (~maxmemory 70% 이내)
- `INFO stats` instantaneous_ops_per_sec (검증 TPS × 2 + 발행 TPS × Profile 수)

## 장애 시나리오

### 1) 한 mci-price down

- 엔진의 grpc round_robin 이 자동으로 살아있는 인스턴스에 라우팅.
- 죽은 인스턴스가 발행한 QuoteID 도 Redis 에 남아있어 살아있는 인스턴스가
  validate 가능 (instance prefix 무관하게 record lookup).
- 복구: 인스턴스 재시작 → 새 시퀀스 0 부터 발급 (인스턴스 prefix 가 같아도
  unix_ms 부분이 달라 ID 충돌 없음).

### 2) 두 mci-price 모두 down

- 엔진의 모든 Validate 호출이 transport error.
- **엔진 측 circuit breaker** 가 신규 주문 거절 — fail-safe (Principle 2).
- 복구: 최소 한 인스턴스 부팅 후 health check 통과 → 엔진 자동 재연결.

### 3) Redis master down → Sentinel failover

- 5초 이내 (down-after-milliseconds) failover 트리거.
- 새 master 가 promotion 되는 동안 (~10s) Put/Get 실패 가능.
- 이 시간 동안 발행되는 quote 는 `quote_register_errors` 증가, publish 자체는
  계속 (best-effort 감사) → 클라이언트는 quote 를 받지만 검증 시 NOT_FOUND
  → 자연스럽게 신규 주문 거절.
- 복구: failover 완료 후 Put 정상화. **이 시간대의 quote 는 영구 NOT_FOUND**
  → 해당 quote 로 주문 들어와도 거절됨 (fail-safe).

### 4) Clock skew (mci-price A ↔ B)

- A 가 발행한 record 의 ValidUntil 을 B 가 검증 — wallclock 시차 만큼 오차.
- NTP/PTP 동기화 필수. skew > validity/2 면 page.
- 모니터링: 엔진의 `Validate(ts_unix_nano)` ↔ mci-price 의 receive 시각 차이.

## 배포 / 롤백 순서

### 첫 배포

1. Redis Sentinel 셋업 — 별도 PR / 별도 변경 (v1 변경 없음).
2. mci-price 인스턴스 A 에 `--quoteid-instance=A --quoteid-redis=...` 추가
   → 단독 검증, 엔진은 아직 호출 안 함.
3. 인스턴스 B 동일 배포.
4. **canary** — 엔진 측 일부 트래픽 (예: 1% Tier=Standard) 만 QuoteID 검증
   활성. nack 률 모니터링.
5. 점진 확대 → 100%.

### 롤백

- 엔진 측에서 `Validate` 호출만 비활성화 (기존 흐름으로 회귀).
- mci-price 는 QuoteID 발급은 계속 (proto 필드는 omitempty 라 클라이언트
  호환).
- Redis 는 그대로 유지.

## v1.1 — MarkConsumed (commit 추가)

QuoteValidationService 에 `MarkConsumed` RPC 가 추가됨. 한 QuoteID 가 정확히
한 체결에만 묶이도록 atomic 보장 (FX Global Code Principle 17 "use only once").

### 엔진 흐름

```go
// 1) 검증 — read-only, 멱등
vr, _ := client.Validate(ctx, &ValidateRequest{QuoteId: q})
if vr.Status != OK { reject; return }

// 2) 자체 정책 (slippage / side / 한도)

// 3) 사용 표시 — atomic SET NX, race-safe
mc, _ := client.MarkConsumed(ctx, &MarkConsumedRequest{
    QuoteId:    q,
    ConsumerId: orderID,  // 예: 엔진의 OrderID — 감사 추적용
})
switch mc.Status {
case OK:               // 처음 — 체결 진행
case ALREADY_CONSUMED: // 다른 주문이 먼저 잡음 — ExecutionReport OrdRejReason=6
case NOT_FOUND:        // race 로 GC — OrdRejReason=5
case EXPIRED:          // last-look 도중 만료 — OrdRejReason=13
}
```

### 메트릭 추가 (`/v1/stats`)

| 키 | 의미 | 알림 임계 |
|---|------|-----------|
| `quote_validation.already_consumed` | Validate 가 본 conflict | trend (replay 시도 추적) |
| `quote_validation.consume_total` | MarkConsumed RPC 누적 | TPS 모니터링 |
| `quote_validation.consume_ok` | 정상 표시 | OK 비율 평소 ~100% |
| `quote_validation.consume_already` | race 충돌 | > 0.1% 이면 클라이언트 중복 주문 / 봇 의심 |
| `quote_validation.consume_not_found` | record GC race | trend |
| `quote_validation.consume_expired` | 만료 후 시도 | last-look 시간 정책 재검토 |

### Redis 키 모델

- `<prefix>:q:<quote_id>` — record JSON (Put). TTL = validity + grace.
- `<prefix>:c:<quote_id>` — consumer_id 문자열 (MarkConsumed, SET NX). TTL 동일.

두 키의 TTL 이 동일하므로 record GC 시 consumed 표시도 동반 만료 — race
는 자연스럽게 사라짐.

### SET NX 원자성 보장 범위

- Redis 단일 master / Sentinel: SET NX 가 fully atomic.
- Redis Cluster: 같은 hash tag 안의 키만 같은 slot — 우리 키 모델은 prefix
  공통이지만 hash tag 안 씀. v2 에서 `{quote_id}:q` / `{quote_id}:c` 로
  바꿔야 cluster 안전. (v1 은 Sentinel 권장.)

## v1.2 — Sentinel FailoverClient (commit 추가)

`--quoteid-redis` 가 단일 addr 이면 직접 연결, 콤마 구분 다중 addr 면
자동으로 `redis.NewFailoverClient` (Sentinel) 사용. master 이름은
`--quoteid-redis-master` (default `wtg-quoteid-master`).

### Sentinel failover 동작

- master node down → Sentinel quorum 이 5초 이내 (down-after-milliseconds)
  새 master promotion.
- redis-go client 가 Sentinel pub/sub 으로 master change 수신 → 자동 재연결.
- 이 과정 중 (10s 이내) Put / SetNX 가 timeout. PricingConsumer 의
  `quote_register_errors` 가 증가하지만 publish 자체는 best-effort 계속.
- 이 시간대 발행 quote 는 Registry 누락 → 검증 시 NOT_FOUND → 신규 주문
  자연 거절 (fail-safe).

### canary 시나리오 (운영팀 체크리스트)

1. 단일 mci-price 인스턴스에 새 `--quoteid-redis=sentinel1,sentinel2,sentinel3`
   + `--quoteid-redis-master=...` 적용 → 로그에 `redis_mode=sentinel` 확인.
2. Sentinel master 강제 failover (`SENTINEL FAILOVER <master>`) → mci-price
   가 자동 재연결, 로그에 reconnect 메시지.
3. failover 동안 발행 quote 의 Put 실패율 < 1% 확인.

## v1.3 — BatchValidate (commit 추가)

다건 QuoteID 를 단일 RPC 로 검증. 결과는 입력과 같은 순서의 배열.

### 엔진 흐름

```go
// FIX NewOrderList ('E') 같은 다건 주문의 사전 검증.
batchResp, _ := client.BatchValidate(ctx, &BatchValidateRequest{
    QuoteIds: []string{q1, q2, q3, ..., qN},   // 최대 1000
})
for i, r := range batchResp.Results {
    switch r.Status {
    case OK:                 // i-번째 주문 진행
    case ALREADY_CONSUMED:   // 거절 (OrdRejReason=6)
    case NOT_FOUND:          // 거절 (OrdRejReason=5)
    case EXPIRED:            // 거절 (OrdRejReason=13)
    case STATUS_UNSPECIFIED: // 일시 internal error, 재시도 권장
    }
}
```

### 성능

서버는 goroutine fan-out — N=100 batch 가 단일 Validate 와 비슷한 wallclock.
직렬 호출 대비 N× 개선 (Redis round-trip 병렬화).

상한 1000 (운영 abuse / goroutine 폭발 방어). 초과 시 InvalidArgument.

### 메트릭 추가

| 키 | 의미 |
|---|------|
| `quote_validation.batch_total` | BatchValidate RPC 누적 |
| `quote_validation.batch_items` | 처리된 quote_id 총합 (=총 RPC × 평균 batch 크기) |

## v1.4 — HTTP gateway (commit 추가)

mci-price 의 기존 HTTP 서버 (`--listen`) 에 자동으로 마운트 — 별도 binary
없음. gRPC 와 동일 핸들러 / 동일 카운터 / 동일 Registry 인스턴스. wire 만
JSON.

### 라우트

| 메서드 | 경로 | 의미 |
|--------|------|------|
| POST | `/v1/quoteid/validate` | 단건 검증 |
| POST | `/v1/quoteid/batch-validate` | 다건 병렬 검증 (≤1000) |
| POST | `/v1/quoteid/mark-consumed` | atomic 사용 표시 |
| GET | `/v1/quoteid/stats` | 누적 카운터 |

### wire 포맷

protojson — proto 정의된 필드명의 camelCase. 예시:

```bash
curl -X POST http://localhost:8082/v1/quoteid/validate \
  -H 'Content-Type: application/json' \
  -d '{"quoteId":"A-mq4b3z-1f","engineId":"matching-engine-A"}'
# {"status":"OK","record":{"quoteId":"A-mq4b3z-1f","pair":"USD/KRW",
#   "channel":"WEB","site":"BRANCH","tier":"VIP","bid":1400.1,"ask":1400.15,...}}

curl -X POST http://localhost:8082/v1/quoteid/mark-consumed \
  -H 'Content-Type: application/json' \
  -d '{"quoteId":"A-mq4b3z-1f","consumerId":"order-12345"}'
# {"status":"OK","record":{...}}

curl http://localhost:8082/v1/quoteid/stats
# {"total":1234,"ok":1100,"not_found":50,...}
```

### 사용 시나리오

- **비-Go FIX gateway** — Quickfix-CPP / OnixS C++ / Java 엔진이 별도
  gRPC stub 생성 없이 직접 호출.
- **운영 도구** — curl / Postman / Insomnia 로 debug.
- **사후 감사** — bash script 로 N개 QuoteID 검증 후 csv 출력.

### 보안 / 운영

HTTP gateway 는 `--listen` 의 권한 모델을 그대로 따름 — 현재 plain HTTP.
운영에서 외부 노출 시 nginx / haproxy 등 reverse proxy + mTLS 권장. v2 에서
`--quoteid-http-cert/-key` 등 native TLS 옵션 검토.

본문 크기 상한 256KB — BatchValidate 1000건 ≈ 30KB 의 여유.

## v1.5 — BatchMarkConsumed (commit 추가)

BatchValidate 의 짝 — 다건 (quote_id, consumer_id) 쌍을 한 RPC 로 표시.
각 항목 atomic (per-key SET NX / mutex) — 일부 OK 일부 ALREADY_CONSUMED
혼재 가능. goroutine fan-out 으로 Redis round-trip 병렬화.

### 엔진 흐름 — FIX NewOrderList ('E') 처리

```go
// 1) 전체 batch 사전 검증
vr, _ := client.BatchValidate(ctx, &BatchValidateRequest{
    QuoteIds: orderQuoteIDs,
})
okIDs := pickOKItems(vr.Results)   // 엔진 자체 정책 통과 항목

// 2) 일괄 표시 — atomic per item
mc, _ := client.BatchMarkConsumed(ctx, &BatchMarkConsumedRequest{
    Items: makeConsumeItems(okIDs, orderIDs),
})
for i, r := range mc.Results {
    switch r.Status {
    case OK:               // i 번째 leg fill
    case ALREADY_CONSUMED: // race 충돌 — 거절
    case NOT_FOUND:        // race GC — 거절
    case EXPIRED:          // 너무 늦음 — 거절
    }
}
// 3) ExecutionReport 다건 송신
```

상한 1000, 초과 시 InvalidArgument. HTTP wire 도 동일.

### 메트릭 추가

| 키 | 의미 |
|---|------|
| `quote_validation.batch_consume_total` | BatchMarkConsumed RPC 누적 |
| `quote_validation.batch_consume_items` | 처리된 (quote_id, consumer_id) 항목 총합 |

### HTTP 라우트

`POST /v1/quoteid/batch-mark-consumed` — protojson body:

```json
{
  "items": [
    {"quoteId":"A-1","consumerId":"order-1"},
    {"quoteId":"A-2","consumerId":"order-2"}
  ],
  "engineId":"matching-engine-A"
}
```

## v1.6 — Redis Cluster hash tag (commit 추가)

키 형식 변경:
```
이전 (v1.0–v1.5)    : <prefix>:q:<id>   / <prefix>:c:<id>
v1.6+               : <prefix>:{<id>}:q / <prefix>:{<id>}:c
```

`{<id>}` hash tag 안에 QuoteID 가 들어가 Redis Cluster 의 slot 계산이
QuoteID 만 기준으로 됨 → 두 키가 same slot. Lua script / pipelining 으로
multi-key atomic 가능 (v2 후속).

Standalone / Sentinel 에서는 slot 라우팅 자체가 없으므로 동작 변화 없음.

### 마이그레이션

QuoteID 의 lifetime 이 ~1.5초 (validity 500ms + grace 1s) — 배포 시점에
새 발급 ID 는 새 키 형식, 옛 ID 는 TTL 로 자연 소멸. **별도 migration
스크립트 불필요.**

### Cluster 모드 추가

`--quoteid-redis-mode` flag 신설 — `direct` / `sentinel` / `cluster` 명시.
빈값이면 auto: addr 1개 → direct, 2+ → sentinel.

```bash
mci-price \
  --quoteid-redis-mode=cluster \
  --quoteid-redis=10.0.0.1:6379,10.0.0.2:6379,10.0.0.3:6379,... \
  --quoteid-redis-pass=$PW \
  --quoteid-instance=A
```

`redis.ClusterClient` 가 자동 MOVED 처리 + slot 재계산. master 노드 down
시 ASK / cluster failover 절차에 따라 자동 복구.

### Sentinel vs Cluster 선택 가이드

| 항목 | Sentinel | Cluster |
|------|----------|---------|
| 토폴로지 | 1 master + N replica | 16384 slot 샤딩 |
| HA | master failover (자동) | slot reassignment (자동) |
| 처리량 | 단일 master 의 한계 | 노드 수에 따라 확장 |
| 운영 복잡도 | 낮음 | 높음 |
| 권장 규모 | < 50k QPS | 50k+ QPS |

v1 트래픽 (~1-10k QPS) 에서는 Sentinel 권장. Cluster 는 멀티 DC / 대형
은행 백본 통합 시.

## v1.7 — Lua script multi-key atomic (commit 추가)

`MarkConsumed` 가 단일 round-trip + 진정한 atomic 으로 동작.

### 이전 (v1.5–v1.6) — 2 RTT 의 race window

```
[Client]                [Redis]
   |  GET q:<id>          |   → record
   |←─────────────────────|     (ValidUntil 미래)
   |                      |     ⚠️ 이 사이에 record TTL 만료 가능
   |  SET NX c:<id>       |   → OK (consumed!)
   |←─────────────────────|     하지만 ValidUntil 은 이미 과거
```

이론적 race 였지만 cap 가 grace-edge 직전 호출 + Redis 메모리 압박에서
재현 가능. consumed=true 인데 record EXPIRED 인 정합성 깨짐 상태 발생.

### v1.7+ — Lua 단일 EVAL

```lua
-- KEYS[1]=q, KEYS[2]=c, ARGV[1]=consumer, ARGV[2]=now_nano, ARGV[3]=ttl_s
local rec_json = redis.call("GET", KEYS[1])
if not rec_json then return {3} end                -- ConsumeNotFound
local rec = cjson.decode(rec_json)
if now >= rec.valid_until_unix_nano then
  return {4, rec_json}                              -- ConsumeExpired
end
local ok = redis.call("SET", KEYS[2], consumer, "EX", ttl, "NX")
if ok then return {1, rec_json} end                 -- ConsumeOK
return {2, rec_json, redis.call("GET", KEYS[2]))    -- ConsumeAlreadyDone
```

Redis 단일 스레드 → Lua 실행 중 다른 명령 인터리브 없음 → 진정한 atomic.
모든 키가 hash tag `{<id>}` 안 (v1.6) 이라 Cluster slot 동일 — EVAL 가능.

### 성능

- 1 RTT 대신 2 RTT 절약 → p99 ~1ms 단축
- EVALSHA 캐시는 redis-go 가 자동 — 첫 호출 후 script SHA1 로 호출
- ClusterClient + EVAL: same-slot 보장이라 redirect 없음

### 검증

`TestRedisRegistry_MarkConsumed_LuaConcurrent` — 32 goroutine 이 동시에
같은 QuoteID 에 MarkConsumed → 정확히 1 OK + 31 AlreadyDone.

## v1.8 — HTTP native TLS (commit 추가)

mci-price 의 `--listen` 이 native TLS termination 가능. reverse proxy
(nginx/haproxy) 없이 mTLS 까지 직접.

### 옵션

```bash
mci-price \
  --listen=:8443 \
  --http-tls-cert=/etc/wtg/http.crt \      # 서버 cert PEM
  --http-tls-key=/etc/wtg/http.key \       # 서버 key PEM
  --http-tls-client-ca=/etc/wtg/engine-ca.crt  # mTLS — 클라이언트 CA bundle
  # ... 나머지 기존 flag
```

ENV: `WTG_PRICE_HTTP_TLS_CERT`, `WTG_PRICE_HTTP_TLS_KEY`,
`WTG_PRICE_HTTP_TLS_CLIENT_CA`.

옵션 둘 다 비면 v1.7 까지와 동일하게 plain HTTP — back-compat.

### Hot reload

`pkg/tlsutil.Reloader` 가 cert 핫리로드 — Let's Encrypt / cert-manager 의
자동 회전 호환. SIGHUP 또는 파일 mtime watch (Reloader 옵션 내장). 무중단.

### gRPC TLS (`--grpc-tls-*`) 와 분리

- `--grpc-tls-*` — gRPC 포트의 PriceService / QuoteValidationService.
- `--http-tls-*` — HTTP 포트 (`--listen`) 의 stats / quoteid REST.

두 포트가 다른 인증서 / 다른 client CA 를 가질 수 있음 — gRPC 는 엔진
mTLS, HTTP 는 일반 운영 도구 (curl + bearer token 등).

### Curl 예제

```bash
curl --cacert /etc/wtg/wtg-ca.crt \
     --cert /etc/wtg/engine.crt --key /etc/wtg/engine.key \
     -X POST https://price.internal:8443/v1/quoteid/validate \
     -H 'Content-Type: application/json' \
     -d '{"quoteId":"A-mq4b-1f"}'
```

## v1.9 — engine_id allowlist (RBAC, commit 추가)

mTLS CN 만으로는 부족한 경우 (같은 cert 를 공유하지만 다른 매칭 엔진
인스턴스 / 환경별 권한 분리) 를 위한 추가 RBAC.

### 활성

```bash
mci-price --quoteid-engines=matching-A,matching-B,audit-batch
```

또는 ENV `WTG_PRICE_QUOTEID_ENGINES=matching-A,...`.

빈값 (default) 이면 RBAC 비활성 — 모든 caller 통과 (v1.0–v1.8 동작).

### 검사 대상

모든 RPC — `Validate` / `BatchValidate` / `MarkConsumed` /
`BatchMarkConsumed` — 가 `engine_id` 필드를 허용 목록과 대조. 미허용 시:

- gRPC: `codes.PermissionDenied` (FIX 호환 mTLS 거부와 같은 level)
- HTTP: `403 Forbidden`

빈 `engine_id` (= 미지정) 도 거부 — caller 가 반드시 자기 identity 를
명시해야 한다.

### 메트릭

- `quote_validation.denied_engine` — 누적 거절 카운터. 알림 임계:
  > 0 이면 정책 위반 또는 잘못된 엔진 설정 알림. 0 으로 유지가 정상.

### 운영 시나리오

* **환경별 분리** — staging 의 `matching-staging`, prod 의 `matching-A` /
  `matching-B`. cert 만으로는 잘못된 env 의 엔진이 prod 에 호출 가능
  (인증서 mis-deploy 시). engine_id 가 추가 봉인.
* **운영 도구** — curl 로 디버깅할 때 별도 `engine_id` 부여 (`debug-cli`)
  → audit 로그에서 사람-호출 vs 엔진-호출 구분.
* **점진 출시** — canary 엔진 (`matching-A-canary`) 만 처음 허용, 안정
  되면 전체 엔진 추가.

## v1.10 — BatchMarkConsumed Pipeline 통합 (commit 추가)

기존 (v1.5–v1.9): BatchMarkConsumed 가 N goroutine fan-out — 각 goroutine
이 단일 EvalSha. Pool churn + goroutine overhead + N 개별 connection
grab.

v1.10+: Registry.MarkConsumedMany 가 직접 처리 — Memory 는 단일 mutex 안에
일관 snapshot, Redis 는 `pipe.Exec()` 으로 N EvalSha 묶음 송신. 1
connection grab + 1 RTT (direct/sentinel) 또는 슬롯당 1 RTT (cluster).

### 성능 비교 (Redis direct/sentinel)

| Batch 크기 | v1.5 fan-out (N goroutine) | v1.10 pipeline |
|------------|----------------------------|----------------|
| 10  | ~3-5ms (pool 점유 경합) | ~2ms (1 RTT) |
| 100 | ~10-20ms              | ~3ms (1 RTT) |
| 1000 | ~50-100ms             | ~8-15ms (1 RTT, larger payload) |

Cluster 에서는 slot 별 자동 split — slot 수에 비례한 RTT, 여전히 N
개별보다 빠름.

### Per-item atomic 보장은 동일

각 항목이 markConsumedScript (v1.7 Lua) 를 통과 — same QuoteID 에 동시
호출 시 정확히 한 명만 ConsumeOK. Batch 자체는 *not transaction* — 일부
OK / 일부 AlreadyDone 혼재 가능 (FIX NewOrderList 의 일부 leg 만 race
잃은 경우).

### Registry 인터페이스 추가

```go
type Registry interface {
    ... 기존 ...
    MarkConsumedMany(ctx, reqs []ConsumeRequest) ([]ConsumeResult, error)
}

type ConsumeRequest struct {
    QuoteID    QuoteID
    ConsumerID string
}
```

호출자가 직접 사용 가능 — 외부 인덱서나 audit 도구가 pkg/quoteid 만
import 해도 batch 가능.

## v1.11 — Validate/BatchValidate atomic Lookup + Pipeline (commit 추가)

read path 의 race window 제거 + 성능 개선:

### Validate (단건)

이전 (v1.7–v1.10): `Get` + `Consumed` 두 개의 Redis GET — 그 사이 record
TTL 만료 + consumed marker 만 남는 짧은 race window (이론적이지만 grace
경계 + GC 압박 시 재현 가능).

v1.11+: 단일 `lookupScript` (Lua) 가 GET q + GET c + state 분류를 atomic
하게 처리. 1 RTT. Redis 단일 스레드 보장.

### BatchValidate (다건)

이전: N goroutine fan-out, 각자 2 RTT × N items.

v1.11+: `Registry.LookupMany` 가 pipeline 으로 N EVAL lookupScript 묶음
송신. 1 RTT (direct/sentinel) / slot 별 1 RTT (cluster).

### 성능

| Batch | v1.10 fan-out | v1.11 pipeline |
|-------|---------------|----------------|
| 단건 Validate | 2 RTT | 1 RTT |
| BatchValidate 10 | ~5-10ms | ~2ms |
| BatchValidate 100 | ~20-40ms | ~3ms |
| BatchValidate 1000 | ~100-200ms | ~10-20ms |

### Lookup 결과 (LookupResult)

```go
type LookupResult struct {
    Found      bool   // record 존재
    Record     Record // Found 일 때 채워짐
    Consumed   bool   // consumed marker 존재
    ConsumedBy string
}
```

`Found=false, Consumed=true` 도 가능 — record TTL 이 consumed marker
보다 먼저 만료된 짧은 윈도우. Validate 응답에서는 NOT_FOUND 매핑
(record 없으면 거래 불가).

### Pipeline + Script 주의사항

Redis go client v9 의 `Script.Run` 은 EVALSHA→EVAL fallback 이 동기. Pipeline
안에서는 NOSCRIPT 응답을 fallback 이전에 못 보므로, MarkConsumedMany /
LookupMany 둘 다 `Script.Eval` 직접 사용 — 매 호출 EVAL source 송신.
실측 차이는 작음 (Redis 측 EVAL 캐시).

## v1.12 — engine_id allowlist etcd hot reload (commit 추가)

v1.9 의 정적 allowlist 를 etcd watch 로 동적 갱신. 엔진 추가/제거 시
mci-price 재시작 불필요 — 실 운영에서 canary 출시 / 긴급 차단 즉시 반영.

### 활성

```bash
mci-price \
  --etcd=etcd1:2379,etcd2:2379,etcd3:2379 \
  --etcd-prefix=wtg/ \
  --quoteid-engines-etcd=wtg/quoteid/engines/ \
  ...
```

또는 ENV `WTG_PRICE_QUOTEID_ENGINES_ETCD=wtg/quoteid/engines/`.

빈값이면 etcd watch 비활성 — 정적 `--quoteid-engines` 만 사용.

### etcd 키 모델

```
wtg/quoteid/engines/matching-A    (value 무시 — presence = allow)
wtg/quoteid/engines/matching-B
wtg/quoteid/engines/audit-cli
```

운영자 추가/제거 (etcdctl 또는 mci-admin UI 가 미래):

```bash
# 엔진 추가 — 즉시 활성화
etcdctl put wtg/quoteid/engines/matching-C ""

# 엔진 차단 — 즉시 거부
etcdctl del wtg/quoteid/engines/matching-A

# 현재 active 목록 조회
etcdctl get wtg/quoteid/engines/ --prefix --keys-only
```

### 동작 흐름

1. 시작 시 prefix Get → 모든 engine_id 추출 → atomic.Pointer 갱신.
2. 백그라운드 watch goroutine 이 PUT/DELETE 이벤트 → set 갱신 → 콜백 →
   `QuoteValidationServer.SetEngineAllowlistMap` 호출.
3. 모든 RPC handler 의 `checkEngine` 이 `atomic.Pointer.Load` — lock 없음,
   hot path 영향 0.

watch 채널이 끊기면 자동 재등록 (compaction / 네트워크 끊김 대비).

### 정적 vs etcd

| | 정적 (`--quoteid-engines`) | etcd (`--quoteid-engines-etcd`) |
|--|---------------------------|--------------------------------|
| 갱신 | mci-price 재시작 | etcd PUT/DELETE 즉시 |
| 운영 도구 | systemd unit / chef-puppet | etcdctl, mci-admin |
| 의존성 | 없음 | etcd cluster |
| 추천 | dev / 단일 인스턴스 | 운영 multi-instance |

두 옵션 다 채워지면 etcd 가 정적을 덮어쓴다 (마지막 set 이긴다, 보통
etcd 의 초기 로드 결과).

## v1.13 — Pipeline latency metrics (commit 추가)

mci-price 의 `/metrics` (Prometheus) 에 quoteid 전용 4개 collector 추가.

### 새 메트릭

| Metric | Type | Labels | 의미 |
|--------|------|--------|------|
| `wtg_quoteid_op_total` | Counter | `service, op, status` | per-item 호출 (batch 의 N 항목은 N 건) |
| `wtg_quoteid_op_duration_seconds` | Histogram | `service, op` | 단일 RPC wallclock (validate / mark_consumed) |
| `wtg_quoteid_batch_size` | Histogram | `service, op` | batch 항목 수 분포 |
| `wtg_quoteid_batch_duration_seconds` | Histogram | `service, op` | batch 전체 wallclock |

`op` 값: `validate` / `batch_validate` / `mark_consumed` / `batch_mark_consumed`

`status` 값:
- Validate 계열: `ok` / `not_found` / `expired` / `already_consumed` / `denied` / `internal`
- MarkConsumed 계열: `consume_ok` / `consume_already` / `consume_not_found` / `consume_expired` / `denied` / `internal`

### Grafana 쿼리 예제

p99 BatchValidate latency:
```promql
histogram_quantile(0.99,
  rate(wtg_quoteid_batch_duration_seconds_bucket{op="batch_validate"}[5m]))
```

Batch 평균 크기:
```promql
rate(wtg_quoteid_batch_size_sum[5m]) / rate(wtg_quoteid_batch_size_count[5m])
```

RBAC 거절 비율 (alert 후보 — 평소 0 이어야):
```promql
sum(rate(wtg_quoteid_op_total{status="denied"}[5m]))
```

체결 표시 / 충돌 비율:
```promql
sum(rate(wtg_quoteid_op_total{status="consume_already"}[5m])) /
sum(rate(wtg_quoteid_op_total{op=~"mark_consumed|batch_mark_consumed"}[5m]))
```

높은 값이면 클라이언트 중복 주문 / 봇 의심 (v1.5 의 already_consumed 운영
임계 0.1% 와 동일 신호).

### Optional 비활성

`QuoteValidationServer.SetMetrics(nil)` 면 collector 호출 안 함 — 단위 테스트
경로 무시.

## v1.14 — Grafana dashboard JSON (commit 추가)

`etc/grafana/quoteid-dashboard.json` — 17 패널 / 23 PromQL 쿼리. v1.13 의
모든 메트릭을 한 화면에서. import 절차는 `etc/grafana/README.md` 참조.

구성:
* Overview row — 전체 RPS / OK rate / RBAC denied (alert) /
  ALREADY_CONSUMED ratio (yellow 0.1% red 1%).
* Latency row — 4 RPC × p50/p95/p99.
* Throughput row — Validate / MarkConsumed 각각 status 별 RPS.
* Batch row — 평균 batch size + 분포 heatmap.
* Errors row — RBAC denied / Internal + Consume conflict 추이.

Variable: `$service` (default mci-price, regex 다중 인스턴스 지원),
`$rate_window` (1m/5m/15m/1h).

## v1.15 — engine_id metadata (commit 추가)

v1.12 의 etcd allowlist value 가 단순 빈 문자열이었는데, v1.15 부터 JSON
으로 권한 / 만료 / contact 를 담는다. backward compat — 빈 value 는 풀
권한 / 무기한 (v1.12 동작 동일).

### Schema

```json
{
  "permissions": ["validate", "mark_consumed"],
  "expires_at":  "2026-12-31T00:00:00Z",
  "contact":     "trading-platform@bank.com"
}
```

- `permissions` — `validate` (read) / `mark_consumed` (write). 빈 슬라이스
  또는 미지정 = 풀 권한 (default).
- `expires_at` — RFC3339. 도래 후 자동 거절. 빈 문자열 = 무기한.
  잘못된 형식이면 fail-open (운영 안전성, 운영자가 발견 → etcd 수정).
- `contact` — free-form 식별자. 감사 / 운영 추적용. 검사 영향 없음.

### 운영 예

```bash
# audit-cli — read-only, 1년 만료.
etcdctl put wtg/quoteid/engines/audit-cli \
  '{"permissions":["validate"],"expires_at":"2027-05-25T00:00:00Z",
    "contact":"audit-team@bank.com"}'

# matching-A — 풀 권한 (default), 운영 contact.
etcdctl put wtg/quoteid/engines/matching-A \
  '{"contact":"trading-platform@bank.com"}'

# 임시 디버깅 — 1시간 만료.
etcdctl put wtg/quoteid/engines/debug-cli \
  '{"permissions":["validate"],"expires_at":"'"$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)"'"}'

# v1.12 호환 — value 비워도 OK (풀 권한, 무기한).
etcdctl put wtg/quoteid/engines/matching-B ""
```

### 거절 메시지 (gRPC PermissionDenied / HTTP 403)

`reject_text` 예:
- `engine_id not in allowlist`
- `engine_id expired`
- `engine_id lacks mark_consumed permission`

운영자가 Grafana / 로그에서 사유별 분류 가능.

### 운영 시나리오

- **권한 분리**: audit-cli 가 실수로 MarkConsumed 호출해도 차단.
- **자동 회수**: 임시 디버깅 토큰이 expires_at 후 자동 비활성 — 인간이
  잊어도 보안 회수됨.
- **감사 추적**: 사건 발생 시 etcd value 의 contact 로 즉시 운영자 식별.
- **비상 차단**: `etcdctl del wtg/quoteid/engines/matching-A` — 즉시 모든
  RPC 차단.

## v1.16 — Grafana Unified Alerting rules JSON (commit 추가)

`etc/grafana/quoteid-alerts.json` — 5 alert rules 그룹 1개.

| UID | severity | 조건 | for |
|-----|----------|------|-----|
| `wtg-quoteid-rbac-denied` | **page** | denied rate > 0.01/s | 1m |
| `wtg-quoteid-consume-already` | warn | already_consumed ratio > 0.1% | 5m |
| `wtg-quoteid-batch-latency` | warn | BatchValidate p99 > 50ms | 5m |
| `wtg-quoteid-internal` | **page** | internal rate > 0.001/s | 2m |
| `wtg-quoteid-register-errors` | warn | Registry.Put 실패율 > 0.01/s | 5m |

각 rule 은 PromQL 표현식 → Grafana threshold expression → `labels.severity`
(page / warn) 로 라우팅. PagerDuty / Slack contact point 매핑은
`etc/grafana/README.md` 참조.

Import 절차 (UI / provisioning) + 변수 치환 (`${DS_PROMETHEUS}`) 도 동일
문서에 정리.

## v1.17 — mci-admin UI engine_id 관리 페이지 (commit 추가)

v1.15 의 etcd allowlist 를 mci-admin UI 의 "QuoteID 엔진" 탭에서 직접
관리. etcdctl 대신 GUI — 운영자가 vi 없이 권한 / 만료 / contact 수정.

### Backend

`internal/admin/admin_quoteid_engines.go` — 다음 endpoint:

| Method | Path | 의미 |
|--------|------|------|
| GET | `/v1/admin/quoteid-engines` | 전체 목록 |
| GET | `/v1/admin/quoteid-engines/{engine_id}` | 단건 |
| PUT | `/v1/admin/quoteid-engines/{engine_id}` | 등록 / 수정 (EngineMeta JSON) |
| DELETE | `/v1/admin/quoteid-engines/{engine_id}` | 즉시 차단 |

PUT body 검증:
- `permissions[]` 의 각 토큰이 `validate` / `mark_consumed` 가 아니면 400.
- `expires_at` 이 RFC3339 가 아니면 400.
- 빈 body 도 허용 — 풀 권한 / 무기한 (v1.12 호환).

### UI

좌측 nav `QuoteID 엔진` 탭. 테이블:
- engine_id (mono)
- permissions ("(full)" 또는 콤마 구분)
- expires_at — 만료 임박 (7일 이내) 노란색, 만료 후 빨간색
- contact
- 수정 / 삭제 버튼

모달:
- engine_id (수정 시 disabled — etcd key 변경 금지)
- validate / mark_consumed 체크박스 (둘 다 끔 = 풀 권한)
- expires_at (RFC3339, placeholder 가이드)
- contact

삭제 confirm 메시지에 "mci-price 가 hot reload" 명시 — 운영자가 영향
인지.

### 새 config

```bash
mci-admin --etcd-quoteid-engines-prefix=wtg/quoteid/engines/
```
또는 `WTG_ADMIN_ETCD_QUOTEID_ENGINES_PREFIX`. mci-price 의
`--quoteid-engines-etcd` 와 동일 prefix 여야 hot reload 가 한 곳을 가리킨다.

## v1.18 — AsyncRegistry (Put 비동기 batch, commit 추가)

v1.17 까지는 `PricingConsumer.OnTick` 이 Profile 마다 동기 `Registry.Put` 호출 —
Redis RTT × N 프로파일이 hot path 를 블록. tick 처리량 한계가 Redis 응답속도에
직접 비례했음.

v1.18+ : `AsyncRegistry` wrapper 가 Put 을 채널로 큐잉 → 백그라운드 worker
goroutine 이 batch + `PutMany` (Redis Pipeline) 으로 송신. **읽기 path
(Lookup / MarkConsumed) 는 sync 유지** — 검증/체결 latency 보장.

### 활성

```bash
mci-price \
  --quoteid-redis=...        # Redis 모드 (Memory 는 의미 없음)
  --quoteid-async-queue=10000 \   # >0 → 비동기 활성. default 0 = 동기
  --quoteid-async-flush=5ms \     # batch 미만이어도 flush 주기
  --quoteid-async-batch=200 \     # batch 최대
  --quoteid-async-timeout=200ms   # 단일 PutMany ctx timeout
```

ENV: `WTG_PRICE_QUOTEID_ASYNC_{QUEUE,FLUSH,BATCH,TIMEOUT}`.

### 흐름

```
[OnTick (hot path)]
   ↓ Apply (margin) × N profiles
   ↓ attachQuoteID
   ↓ AsyncRegistry.Put(rec)        ← 즉시 반환 (채널 send only)
   ↓ Publisher.PublishQuote
       broker JSON
       gRPC SubscribeQuote

[worker goroutine (background)]
   ←─ channel ─┐
   ↓ 배치 누적  │
   ↓ flush trigger:
     - batch size ≥ BatchMax
     - 또는 FlushInterval 도래
   ↓ inner.PutMany(ctx, batch)     ← Redis Pipeline 1 RTT
   ↓ written / failed 카운터
```

### Trade-off

- **장점**: tick 처리량 (Redis RTT 와 무관 — 채널 send 만). 100 ticks/sec ×
  10 profiles = 1000 Put/sec 부하가 hot path 에서 사라짐.
- **단점**: queue 가득 시 **drop** (best-effort audit). At-least-once 보장
  안 함. drop 된 record 의 QuoteID 는 Registry 에 없어 검증 시 NOT_FOUND →
  자연스럽게 신규 주문 거절 (fail-safe).
- **shutdown**: SIGTERM 후 1초 안에 남은 batch flush — 시간 초과 시 손실.

### 메트릭

`AsyncRegistry.Stats()` 노출 (TODO: Prometheus collector 추가):

| 키 | 의미 |
|---|------|
| `enqueued` | 채널 send 성공 |
| `dropped` | queue full → drop (audit 누락) |
| `written` | worker 가 PutMany 성공 |
| `failed` | PutMany err (네트워크 / Redis 장애) |
| `queue_len` | 현재 채널 잔여 |
| `queue_cap` | 채널 버퍼 크기 |

운영 임계:
- `dropped` > 0 — queue 부족 → `--quoteid-async-queue` 증가 또는 Redis 응답속도 점검
- `failed` > 0 — Redis Sentinel failover / 네트워크 이상

### 메모리 모드는 의미 없음

`--quoteid-redis` 비어있으면 (Memory) RTT 없어 async 가 오히려 overhead.
main.go 가 Redis 활성일 때만 wrapper 적용 — Memory 면 wrapper 안 만듦.

## v1.19 — AsyncRegistry Prometheus 통합 (commit 추가)

v1.18 의 AsyncRegistry 가 `Stats()` 메서드로만 카운터를 노출했는데,
운영 모니터링 / 알림으로 못 잡혀 사실상 black box 였음. v1.19+ 는 4개
counter + 1 gauge 를 `/metrics` 에 발행.

### 새 메트릭

| Metric | Type | Labels | 의미 |
|--------|------|--------|------|
| `wtg_quoteid_async_enqueued_total` | Counter | service | 채널 send 성공 |
| `wtg_quoteid_async_dropped_total` | Counter | service | queue full → drop (audit 누락) |
| `wtg_quoteid_async_written_total` | Counter | service | worker PutMany 성공 record 수 |
| `wtg_quoteid_async_failed_total` | Counter | service | worker PutMany 실패 record 수 |
| `wtg_quoteid_async_queue_len` | Gauge | service | scrape 시점 채널 잔여 |

queue_len 은 `GaugeFunc` — scrape 마다 `len(channel)` cheap call.

### Grafana 쿼리 예제

drop 발생률 (alert 후보 — 평소 0 이어야):
```promql
rate(wtg_quoteid_async_dropped_total[5m])
```

queue 점유율 (capacity 대비):
```promql
wtg_quoteid_async_queue_len / 10000  # --quoteid-async-queue 와 같은 값
```

처리량 (written 와 enqueued 의 차이는 in-flight + dropped):
```promql
rate(wtg_quoteid_async_written_total[5m])
```

### Alert 후보 (operations.md 의 v1.16 패턴 따라)

```yaml
- title: AsyncRegistry drop 발생
  expr: rate(wtg_quoteid_async_dropped_total[5m]) > 0
  for: 1m
  severity: page    # audit 누락은 보안 / 컴플라이언스 즉시 알림

- title: AsyncRegistry queue 가득 임박
  expr: wtg_quoteid_async_queue_len > 0.8 * 10000  # 80%
  for: 5m
  severity: warn    # --quoteid-async-queue 증가 또는 Redis 점검

- title: AsyncRegistry PutMany 실패 비율
  expr: rate(wtg_quoteid_async_failed_total[5m]) /
        rate(wtg_quoteid_async_enqueued_total[5m]) > 0.001
  for: 5m
  severity: warn    # Redis Sentinel failover / 네트워크 이상
```

### Lifecycle

- main.go 의 `wireQuoteID` 가 `srv.Metrics()` 전달 → AsyncRegistry 생성 시
  Hook + GaugeFunc 등록.
- async 비활성 시 (--quoteid-async-queue=0) 이 코드는 실행 안 됨 — 메트릭
  시계열도 안 생김 (Prometheus rate query 가 빈 시리즈 처리).

## v1.20 — Grafana dashboard AsyncRegistry row (commit 추가)

v1.19 의 메트릭 + alert 위에 시각화 row 추가. `etc/grafana/quoteid-dashboard.json`
이 26 패널 / 32 PromQL 쿼리로 확장 (이전 19 / 23).

### 새 row 구성 (y=49~)

| 위치 | 패널 | 의도 |
|------|------|------|
| stat | Queue length (현재) | scrape 시점 backlog, 5k yellow / 8k red |
| stat | Dropped rate | audit 누락 alert (red if > 0.01/s) |
| stat | Written rate | worker 처리량 — 정상 시 enqueued 와 동일 |
| stat | Failed rate | yellow 0.001 / red 0.01 (Redis 이상) |
| ts   | Enqueue / Write / Drop / Fail throughput | 시계열 비교 — write rate 가 enqueue 와 lag 가 있으면 backlog 누적 신호 |
| ts   | Queue length over time | backlog 추세 + 임계 라인 (5k/8k) |

### 운영 해석

- **queue 가 일정 수준에서 멈춰 있음** — 정상 (incoming rate = drain rate).
- **queue 가 우상향** — drain 부족, 곧 saturation. `--quoteid-async-queue` 증가
  또는 Redis 응답 점검.
- **dropped 발생** — queue saturation 결과. audit 누락 알림 (PagerDuty).
  drop 된 quote 의 검증은 NOT_FOUND → 신규 주문 자연 거절 (fail-safe).
- **enqueued ↔ written 격차** — 짧으면 batch wait (FlushInterval), 지속되면
  Redis 응답 지연.

## v1.21 — C SDK (commit 추가)

매칭 엔진이 C 로 만들어졌으므로 gRPC-C++ (~수십 MB) 대신 HTTP REST +
libcurl 기반 thin client SDK. `etc/sdk/c/` 에 5 파일.

### 파일

- `quoteid_client.h` — 공용 C API (qid_client_t opaque + 4 RPC + 2 헬퍼).
- `quoteid_client.c` — 구현 (~430 라인, libcurl + cJSON).
- `example_fix_flow.c` — FIX NewOrderSingle handler 예제.
- `Makefile` — pkg-config 우선, fallback `-lcurl -lcjson`.
- `README.md` — OS 별 설치 / 빌드 / API / 스레드 모델 / TLS 권장.

### 의존성

| OS | 설치 |
|----|------|
| Ubuntu/Debian | `apt install libcurl4-openssl-dev libcjson-dev` |
| RHEL/CentOS | `yum install libcurl-devel cjson-devel` |
| macOS | `brew install curl cjson` |
| Alpine | `apk add curl-dev cjson-dev` |

운영 시스템 모두에 표준 패키지로 존재 — 추가 빌드 단계 불필요.

### API 요약

```c
qid_client_t* qid_client_new(const qid_client_options_t* opts);
void          qid_client_free(qid_client_t* c);

qid_err_t qid_validate     (c, quote_id, &result);
qid_err_t qid_mark_consumed(c, quote_id, consumer_id, &result);
qid_err_t qid_batch_validate(c, quote_ids, n, results, &nout);
qid_err_t qid_batch_mark_consumed(c, quote_ids, consumer_ids, n, results, &nout);
```

`qid_err_t` 는 transport / HTTP / JSON 레이어 에러. `qid_status_t` 는 RPC
응답의 비즈니스 상태 (OK / NOT_FOUND / EXPIRED / ALREADY_CONSUMED). 두
계층 분리로 fail-safe 정책 명확.

### TLS / 보안

`opts.ca_file / cert_file / key_file` 로 mTLS. mci-price 의
`--http-tls-client-ca` 와 짝. dev 자체발급은 `insecure_skip_verify=true`,
**운영 금지**.

### 라이브 검증

mci-price 띄워 SDK 호출:

```
$ /tmp/mci-price --listen :18084 --quoteid-instance A &
$ QID_BASE=http://localhost:18084 QID_ENGINE_ID=test-engine \
    ./example_fix_flow A-nonexistent order-42
[Validate] status=NOT_FOUND pair= bid=0.00000 ask=0.00000 profile=..
REJECT order — OrdRejReason=5 (quote_id not found)
```

HTTP → JSON → C struct → FIX OrdRejReason 매핑 전체 path 작동.

### 스레드 모델

- `qid_client_t` 인스턴스 = 1 thread (libcurl easy handle 표준).
- 다중 thread 엔진은 thread 별 인스턴스 분리 (pool 패턴).
- v2 후속 — `qid_client_pool_t` 추상화.

## v1.22 — C SDK pool (멀티스레드 엔진용, commit 추가)

매칭 엔진이 멀티스레드라 v1.21 의 단일 client 로는 부족 — libcurl easy
handle 의 단일 스레드 규칙 회피용 thread-safe pool.

### API

```c
qid_client_pool_t* pool = qid_client_pool_new(&opts, size);
qid_client_t* c = qid_client_pool_acquire(pool);   /* block 또는 NULL */
qid_client_t* c = qid_client_pool_try_acquire(pool); /* non-block */
qid_client_pool_release(pool, c);
qid_client_pool_stats_t s = qid_client_pool_stats(pool);
```

### 내부 모델

- 고정 array N 개 client 사전 생성 (boot 1회) — TLS handshake / connection
  비용을 사전 분산.
- free stack (LIFO) — release 즉시 cache-warm client 가 다음 acquire 에.
- pthread mutex + condvar — acquire block, release signal.
- 카운터 — `acquires` / `contended` (block 한 횟수, saturation 지표).

### 검증

`etc/sdk/c/test_pool.c` — pool size 4 + 16 thread × 50 호출 = 800 RPC.

```
pool=4 workers=16 iters_per=50 (total RPC=800)
stats: size=4 available=4 acquires=800 contended=21
ok=800 err=0 not_found=800
OK
```

- `available=4` — 모두 반환됨 (leak 없음).
- `contended=21` — saturation 시 block 후 다른 thread release 대기 정상.
- `ok=800` — race / 호출 누락 없음.

### 운영 — pool size 선택

- 동시 in-flight order thread × 1.5 권장 (여유).
- `contended` 증가율을 Prometheus 외부 메트릭으로 노출하면 saturation
  실시간 감지 (v2 — qid_client_pool 의 Prometheus collector).

## v1.23 — C SDK 비동기 engine (curl_multi pipelining, commit 추가)

v1.22 의 pool 이 "N thread × 각자 sync" 라면 v1.23 의 async engine 은
"1 thread × N in-flight". curl_multi handle + 1 worker thread 로 단일
caller thread 가 여러 quote 를 동시 진행.

### API

```c
qid_async_engine_t* eng = qid_async_engine_new(&opts);

qid_async_t* h = qid_validate_async(eng, qid);
/* 또는 mark_consumed_async */
qid_async_t* h = qid_mark_consumed_async(eng, qid, order_id);

int qid_async_is_done(h);                          /* non-block */
qid_err_t qid_async_wait(h);                       /* block */
qid_err_t qid_async_get_validate(h, &result);      /* wait + parse */
qid_err_t qid_async_get_mark(h, &result);
void qid_async_free(h);

qid_async_engine_free(eng);  /* worker thread join */
```

### 검증 결과

```
submit done 50 in 0.0004s
after 50ms sleep: 50/50 already done
all done in 0.0537s (submit→done 0.0533s)
ok=50 err=0 not_found=50
OK
```

- submit 50 in 0.4ms — 큐에 enqueue 만, 즉시 반환.
- 50ms sleep 동안 worker 가 모두 처리.
- 직렬 sync (~50 × 1ms = 50ms) 와 비슷한 wallclock — 로컬 loopback 이라
  RTT 가 작음. 실 네트워크 (1-2ms RTT) 에서는 async 가 50-100ms,
  sync 직렬은 50-100배 더 걸림.

### pool vs async 선택

| 조건 | 추천 |
|------|------|
| N order handler thread, 각자 1주문 = 1 RPC | pool (lock-free fast path) |
| 1 thread, batch 모드 N quote 동시 검증 | **async** |
| FIX NewOrderList 다건 한 묶음 | BatchValidate RPC + sync (서버측 fan-out) |
| pool 부족 + burst 발생 | async 병행 |

### 한계

- in-flight 상한 1024 (MAX_INFLIGHT). 초과 시 ERR_INTERNAL 즉시 — drop.
  높은 burst 시나리오는 engine 인스턴스 추가.
- worker thread 1개 — 1 multi handle 의 CPU bound 한계. 보통 문제 없음
  (curl 은 응답 파싱이 cheap), 필요시 multi engine 인스턴스.

## v1.24 — C SDK Prometheus exposition (commit 추가)

엔진측 pool / async stats 를 Prometheus 텍스트 형식으로 출력. 엔진팀이
자기 `/metrics` HTTP 응답에 첨부하거나 pushgateway 로 push — Go 측 mci-price
의 `/metrics` 와 동등하게 엔진 client-side 도 관측 가능.

### API

```c
size_t qid_client_pool_stats_text(pool, "matching-A", buf, sizeof(buf));
size_t qid_async_engine_stats_text(eng, "matching-A", buf, sizeof(buf));
```

snprintf 시맨틱 — 반환값 > cap 이면 truncation.

### 출력 metric

```
qid_pool_size{service="matching-A"}            gauge
qid_pool_available{service="matching-A"}       gauge
qid_pool_acquires_total{service="matching-A"}  counter
qid_pool_contended_total{service="matching-A"} counter
qid_async_submits_total{service="matching-A"}  counter
qid_async_completed_total{service="matching-A"} counter
qid_async_failed_total{service="matching-A"}   counter
qid_async_in_flight{service="matching-A"}      gauge
```

### 권장 엔진 측 alert

| Alert | 조건 | severity |
|-------|------|----------|
| Pool saturation | `rate(contended)/rate(acquires) > 0.1` | warn |
| Pool empty 지속 | `available == 0` for 1m | warn → page |
| Async transport failure | `rate(failed) > 0.001/s` | warn |
| Async in_flight 임박 | `in_flight > 800` (cap 1024) | warn |

### 라이브 검증

```
test_pool:
  qid_pool_size{service="test-pool"} 4
  qid_pool_acquires_total{service="test-pool"} 800
  qid_pool_contended_total{service="test-pool"} 16

test_async:
  qid_async_submits_total{service="test-async"} 50
  qid_async_completed_total{service="test-async"} 50
  qid_async_failed_total{service="test-async"} 0
  qid_async_in_flight{service="test-async"} 0
```

## v2 후보

- recording rules — PromQL 미리 계산 (예: ALREADY_CONSUMED ratio) 으로
  대시보드 응답 속도 개선.
- audit ring + websocket — engine_id 변경 시 다른 운영자가 즉시 알림.
