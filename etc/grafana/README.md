# WTG Grafana Dashboards

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
