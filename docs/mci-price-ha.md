# mci-price 다중 인스턴스 / HA 운영 명세

WTG 의 mci-price 가 여러 인스턴스로 동시에 운영될 때 어떤 컴포넌트가
자동으로 격리되고 어떤 부분이 운영 측 합의가 필요한지 정리.

코드 참조: `internal/price/server.go` (broker subscribe), `internal/price/archiver_pgx.go`
(ON CONFLICT DO NOTHING), `pkg/quoteid/` (Registry backend).

---

## 1. 자동으로 OK — 변경 없이 다중 인스턴스 가능

### 1.1 broker fan-out (FANOUT broadcast)

```go
Queue: &mymq.QueueOptions{
    Flags:        mymq.QfUnsolMsg | mymq.QfUnsolHdr | mymq.QfUnsolRep,
    ExchangeName: cfg.ExchangeName,
    ExchangeType: mymq.ExchangeFanout,
}
```

`QfUnsolRep` (representative receiver) + `ExchangeFanout` 으로 등록되어 broker 의
`publish.c` 가 **모든 인스턴스에 broadcast**. 즉:

- N 개의 mci-price 가 같은 raw tick 을 동시 수신
- broker 측에서 round-robin 분배 X → "queue group" 시맨틱 아님
- 한 인스턴스 죽어도 다른 인스턴스가 계속 처리

### 1.2 BestConsumer / Aggregator / PricingConsumer / CrossRateConsumer

모두 **메모리 only state**. 인스턴스끼리 격리:

| Consumer | state | 다중 인스턴스 충돌? |
|----------|-------|--------------------|
| BestConsumer | per (Symbol, Source) 캐시 | X — 각자 동일 BEST tick 산출 |
| Aggregator | per timeframe OHLC | X — 각자 동일 bar 산출 |
| PricingConsumer | PricingTable snapshot 적용 | X — 각자 동일 quote 산출 |
| CrossRateConsumer | leg 캐시 + cross 합성 | X — 각자 동일 cross 산출 |

### 1.3 etcd 카탈로그 (PricingTable / SymbolMap / CurrencyMaster / PairMaster)

모든 인스턴스가 **같은 etcd watch** — atomic snapshot 교체 + 동일 결과.
admin UI 가 PUT 한 PolicyDoc 이 모든 인스턴스에 즉시 전파.

### 1.4 Archiver → TimescaleDB

```sql
INSERT INTO quote_bars (...)
ON CONFLICT (pair, tf, opened_at) DO NOTHING
```

PK 충돌 시 first-wins. 두 인스턴스가 같은 (pair, tf, opened_at) bar 를 동시
INSERT 시도해도 DB row 1건만 보존. 통합 테스트
`TestPgxInserter_MultiInstanceConcurrent` 가 검증.

---

## 2. 운영자 합의 필요 영역

### 2.1 gRPC PriceService 클라이언트 (mci-edge-price)

mci-edge-price 가 어느 mci-price 인스턴스에 stream 연결하는지는 운영자가 정함:

| 모델 | 장단점 |
|------|--------|
| **로드밸런서 (L7 gRPC LB)** | 죽으면 자동 failover, 균등 분배. setup 복잡 |
| **edge 측 client-side LB** | grpc-go 의 `round_robin` + DNS — 추가 인프라 X |
| **단일 active + standby** | 운영 단순, 죽으면 수동 전환 |

권장: edge 가 gRPC `round_robin` resolver + healthcheck 활용. 인스턴스 죽으면
client 가 자동으로 다른 인스턴스에 reconnect.

### 2.2 QuoteID Registry backend

| Backend | 다중 인스턴스 호환성 |
|---------|---------------------|
| **메모리** (`MemoryRegistry`) | 인스턴스끼리 quote_id 격리 — 다중 인스턴스 시 **사용 X** |
| **Redis** (`RedisRegistry`) | atomic CAS — 모든 인스턴스 일관 first-wins |

운영 시 반드시 **Redis backend** 또는 다른 공유 저장소. mci-price 의 quoteid
backend 설정 (`--quoteid-redis-*`) 으로 활성.

`mark_consumed` 의 first-wins 가 redis SETNX 기반이라 인스턴스 무관 atomic.

### 2.3 broker 끊김 시 메시지 손실

broker (`mymqd`) 는 **at-most-once** PUB/SUB.

