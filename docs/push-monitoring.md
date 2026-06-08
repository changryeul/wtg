# mci-push 모니터링 (Phase 2.5)

WTG `mci-push` 의 Prometheus 메트릭, Grafana 대시보드, alert rule 운영 가이드.

## 메트릭 카탈로그

| 메트릭 | 종류 | 라벨 | 의미 |
|---|---|---|---|
| `mci_push_dispatcher_received_total` | gauge | — | broker 에서 받은 unsolicited 총수 (legacy 누적) |
| `mci_push_dispatcher_recv_broker_total` | gauge | — | **Phase 2.5** broker subscribe 로 받은 수 (source 비교용) |
| `mci_push_dispatcher_recv_http_total` | gauge | — | **Phase 2.5** POST /v1/internal/push 로 받은 수 |
| `mci_push_dispatcher_delivered_total` | gauge | — | ws Send 까지 도달한 fan-out 합 |
| `mci_push_dispatcher_dropped_total` | gauge | — | sent=0 인 fan-out 총합 (사유별 분기 = 아래 4 합) |
| `mci_push_dispatcher_drop_unsupp_total` | gauge | — | Func 가 FCCast/FCPush/FCSignal 아님 |
| `mci_push_dispatcher_drop_envelope_total` | gauge | — | envelope JSON marshal 실패 |
| `mci_push_dispatcher_drop_unknown_user_total` | gauge | — | LogonID 명시 됐는데 user 의 conn 없음 |
| `mci_push_dispatcher_drop_no_broadcast_total` | gauge | — | LogonID 빈값인데 등록된 conn 0 |
| `mci_push_dispatcher_drop_inject_full_total` | gauge | — | **Phase 2.5** HTTP inject channel full → drop (백프레셔) |
| `mci_push_dispatcher_send_failed_total` | gauge | — | fan-out 안 일부 conn send 실패 (slow/closed) |
| `mci_push_http_inject_total` | **counter** | `cn`, `result` | **Phase 2.5** POST /v1/internal/push 처리 결과 |

`mci_push_http_inject_total` 라벨:
- `cn` — mTLS client cert 의 CN. mTLS 없으면 `"anonymous"`, cert 있는데 CN 빈값이면 `"unknown"`.
- `result` — `ok` / `unauthorized` / `bad_json` / `inject_full`.

**Cardinality 주의:** `cn` 라벨은 운영 svc 수 (보통 5~20) × 4 result = ~100 시리즈 이내. 안전.

## 대시보드 / Rule 파일

```
etc/grafana/
  mci-push-dashboard.json         # Grafana 대시보드 (10 panel + 2 stat)
  mci-push-recording-rules.yml    # Prometheus recording rules
  mci-push-alerts.yml             # Prometheus alert rules
```

### Grafana 대시보드 import

1. Grafana → Dashboards → Import → `mci-push-dashboard.json` 업로드
2. Prometheus data source 선택 → Save
3. 변수: `rate_window` (1m/5m/15m/1h), `cn` (운영 svc CN multi-select)

대시보드 구성:
- **Source 비교** — broker rate vs HTTP rate + HTTP 비중 stat + inject full drop stat
- **CN 별 호출** — 각 운영 svc 의 ok rate + result 분포
- **Dispatcher** — delivered/dropped + drop 사유별 stack + send_failed + broker disconnect

### Prometheus rules 설치

```yaml
# prometheus.yml
rule_files:
  - "/etc/prometheus/rules/mci-push-recording-rules.yml"
  - "/etc/prometheus/rules/mci-push-alerts.yml"
```

## 운영 모드

### Standalone (broker 없음) — `--no-broker`

mci-push 를 broker 연결 없이 HTTP push only 모드로 부팅.

```bash
mci-push --listen=:8081 --push-secret=$WTG_PUSH_SECRET --no-broker
```

용도:
- **dev / 통합 test** — broker 띄울 필요 없이 mci-push 단독 실행
- **장애 대응** — broker 사고 중에도 HTTP push 만으로 ws fan-out 유지
- **Phase 2.7 사전 검증** — 운영 환경에서 broker 의존 없는 시나리오 실험 (staging)

