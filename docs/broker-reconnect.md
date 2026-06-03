# Broker reconnect / 운영 메트릭

WTG 의 모든 mci-* 서비스가 `pkg/mymq.Client` 를 통해 broker (`mymqd`) 에 붙고,
끊김 시 자동 재연결한다. 본 문서는 reconnect 의 동작과 운영 메트릭 / 알람을 정리.

코드 참조: `pkg/mymq/reconnect.go` (supervisorLoop), `pkg/mymq/client.go`
(heartbeatLoop, MetricsHook), `pkg/metrics/metrics.go` (broker_* 카운터).

---

## 1. 자동 재연결 흐름

```
정상 운영
   ↓ broker 끊김 (TCP RST, heartbeat timeout, 서버 재시작)
1. readLoop / heartbeatLoop 가 종료 → doneCh close
2. supervisorLoop 가 도착 →
   - MetricsHook.OnDisconnect(cause)
   - failPending(cause) → 모든 in-flight RPC 에 ErrBroker 통보
     + MetricsHook.OnInflightAborted(N)
3. exponential backoff (1s → 30s, factor 2.0)
4. tryReconnect:
   - dialBroker (TCP/TLS)
   - 핸드셰이크 재실행 (DECLARE_SESSION 또는 CONNECT)
     ← broker 측 receiver / exchange 등록도 동시에 (자동 재구독)
   - heartbeatLoop 재시작
5. 성공 → MetricsHook.OnReconnect(attempts, duration)
   사용자 OnReconnect 콜백 호출 (옵션)
```

### 자동 재구독

mci-push / mci-price 처럼 `Queue` 옵션을 채운 client 는 핸드셰이크 자체가
broker 의 receiver / exchange 등록을 포함한다. 즉 reconnect = 자동 재구독.
별도 OnReconnect hook 에서 bind 를 다시 호출할 필요 없음.

mci-api / mci-admin / quote-forwarder 처럼 RPC 전용 / publisher 전용 client 는
구독 자체가 없어 재등록 대상 X.

### heartbeat watchdog

broker 가 알려준 `heartbeat_interval` 의 2배 동안 어떤 프레임도 안 오면
connection 사망 판정 (TCP keepalive 보다 빠른 감지).
-> `MetricsHook.OnHeartbeatTimeout`.

---

## 2. 운영 메트릭

`pkg/metrics` 에 등록 (모든 mci-* 서비스의 /metrics endpoint 로 노출).

| Metric | Type | Labels | 의미 |
|--------|------|--------|------|
| `wtg_broker_disconnects_total` | Counter | service | 끊김 발생 누적 |
| `wtg_broker_reconnects_total` | Counter | service | 재연결 성공 누적 |
| `wtg_broker_reconnect_duration_seconds` | Histogram | service | 끊김 -> 성공까지 wallclock |
| `wtg_broker_inflight_aborted_total` | Counter | service | failPending 으로 통보된 pending RPC 누적 |
| `wtg_broker_heartbeat_timeout_total` | Counter | service | heartbeat watchdog 발화 누적 |

label `service` 는 `mci-api / mci-push / mci-price / mci-admin / quote-forwarder`.

### 운영 dashboard 권장 패널

1. **service 별 reconnect rate** — `rate(wtg_broker_reconnects_total[5m])`
2. **service 별 disconnect 빈도** — broker 안정성 지표
3. **reconnect duration p95** — backoff 누적 시간
4. **inflight aborted spike** — broker 끊김 시 영향 받은 매매 RPC 수
5. **heartbeat timeout vs TCP RST** — `rate(wtg_broker_heartbeat_timeout_total[5m])` vs `rate(disconnects)`

---

## 3. 알람

### Grafana provisioning

`deploy/observability/grafana/provisioning/alerting/p7-broker-alerts.json` 의
그룹 `wtg-p7-broker` (4 룰) 이 docker-compose 재기동 시 자동 등록.

