# WTG Grafana Dashboards

## p6-cross-master-dashboard.json — WTG P6 Cross-rate & Pricing fan-out

mci-price 의 `/metrics` 에서 노출되는 `wtg_cross_*`, `wtg_pricing_*`,
`wtg_master_*` (총 20개) 를 시각화. 23 패널 (4 row + 14 stat + 5 timeseries).

### Variable

- `$DS_PROMETHEUS` — Prometheus data source.
- `$rate_window` — rate() 윈도우 (1m / 5m / 15m / 1h). default 5m.

### 패널 구성

| Row | Panel | 의도 |
|-----|-------|------|
| Overview | Currency / Pair total / Pair active / Cross formulas / Customers (5L) / Tick processed | 마스터 / 활성 상태 한눈 |
| Cross-rate | Cross emit rate (timeseries) | 합성 throughput |
| Cross-rate | Skipped breakdown (debounce/missing_leg/stale/unknown_pair) | 원인별 추적 |
| Cross-rate | Errors / Unknown pair (alert) / Hit rate % / Stale (cooker 끊김) | 운영 health |
| Pricing | Tick in/drop rate (timeseries) | 입력 부하 + drop 비율 |
| Pricing | Profile-only vs 5-Layer publish rate | customer fan-out 분리 추세 |
| Pricing | Publish errors / Registry.Put failures / Profiles skipped / Drop ratio % | 에러 / 효율 |
| Master | sizes over time (timeseries) | fx-sync sync 상태 추적 |

### Import / Provisioning

위 quoteid-dashboard 와 동일 — Grafana UI Import 또는 API:
```bash
curl -X POST http://grafana.local/api/dashboards/db \
  -H "Authorization: Bearer $GRAFANA_TOKEN" \
  -H "Content-Type: application/json" \
  -d @<(jq '{dashboard: ., overwrite: true}' etc/grafana/p6-cross-master-dashboard.json)
```

### Alert rules — p6-cross-master-alerts.json

Grafana Unified Alerting 그룹 1 (`wtg-p6`) + 6 rule:

| UID | severity | 조건 | for |
|-----|----------|------|-----|
| wtg-p6-cross-errors | **page** | `max(wtg_cross_errors_total) > 0` | 1m |
| wtg-p6-cross-stale | warn | `rate(wtg_cross_skipped_stale_total[5m]) > 1` | 5m |
| wtg-p6-master-empty | **page** | `wtg_master_pair_active_count < 1` | 5m |
| wtg-p6-publish-errors | warn | `rate(profile + 5L publish errors[5m]) > 0.1` | 5m |
| wtg-p6-quote-register-errors | warn | `rate(wtg_pricing_quote_register_errors_total[5m]) > 0.01` | 5m |
| wtg-p6-tick-drop-ratio | warn | `dropped / in > 20%` | 5m |

각 rule 의 `runbook_url` 에 진단 절차 명시. labels.severity 로 notification
policy 분기 (page→PagerDuty / warn→Slack).

#### Import — UI

Grafana → Alerting → Alert rules → Import → `p6-cross-master-alerts.json` 선택.

#### Provisioning

deploy/observability 의 docker-compose 가 이미 alerting 디렉토리 마운트 —
변경 사항 file 저장 후 Grafana 재시작:

```bash
docker compose restart grafana
```

#### 활성 alert 확인

```bash
# 모든 rule 목록
curl -s -u admin:admin http://localhost:3030/api/v1/provisioning/alert-rules \
  | jq -r '.[] | "\(.uid)\t\(.labels.severity)\t\(.title)"'

# firing 중인 alert
curl -s -u admin:admin http://localhost:3030/api/alertmanager/grafana/api/v2/alerts \
  | jq -r '.[] | "\(.labels.alertname)\t\(.status.state)"'
```

---

## quoteid-dashboard.json — WTG QuoteValidationService

mci-price 의 `/metrics` endpoint 에서 노출되는 `wtg_quoteid_*` 시리즈를
시각화. 17 패널 / 23 PromQL 쿼리.

### Import

1. Grafana UI → Dashboards → Import.
2. "Upload JSON file" 로 `quoteid-dashboard.json` 선택.
3. Data source 선택 (Prometheus instance) → Import.

또는 API 로:

```bash
curl -X POST http://grafana.local/api/dashboards/db \
  -H "Authorization: Bearer $GRAFANA_TOKEN" \
  -H "Content-Type: application/json" \
  -d @<(jq '{dashboard: ., overwrite: true}' etc/grafana/quoteid-dashboard.json)
```

### Variable

- `$DS_PROMETHEUS` — Prometheus data source.
- `$service` — `service` label 값 (default `mci-price`). 다중 인스턴스 시
  `mci-price-A|mci-price-B` 같이 regex 가능.
- `$rate_window` — rate() 윈도우 (1m / 5m / 15m / 1h). default 5m.

### 패널 구성

