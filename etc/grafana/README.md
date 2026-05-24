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

### Alert 후보 (Grafana Unified Alerting)

operations.md §v1.13 의 Grafana 쿼리 예제 참조. 임계값:

- `wtg_quoteid_op_total{status="denied"} rate > 0.01/s` — page (RBAC 위반)
- `wtg_quoteid_op_total{status="consume_already"} ratio > 0.001` — warn
- `histogram_quantile(0.99, batch_duration) > 0.05s` — warn (batch 100건 기준 SLO)
- `wtg_quoteid_op_total{status="internal"} rate > 0.001/s` — page (인프라 이상)