| 알람 | 임계 | severity | for |
|------|------|----------|-----|
| Broker reconnect rate 비정상 | `sum by (service) (rate(wtg_broker_reconnects_total[5m])) > 0.1` | warning | 2m |
| Inflight RPC abort spike | `sum by (service) (rate(wtg_broker_inflight_aborted_total[1m])) > 10` | page | 1m |
| Heartbeat watchdog 누적 | `sum by (service) (increase(wtg_broker_heartbeat_timeout_total[10m])) > 3` | page | 5m |
| Reconnect duration p95 | `histogram_quantile(0.95, ...) > 30s` | warning | 5m |

알람 발화 시:
1. `service` label 로 어느 서비스인지 (한 서비스만? 여러? broker 측 vs 클라이언트 측 분리)
2. broker (`mymqd`) 로그 확인 — heartbeat / connect 실패 사유
3. 네트워크 link / 방화벽 점검
4. 매매 영향 확인 — `inflight_aborted` 누적과 사용자 영향 매핑

---

## 4. wire-up 패턴

각 mci-* 서비스의 server.go 에서 `Options.Metrics` 채우기:

```go
mq, err := mymq.Open(ctx, host, port, mymq.Options{
    ApplName:  "mci-price",
    Reconnect: &mymq.ReconnectOptions{...},
    Metrics: mymq.MetricsHook{
        OnDisconnect:       func(_ error) { s.metrics.IncBrokerDisconnect("mci-price") },
        OnReconnect:        func(_ int, d time.Duration) { s.metrics.IncBrokerReconnect("mci-price", d) },
        OnInflightAborted:  func(n int) { s.metrics.IncBrokerInflightAborted("mci-price", n) },
        OnHeartbeatTimeout: func() { s.metrics.IncBrokerHeartbeatTimeout("mci-price") },
    },
})
```

hook 은 짧고 stateless 해야 — counter.Inc 같은 cheap 호출만 권장. 무거운
작업은 별도 goroutine.

---

## 5. 검증

`pkg/mymq` 단위 테스트:

```bash
go test -tags=integration -run='TestReconnect|TestMetricsHook' -v ./pkg/mymq/
```

검증 항목:
- broker 끊김 -> 자동 재연결 -> OnReconnect 호출
- pending RPC -> ErrBroker 통보 (failPending)
- OnDisconnect / OnReconnect 콜백 시그니처 + attempt / duration 정확성
- subCh persistence (재연결 후에도 동일 채널)
- supervisor close 시점 처리
- MetricsHook 의 4 callback 모두 발화

실제 broker 와 통합:

```bash
MYMQD_HOST=10.0.0.10 MYMQD_PORT=11217 go test -v ./test/integration/...
```

### 카오스 시나리오로 검증

`scripts/chaos-broker.sh` — broker (mymqd) docker container 를 잠시 죽이고
살려서 reconnect 메트릭 + alert 발화를 라이브 관찰.

```bash
./scripts/chaos-broker.sh quick      # 10초 다운 — metric 발화만
./scripts/chaos-broker.sh sustained  # 60초 다운 — alert for: 1m 만족
```

라이브 결과 예시 (10s 다운):
```
Before — disconnects=0 reconnects=0 inflight_aborted=0
After  — disconnects=1 reconnects=1 inflight_aborted=0
```

### 다중 인스턴스 QuoteID Redis 일관성

`pkg/quoteid/redis_test.go` 의 `TestRedisRegistry_MultiInstance_FirstWins` —
두 별도 `RedisRegistry` 인스턴스가 같은 Redis 를 공유하며 동시 `MarkConsumed`
호출 시 정확히 한 건만 `ConsumeOK`, 다른 건 `ConsumeAlreadyDone`. 후속 호출에서
모든 인스턴스가 같은 `ConsumedBy` 관찰.

```bash
go test -race -run='TestRedisRegistry_MultiInstance' ./pkg/quoteid/
```
