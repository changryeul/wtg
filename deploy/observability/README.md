# WTG 운영 메트릭 + Trace 시각화 (dev/dev-ops)

Prometheus + Grafana + Jaeger docker-compose. mci-price / quote-forwarder /
mci-edge-price 의 `/metrics` 를 scrape, etc/grafana 의 dashboard 들을 자동
import. OTel SDK 가 보낸 span 은 Jaeger 에 수집 → Grafana Explore 또는
Jaeger UI 에서 시각화.

## 빠른 시작

```bash
cd deploy/observability
docker compose up -d

# Grafana 로그인
open http://localhost:3030
# admin / admin → 초기 로그인 후 변경.

# Jaeger UI
open http://localhost:16686
```

좌측 메뉴 **Dashboards → WTG 폴더** 에 자동 import 된 dashboard 들:
- **WTG P6 — Cross-rate & Pricing fan-out** (23 패널)
- **WTG QuoteValidationService** (17 패널)

**Trace 활성** — WTG 서비스 기동 시 OTel flag 추가:

```bash
mci-api      --otel-endpoint=localhost:4317 --otel-insecure --otel-sample 0.1
mci-edge-api --otel-endpoint=localhost:4317 --otel-insecure
```

요청 1건 보낸 후:
- **Jaeger UI** (http://localhost:16686) → Service: mci-api 선택 → Find Traces
- **Grafana Explore** → Datasource: Jaeger → trace_id 검색 또는 service

상세 span 카탈로그 (현재 broker.call 등): `docs/broker-tracing.md` §8.

## 구성

```
deploy/observability/
  docker-compose.yml             — prom + grafana
  prometheus/
    prometheus.yml               — scrape 대상 (mci-price :8082 등)
  grafana/
    provisioning/
      datasources/prometheus.yml — Prometheus 자동 등록
      datasources/jaeger.yml     — Jaeger trace datasource 자동 등록
      dashboards/wtg.yml         — dashboards 디렉토리 자동 import
    dashboards/                  — JSON 들 (etc/grafana 의 prov 변환본)
      p6-cross-master-dashboard.json
      quoteid-dashboard.json
```

## scrape 대상

prometheus 가 host 의 다음 endpoint 를 5~15초 주기로 scrape:

| job | target | 비고 |
|-----|--------|------|
| mci-price | host.docker.internal:8082 | P6 메트릭 (cross/pricing/master) |
| quote-forwarder | host.docker.internal:9091 | feed별 throughput / drop |
| mci-edge-price | host.docker.internal:8083 | ws subscriber 수 등 (옵션) |
| prometheus | self | 자기 진단 |

`host.docker.internal` 는 Docker Desktop (macOS / Windows) 기본 지원.
Linux 는 `extra_hosts: host.docker.internal:host-gateway` 로 처리 (compose 에 이미 포함).

## 운영 환경 적용

위 docker-compose 는 **dev 검증용**. 운영에서는:

1. 기존 Prometheus / Grafana 인프라가 있으면 `prometheus.yml` 의 scrape_configs
   해당 부분만 복사 + `etc/grafana/*.json` 을 Grafana UI Import.
2. 운영 datasource UID 가 `prometheus` 가 아니면, etc/grafana 의 dashboard
   import 시 UI 가 `\${DS_PROMETHEUS}` 변수 선택을 묻는다 — 그때 매핑.
3. Prometheus 보관 기간: dev=7d, 운영=30~90d 권장 (TSDB 디스크 산정 필요).
4. **Jaeger all-in-one 은 dev 만** — span memory 저장. 운영은:
   - OTel Collector (별도 컨테이너) → Tempo/Jaeger/Datadog/Honeycomb
   - storage backend: Elasticsearch / Cassandra / Tempo+S3
   - sampling: edge 1~10% (운영 권장 — `--otel-sample 0.05`)

## 종료 / 재시작

```bash
docker compose down       # 컨테이너 종료, volume (TSDB / Grafana DB) 보존
docker compose down -v    # data 모두 삭제 (재시작 시 dashboard 자동 재import)
docker compose restart prometheus  # 설정 변경 후 재시작
```

prometheus 설정 hot reload (재시작 없이):
```bash
curl -X POST http://localhost:9090/-/reload
```

## 확인 — 작동 검증

1. **Prometheus targets 모두 UP** — http://localhost:9095/targets
2. **PromQL 직접 쿼리** — http://localhost:9095/graph 에서 `wtg_cross_emits_total`
3. **Grafana dashboard** — http://localhost:3030/dashboards → WTG 폴더
4. **mci-price 의 /metrics 직접** — `curl http://localhost:8082/metrics | grep wtg_`
5. **Jaeger** — http://localhost:16686 → Service drop-down 에 `mci-api` 표시 (요청
   1건 이상 보낸 후). Find Traces → broker.call span tree 확인.

## Trace 검증 시나리오

```bash
# 1. WTG 활성
mci-api --otel-endpoint=localhost:4317 --otel-insecure ...

# 2. /v1/tx 호출 + traceparent 헤더
TP="00-deadbeef0123456789abcdef01234567-1122334455667788-01"
curl -X POST http://127.0.0.1:8080/v1/tx \
  -H "Authorization: Bearer $TOKEN" \
  -H "traceparent: $TP" \
  -H "Content-Type: application/json" \
  -d '{"alias":"WECHO_PING"}'

# 3. Jaeger UI 에서 trace_id 검색 — deadbeef... 입력 후 검색
open http://localhost:16686/trace/deadbeef0123456789abcdef01234567

# 4. span 트리:
#    mci-api request (auto-generated)
#      └ broker.call (xchg=ECHO rkey=PING usid=alice)
```
