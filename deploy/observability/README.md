# WTG 운영 메트릭 시각화 (dev/dev-ops)

Prometheus + Grafana docker-compose. mci-price / quote-forwarder / mci-edge-price
의 `/metrics` 를 scrape, etc/grafana 의 dashboard 들을 자동 import.

## 빠른 시작

```bash
cd deploy/observability
docker compose up -d

# Grafana 로그인
open http://localhost:3000
# admin / admin → 초기 로그인 후 변경.
```

좌측 메뉴 **Dashboards → WTG 폴더** 에 자동 import 된 dashboard 들:
- **WTG P6 — Cross-rate & Pricing fan-out** (23 패널)
- **WTG QuoteValidationService** (17 패널)

## 구성

```
deploy/observability/
  docker-compose.yml             — prom + grafana
  prometheus/
    prometheus.yml               — scrape 대상 (mci-price :8082 등)
  grafana/
    provisioning/
      datasources/prometheus.yml — Prometheus 자동 등록
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

1. **Prometheus targets 모두 UP** — http://localhost:9090/targets
2. **PromQL 직접 쿼리** — http://localhost:9090/graph 에서 `wtg_cross_emits_total`
3. **Grafana dashboard** — http://localhost:3000/dashboards → WTG 폴더
4. **mci-price 의 /metrics 직접** — `curl http://localhost:8082/metrics | grep wtg_`