| Row | Panel | 의도 |
|-----|-------|------|
| Overview | Total RPC rate | 전체 트래픽 부하 |
| Overview | OK rate | 정상 처리량 |
| Overview | RBAC denied | alert (> 0 이면 정책 위반) |
| Overview | ALREADY_CONSUMED ratio | replay / 봇 의심 (yellow 0.1%, red 1%) |
| Latency | Validate / MarkConsumed 단건 p50/95/99 | 단일 RPC SLO |
| Latency | Batch* wallclock p50/95/99 | 다건 RPC SLO |
| Throughput | Validate / MarkConsumed by status | 분류별 RPS 추이 |
| Batch | 평균 batch size | 배치 활용도 |
| Batch | Batch size 분포 heatmap | 분포 변화 detect |
| Errors | RBAC denied / Internal | 보안 / 인프라 이상 |
| Errors | Consume conflict (already / expired / not_found) | race / 클럭 skew |
| AsyncRegistry | Queue length (stat) | 현재 backlog (5k yellow, 8k red) |
| AsyncRegistry | Dropped rate (stat) | audit 누락 (alert) |
| AsyncRegistry | Written / Failed rate (stat) | worker 처리량 / 실패 |
| AsyncRegistry | Enqueue / Write / Drop / Fail throughput | 시계열 비교 |
| AsyncRegistry | Queue length over time | 백로그 추세 (saturation 진행 추적) |

### Recording rules — quoteid-recording-rules.yml (v1.25 추가)

자주 쓰는 PromQL 을 Prometheus 측에서 미리 계산해 `wtg:*` 시계열로 저장.
Grafana panel / alert rule 이 무거운 `histogram_quantile` / `rate / clamp_min`
재계산 없이 사전 결과만 lookup.

#### 설치 (Prometheus side)

```yaml
# prometheus.yml
rule_files:
  - "/etc/prometheus/rules/quoteid-recording-rules.yml"
```

또는 k8s `kubectl create configmap quoteid-recording-rules
--from-file=quoteid-recording-rules.yml` 로 mount.

#### 4 group / 15 rule

| Group | rules | 의도 |
|-------|-------|------|
| `wtg-quoteid-latency` | 6 (p99 × 4 ops + p95 × 2) | dashboard 의 무거운 quantile 제거 |
| `wtg-quoteid-ratios` | 3 (ALREADY_CONSUMED ratio / OK ratio / denied rate) | alert / health 게이지 base |
| `wtg-quoteid-async` | 3 (drop ratio / lag / queue max) | backpressure 지표 |
| `wtg-quoteid-throughput` | 3 (validate / mark / total rate) | 처리량 추세 |

#### 적용 현황 (v1.27)

alerts JSON 의 다음 3개 rule 이 v1.25 recording series 사용:

| Rule | 변경된 expr |
|------|------------|
| `wtg-quoteid-rbac-denied` | `sum(wtg:quoteid_denied:rate5m)` |
| `wtg-quoteid-consume-already` | `max(wtg:quoteid_already_consumed:ratio)` |
| `wtg-quoteid-batch-latency` | `max(wtg:quoteid_batch_validate:p99)` |

dashboard 의 Overview "ALREADY_CONSUMED ratio" stat 도 recording 사용:
```promql
max(wtg:quoteid_already_consumed:ratio{service=~"$service"})
```

dashboard 의 latency 패널 (Validate / MarkConsumed / Batch* p50/p95/p99) 는
사용자 변경 가능한 `$rate_window` 변수 의존 — raw expression 유지.

#### 운영 주의

위 4 항목이 활성화되려면 Prometheus 측에 recording rules 가 먼저 로드
되어야 한다. 로드 안 됐으면 alerts / dashboard 패널이 비어 보임. 점검
:
```bash
curl -s http://prometheus:9090/api/v1/rules | jq '.data.groups[].rules[] | select(.type=="recording") | .name'
```
`wtg:quoteid_*` 가 보여야 OK.

### Alert rules — quoteid-alerts.json (v1.16 추가)

Grafana Unified Alerting 그룹 1개 (`wtg-quoteid`) + 5 rule 패키지:

| UID | severity | 조건 | for |
|-----|----------|------|-----|
| wtg-quoteid-rbac-denied | **page** | denied rate > 0.01/s | 1m |
| wtg-quoteid-consume-already | warn | already_consumed ratio > 0.1% | 5m |
| wtg-quoteid-batch-latency | warn | BatchValidate p99 > 50ms | 5m |
| wtg-quoteid-internal | **page** | internal rate > 0.001/s | 2m |
| wtg-quoteid-register-errors | warn | Registry.Put 실패율 > 0.01/s | 5m |

#### Import — UI

Grafana → Alerting → Alert rules → Import → `quoteid-alerts.json` 선택 →
folder "WTG" 자동 생성.

#### Provisioning

파일을 그대로 Grafana provisioning 디렉토리에 마운트:

```yaml
# docker-compose.yml 발췌
services:
  grafana:
    image: grafana/grafana:10.4
    volumes:
      - ./etc/grafana/quoteid-alerts.json:/etc/grafana/provisioning/alerting/quoteid-alerts.json:ro
```

또는 kubectl ConfigMap → mount.

#### 변수 치환

`${DS_PROMETHEUS}` 는 Grafana 의 Prometheus data source UID. import 시
대화상자에서 선택. provisioning 시 환경변수로 (`GF_DATASOURCE_PROMETHEUS_UID`)
또는 import 후 UI 에서 수동 매핑.

#### 알림 통합

각 rule 의 `labels.severity` (`page` / `warn`) 에 따라 PagerDuty /
Slack contact point 라우팅:

```yaml
# Grafana notification policy
matchers:
  - severity = page
  receiver: pagerduty-oncall

matchers:
  - severity = warn
  receiver: slack-trading-ops
```
