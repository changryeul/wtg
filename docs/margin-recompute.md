# 마진 재계산 (분쟁/감사 backfill)

mci-admin 의 `POST /v1/admin/margin/recompute` — 운영자가 "이 기간에 이 마진
테이블을 적용했더라면 고객이 어떤 시세를 받았을까?" 를 시각적으로 검증.

## 사용 시나리오

- **분쟁**: 고객이 체결가에 이의 제기 → 그 시점 raw 시세 + 당시 PricingTable
  로 재계산해 "우리 시스템이 보였을 customer 가격" 산출.
- **감사**: 과거 마진 설정 점검 — 새 테이블 적용 결과를 가상으로 시뮬레이션.
- **회귀 테스트**: 마진 테이블 변경 PR 전, production 의 최근 봉에 적용해
  통계적 영향 (avg / max / min 마진 폭) 확인.

## 활성화

mci-admin 부팅 시 `--chart-dsn` 채워야 활성. 빈 값이면 endpoint 503.

```bash
mci-admin \
  --listen=:9090 \
  --chart-dsn="postgres://wtg:secret@db:5432/wtg?sslmode=disable" \
  --chart-pool=5 \
  --etcd=etcd:2379 \
  ...
```

ENV: `WTG_ADMIN_CHART_DSN`, `WTG_ADMIN_CHART_POOL`.

mci-chart / mci-price 와 **같은 TimescaleDB 인스턴스** 가리켜야 quote_bars
공유. 같은 cluster / replica OK.

## REST API

### Request

```http
POST /v1/admin/margin/recompute
Content-Type: application/json

{
  "from": "2026-05-01T00:00:00Z",
  "to":   "2026-05-01T23:59:59Z",
  "pair": "USD/KRW",
  "profile": {
    "Channel": "WEB",
    "Site":    "BRANCH",
    "Tier":    "VIP"
  },
  "table_override": {
    "version": 99,
    "hq_margin": [
      {"pair":"USD/KRW","tier":"VIP","bid_amount":0.10,"ask_amount":0.10}
    ]
  },
  "sample_limit": 10
}
```

필드:
- `from / to` (필수) — 시간 범위 `[from, to)`. tf=1m 봉 대상.
- `pair` (필수) — 통화쌍.
- `profile` (필수) — Channel / Site / Tier 모두 채워야.
- `table_override` (선택) — `pricing.PricingTableDoc` JSON. 없으면 etcd 의
  현재 `wtg/pricing/table` 키 값.
- `sample_limit` (선택, default 10, max 200) — 응답에 포함할 샘플 봉 수.
  stride 로 전체에서 고르게 선택.

### Response

```json
{
  "bars_processed": 1440,
  "table_version":  99,
  "table_source":   "override",
  "profile":        { "Channel": "WEB", "Site": "BRANCH", "Tier": "VIP" },
  "pair":           "USD/KRW",
  "from":           "2026-05-01T00:00:00Z",
  "to":             "2026-05-01T23:59:59Z",
  "stats": {
    "bid_margin_avg": -0.1023,
    "bid_margin_max":  0.0000,
    "bid_margin_min": -0.1500,
    "ask_margin_avg":  0.1015,
    "ask_margin_max":  0.1500,
    "ask_margin_min":  0.0000
  },
  "samples": [
    {
      "opened_at":    "2026-05-01T00:00:00Z",
      "raw_bid":      1400.0000,
      "raw_ask":      1400.0500,
      "customer_bid": 1399.9000,
      "customer_ask": 1400.1500,
      "bid_margin":   -0.1000,
      "ask_margin":    0.1000
    },
    ...
  ]
}
```

`bid_margin` / `ask_margin` = customer 가격 - raw 가격. bid 는 보통 ≤ 0
(고객에게 더 낮은 매도 호가), ask 는 ≥ 0 (더 높은 매수 호가).

## 한계

- **1m 봉만** — 더 정밀한 (1s / tick 단위) 재계산은 raw tick 저장 안 함.
- **close_bid/close_ask 만** — OHLC 의 close 점만 사용. open/high/low 별
  재계산은 v2.
- **봉 상한 10000** — 단일 호출 abuse 차단. 더 큰 기간은 호출자가 분할.
- **timeout 30s** — 1만 봉 + 1 profile 기준 충분, 더 큰 시나리오는 async
  job (v2 후속).

## mci-admin UI 탭

mci-admin 좌측 nav 의 "마진 재계산" 클릭 → 폼 + 결과 화면.

폼 항목:
- From / To (UTC datetime-local, default 최근 1시간)
- Pair (USD/KRW / EUR/KRW / JPY/KRW / GBP/KRW / AUD/KRW / CNY/KRW)
- Channel (WEB / MOB / CS / FIX)
- Site (BRANCH / HQ)
- Tier (VIP / GOLD / STD)
- 샘플 수 (1~200, default 10)
- 체크박스 `table_override JSON 사용` — 켜면 textarea 표시. PricingTableDoc
  JSON 직접 입력. 안 켜면 etcd 의 현재 키 (`wtg/pricing/table`) 사용.

결과:
- 통계 (bid 평균/최대/최소, ask 평균/최대/최소) 6 셀
- 샘플 테이블 (시각 / raw bid·ask / customer bid·ask / bid Δ / ask Δ)

브라우저에서 즉시 시각화 — 분쟁 응대 시 운영자가 GUI 로 빠르게 확인.

## v2 후보

- **Async job 모드** — `POST /v1/admin/margin/jobs` → job_id 반환,
  `GET /v1/admin/margin/jobs/{id}` 로 polling. 1년+ / 100k+ 봉 시나리오.
- **OHLC 전체 재계산** — close 외 4 points 모두.
- **CSV / parquet export** — 정산용 다운로드.
- **Profile multi-select** — 한 호출에 N profile 비교.
- **차트 시각화** — 결과를 lightweight-charts 로 raw vs customer 선 비교.
