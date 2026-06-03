# WTG 운영 모니터링 명세

admin UI 의 "운영 모니터링" 페이지 카드 정의 + Prometheus proxy 동작 + tuning.

코드 참조: `internal/admin/admin_promproxy.go` (proxy), `internal/admin/ui/index.html`
(MON_CARDS 정의).

---

## 1. 동작

```
admin UI 의 카드
   ↓ GET /v1/admin/prom-query?path=query&query=<PromQL>
admin (Go)
   ↓ http GET <PromURL>/api/v1/query?query=...
Prometheus
```

- proxy 가 path 화이트리스트 (`query`, `query_range`) 만 허용
- POST 거부 (write 차단)
- admin `--prom-url` 미설정 시 503 → UI 가 배너 표시
- 모든 호출은 인증 거친 후 (admin 의 기존 미들웨어 체인)

## 2. 카드 카탈로그

자세한 의미는 `index.html` 의 `MON_CARDS` const.

| Key | Title | Query | Warn | Page |
|-----|-------|-------|------|------|
| `http_rate` | HTTP 요청율 (전체) | `sum(rate(wtg_http_requests_total[5m]))` | — | — |
| `http_5xx` | 5xx 에러율 | `sum(rate(wtg_http_requests_total{status=~"5.."}[5m]))` | 0.1/s | 1/s |
| `rl_denied` | Rate limit 거부율 | `sum(rate(wtg_ratelimit_denied_total[5m]))` | 0.5/s | 5/s |
| `rl_login_denied` | Login brute force | `sum(rate(wtg_ratelimit_denied_total{rule="POST /v1/login"}[5m]))` | 0.5/s | 1/s |
| `broker_disc` | Broker 끊김율 | `sum(rate(wtg_broker_disconnects_total[5m]))` | 0.05/s | 0.5/s |
| `broker_inflight` | Broker inflight abort | `sum(rate(wtg_broker_inflight_aborted_total[5m]))` | 1/s | 10/s |
| `qid_denied` | QuoteID RBAC denied | `sum(rate(wtg_quoteid_op_total{status="denied"}[5m]))` | 0.5/s | 5/s |
| `qid_replay` | QuoteID ALREADY_CONSUMED | `sum(rate(wtg_quoteid_op_total{status="already_consumed"}[5m]))` | 0.5/s | 5/s |

표시 단위는 모두 `/s`. 임계 색상:

| 상태 | 색 | 의미 |
|------|----|------|
| 정상 | 녹색 dot | `value < warn` |
| 경고 | 노란 dot | `warn <= value < page` |
| 심각 | 빨간 dot | `value >= page` |

## 3. 시간 윈도우

상단 드롭다운으로 `1m / 5m / 15m / 1h` 변경 가능. 카드 query 의 `__W__` 가
런타임에 치환됨. 자동 새로고침 (10s) 토글 가능.

## 4. 운영자 사용 패턴

| 시나리오 | 보는 카드 |
|---------|-----------|
| 알람 발화 직후 빠른 확인 | 해당 도메인 카드 (rl_login_denied, broker_disc 등) |
| 평소 daily 점검 | http_rate, http_5xx, broker_disc |
| 매매 영향 의심 시 | broker_inflight, qid_denied |
| replay 의심 시 | qid_replay |

장기 추세 / 상관 분석은 Grafana 사용 권장 — 본 페이지는 "지금 상태" 만.

## 5. 카드 추가하기

`index.html` 의 `MON_CARDS` 배열에 entry 추가:

```js
{
  key: "my_metric",
  title: "My Metric",
  query: "sum(rate(wtg_my_metric_total[__W__]))",
  unit: "/s",
  format: v => v.toFixed(2),
  warn: 1, page: 10,
},
```

영어 prefix `wtg_` 만 사용 (admin proxy 가 path 만 막고 query 자체는 자유 PromQL,
실수로 노출 카드의 query 가 잘못 쓰여 운영 정보 누설되지 않도록 docs review).

## 6. Prometheus URL 운영 설정

