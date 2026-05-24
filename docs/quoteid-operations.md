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

## v2 후보

- Redis Cluster 호환 hash tag (`{quote_id}:q` / `{quote_id}:c`).
- HTTP native TLS — gateway 자체 인증서 (현재는 reverse proxy 의존).
- Lua script 기반 진정한 multi-key atomic (cluster 의 same-slot 보장 시).
