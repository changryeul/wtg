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
  --quoteid-redis=10.0.0.10:6379 \                    # Sentinel 주소 (또는 cluster seed)
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

`--quoteid-redis` 에 Sentinel 주소 콤마 구분 — 단일 addr 만 지원하는 현 wire
한계, **TODO: FailoverClient 추가 옵션 (v1.1)**.

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

## v1 미해결 / v2 후보

- FailoverClient — `--quoteid-redis` 콤마 구분 sentinel 주소 지원.
- BatchValidate — 대량 주문 검증 효율.
- MarkConsumed — 같은 QuoteID 중복 주문 방지.
- HTTP 게이트웨이 — 비-Go FIX gateway 호환.
- BatchValidate / MarkConsumed 둘 다 RPC 추가 → RFC v2 에서 협의.