```bash
mci-admin --prom-url http://prometheus:9090
# 또는
WTG_ADMIN_PROM_URL=http://prometheus:9090 mci-admin
```

docker-compose 환경:
```yaml
WTG_ADMIN_PROM_URL=http://wtg-prometheus:9090
```

dev 환경 (host network):
```bash
mci-admin --prom-url http://127.0.0.1:9095
```

## 7. 한계

- instant query 만 — 시계열 sparkline 은 후속 PR (`query_range` 사용)
- query 화이트리스트 X — 잘못된 PromQL 도 proxy 통과 (Prometheus 가 에러)
- 운영 페이지 자체에 alert 미연동 — Grafana alert UI 와 분리

## 8. Alert state 섹션

`page-monitoring` 하단의 alert 테이블 — Grafana 의 wtg-* 그룹 룰을 라이브
표시. firing 알람을 운영자가 admin UI 안에서 즉시 확인.

```
admin UI alert 섹션
   ↓ GET /v1/admin/grafana-alerts
admin (Go)
   ↓ http GET <GrafanaURL>/api/prometheus/grafana/api/v1/rules
   ↓ (옵션) Basic auth: admin/secret
Grafana
```

### 운영 설정

```bash
mci-admin \
  --grafana-url http://grafana:3000 \
  --grafana-user admin \
  --grafana-pass <secret>
# 또는
WTG_ADMIN_GRAFANA_URL=http://grafana:3000 \
WTG_ADMIN_GRAFANA_USER=admin \
WTG_ADMIN_GRAFANA_PASS=<secret> \
mci-admin
```

### 표시

| 필드 | 의미 |
|------|------|
| state | firing (빨강) / pending (노랑) / inactive (회색) |
| title | alert rule 이름 |
| severity | `page` (빨강) / `warning` (노랑) — label severity |
| domain | label domain (ratelimit, broker, master-sync 등) |
| value | 평가값 (지수 표기) |

상단 카운터: firing / pending / inactive 누적. 기본 필터 "firing/pending 만"
체크박스로 조정.

### 한계

- 그룹 prefix `wtg-` 만 노출 — Grafana 내 다른 알람은 무시
- alert 진압 / annotation snooze 는 UI 안 — Grafana 콘솔에서 직접
- alert history (시작 시각, 평가 횟수) 미표시

## 9. Sparkline (mini trend)

각 카드 하단의 mini line chart — Prometheus `query_range` 호출로 최근
**시간 윈도우 × 2** 의 시계열을 그린다 (예: 5m 윈도우 → 10m 시계열).

- 라이브러리 없이 vanilla SVG `<polyline>`
- 점 수: 약 40개 (step = window / 40, 최소 5s)
- 마지막 값의 warn/page 임계로 선 색상 결정
- query 실패 시 silent — 인스턴트 카드 값은 표시 유지

### query_range proxy

PR 1 의 PromQuery 핸들러가 `path=query_range` 도 처리 — `start / end / step`
파라미터 전달. UI 자동.

## 10. Alert deep link

alert 테이블의 title 셀 클릭 시 Grafana 의 alert 리스트 페이지로 이동.

- URL: `<GrafanaURL>/alerting/list?queryString=<encoded-name>`
- Grafana 검색 바에 alert 이름이 자동 입력되어 필터링됨
- target=_blank — 새 탭으로 열림

GrafanaURL 은 admin 의 `/v1/admin/grafana-config` endpoint 가 반환 (UI 가 1회
호출). admin `--grafana-url` 미설정 시 빈 string — 링크 미동작 (plain text).

### Limitation

- Grafana 의 prometheus rules API 는 alert UID 를 반환하지 않음
- 그래서 검색 기반 deep link 사용 (정확한 단일 페이지 X)
- 운영자가 검색 결과에서 한 번 클릭 더 필요

향후 — Grafana 의 `provisioning/alert-rules` API 추가 호출로 name → UID
매핑하면 정확한 단일 페이지로 link 가능. cardinality 낮으니 비용 적음.

## 11. 향후

- alert annotation / silence 통합
- 카드 hover 시 source query 표시 (디버그)
- threshold 동적 변경 (admin UI 에서)