동작:
- broker subscribe 비활성 (`mci_push_dispatcher_recv_broker_total` 0 유지)
- HTTP inject 정상 (`recv_http_total` 증가)
- ws fan-out / gRPC PushService 정상 (broker 무관)
- 부팅 로그: `mci-push: broker 비활성 모드 (--no-broker) — HTTP push only`

env 대체: `WTG_PUSH_NO_BROKER=1`

## 운영 시나리오

### Phase 2 이행 진척 모니터링

핵심 KPI: `wtg:push:http_ratio:5m` (recording rule).

- 0% → broker subscribe 100%, HTTP 미사용 (Phase 0 기본)
- 50% → 운영 svc 일부가 HTTP 전환 (Phase 2.6 진행 중)
- 80%+ → 거의 HTTP, broker subscribe 잔여 — Phase 2.7 broker 제거 검토 가능
- 100% → broker subscribe 완전 제거 + `mci_push_dispatcher_recv_broker_total` 0 유지

### 알림 대응

#### `PushInjectChannelFull` (warning, for=2m)
**증상:** `wtg:push:drop_inject_full:rate5m > 0` 지속.

**원인 가설:**
1. ws fan-out 처리 속도 < HTTP inject rate
2. dispatcher inject buffer 크기 부족
3. slow ws consumer 누적

**조치:**
- `mci_push_dispatcher_send_failed_total` 같이 증가 → slow consumer 진단
- 단독 증가 → dispatcher inject buffer 상향 (코드 변경 필요)
- 운영 svc 측 호출 burst 패턴 검토

#### `PushAuthFailureSurge` (warning, for=2m)
**증상:** `sum(wtg:push:http_inject_unauth:rate5m_by_cn) > 0.05`.

**원인:**
1. mTLS client cert 만료 또는 폐기 → 운영 svc cert rotate
2. `X-Push-Secret` 환경변수 불일치 → 운영 svc env 검토
3. 의도치 않은 외부 접근 (security 사고 가능성) → audit log 즉시 확인

**진단:**
```promql
topk(5, wtg:push:http_inject_unauth:rate5m_by_cn)
```
→ 어떤 CN 이 실패 중인지 (CN 이 `anonymous` 면 mTLS 없이 secret 만으로 시도한 클라이언트).

slog audit log (`push: mTLS client`) 와 함께 보면 어느 svc 가 wrong cert 로 시도 중인지 식별 가능.

#### `PushSlowConsumer` (warning, for=5m)
**증상:** `wtg:push:send_failed:rate5m > 1`.

**원인:** ws 사용자의 수신 속도 < push rate. SendQueueSize (default 256) 초과 → conn 종료.

**조치:**
- `--send-queue` flag 로 SendQueueSize 상향 (메모리 trade-off)
- 또는 push payload 크기 / rate 축소
- 특정 사용자만 영향이면 그 클라이언트 (Visual C++ legacy CS framework 등) 검토

#### `PushBrokerDisconnected` (critical, for=10m)
**증상:** broker subscribe rate ≈ 0 + HTTP 비중 < 95%.

→ broker 자체 down 또는 mci-push ↔ broker 연결 끊김. **Phase 2.6 완료 (HTTP 100% 이행) 전엔 critical**. 이행 완료 후엔 무관 (broker subscribe 제거 가능).

## audit log (mTLS)

mTLS handshake 통과 시 매 요청마다 slog INFO 로 기록:

```json
{"time":"2026-06-08T15:23:11Z","level":"INFO","msg":"push: mTLS client",
 "cn":"order-engine-prod","remote":"10.0.3.41:54221"}
```

Loki / ELK 로 수집 시 CN 별 호출 패턴 / 비정상 시간대 호출 / 새 CN 등장 추적 가능.

## 관련 문서

- `docs/operations.md` — mci-push flag/env 카탈로그
- `docs/conventions.md` — broker channel / queue 명명
- `docs/push-secret-rotation.md` — **현재 운영 모드 (secret-only) + rotate 절차 + mTLS 향후 전환**
- `etc/grafana/mci-push-*.{json,yml}` — 본문에서 다룬 dashboard/rules