- 인스턴스가 broker 끊김 → reconnect 사이의 메시지는 **잃음**
- 재연결 후 새로운 메시지부터 받음 (replay X)
- 다중 인스턴스라도 broker 자체가 메시지 보관 안 함 — 모든 인스턴스가 같은
  순간에 끊기면 같은 손실
- 알람: `wtg_broker_disconnects_total` + `wtg_broker_inflight_aborted_total`

운영 권장:
- mci-price 인스턴스를 **서로 다른 broker 머신** 에 분산 (broker 도 HA 면)
- 또는 broker 가 단일이면 disconnect 알람을 page severity 로

---

## 3. Prometheus 다중 인스턴스 scrape

같은 job 에 여러 인스턴스 등록 시 운영 측 `prometheus.yml` 에서 `labels`
명시 — 코드 변경 없이 metric 분리.

```yaml
scrape_configs:
  - job_name: mci-price
    static_configs:
      - targets: ["host-1:8082"]
        labels:
          service: mci-price
          instance: "1"   # cfg.Instance 와 동일하게
      - targets: ["host-2:8082"]
        labels:
          service: mci-price
          instance: "2"
```

쿼리:

```promql
# 인스턴스별 reconnect rate
sum by (instance) (rate(wtg_broker_reconnects_total[5m]))

# 한 인스턴스만 비정상 — 다른 인스턴스 정상
rate(wtg_pricing_ticks_in_total{instance="1"}[1m])
rate(wtg_pricing_ticks_in_total{instance="2"}[1m])
```

Grafana 대시보드의 변수 `$instance` 로 인스턴스 선택 가능하게.

---

## 4. failover 시나리오

### 4.1 한 인스턴스 죽음

```
인스턴스 1 죽음
  ↓
- broker → 인스턴스 1 으로의 broadcast 만 손실
- 인스턴스 2 는 계속 동일 raw tick 받음 → BEST/Bar/cross 계속 산출
- gRPC SubscribeQuote 클라이언트 (edge):
   - LB 모델이면 자동으로 다른 인스턴스로 reconnect
   - 단일 active 면 수동 전환 또는 standby 활성
- Archiver:
   - 인스턴스 2 가 같은 bar 를 archive — ON CONFLICT 로 정합성 OK
   - 인스턴스 1 이 부재한 동안 bar 한 건도 손실 X (인스턴스 2 가 모두 처리)
```

### 4.2 broker 죽음 / 재시작

```
broker 죽음
  ↓
- 모든 인스턴스의 connection 끊김 → MetricsHook.OnDisconnect
- in-flight RPC → ErrBroker (inflight_aborted_total 증가)
- exponential backoff (1s → 30s) 후 재연결 시도
broker 복구
  ↓
- 모든 인스턴스가 핸드셰이크 → broker 에 receiver 재등록 (자동 재구독)
- 끊김 ~ 복구 사이 raw tick 은 손실 (broker 가 보관 안 함)
- 메트릭: reconnect_duration_seconds 의 p95 가 backoff 누적량 반영
```

### 4.3 TimescaleDB 죽음

```
DB 죽음
  ↓
- Archiver 의 Insert 가 에러 — Archiver 가 backoff + retry
- 메모리 buffer 가득 차면 oldest bar drop
- 인스턴스의 BEST/Bar 산출 자체는 계속 (메모리 only)
DB 복구
  ↓
- 누락된 bar 는 backfill 불가 (인메모리 buffer 가 작음)
- 분쟁 시 quote_id Record 의 raw 입력으로 마진 재계산 가능 (mci-admin 의
  `/v1/admin/margin/recompute`)
```

---

## 5. 운영 체크리스트

다중 인스턴스 도입 시:

- [ ] mci-price 모두 동일 `--exchange` 값 (broker 측 단일 exchange 사용)
- [ ] mci-price 각자 다른 `--instance` 값 (broker ApplName 충돌 방지)
- [ ] 모든 인스턴스가 같은 etcd cluster 와 PricingTable 구독
- [ ] QuoteID backend = Redis (메모리 X)
- [ ] gRPC PriceService 의 LB / failover 모델 합의
- [ ] Prometheus scrape config 의 `instance` label 명시
- [ ] Grafana 대시보드의 변수 `$instance` 적용
- [ ] Alert rule 의 `sum by (instance) (...)` 분기 점검

---

## 6. 향후 (PR 3)

- broker 재시작 카오스 테스트 — 손실 측정 + 알람 발화 검증
- 인스턴스 재시작 시 catch-up 시간 측정
- TimescaleDB 끊김 시 backfill 정책 설계
