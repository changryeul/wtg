# Chart DB 스키마 — TimescaleDB

WTG 챠트 서비스(`mci-chart`) 가 사용하는 시계열 저장소 설계.
mci-price 의 Aggregator 가 봉을 INSERT, mci-chart 가 SELECT.

## 핵심 결정 (요약)

| 항목 | 결정 | 근거 |
|---|---|---|
| 저장 대상 | **OHLC 봉만** | raw tick 보관 X (감사 요구 없음). 30심볼 × 6 timeframe × 1년 ≈ 수십M row, 압축 후 GB 미만. |
| timeframe | 1s, 1m, 5m, 15m, 1h, 1d | 챠트 6단계. 더 큰 단위 (1w/1M) 는 1d 봉을 클라이언트가 roll-up. |
| bid/ask | **양면 모두 보관** | FX 는 spread 가 본질. mid 만 저장하면 spread 정보 소실. |
| volume | **tick count 로 대체** | FX 는 거래량 정보가 없음. tick 수 = 시장 활성도 proxy. |
| 압축 | **7일 후 자동** | TimescaleDB native compression. SegmentBy: (pair, tf). |
| retention | **2년** | 정책 정책 한 줄. 더 길게 필요하면 정책 조정. |
| Continuous aggregate | **사용 안 함** | mci-price 가 모든 timeframe 을 직접 누적 → DB 자동 roll-up 불필요. 단순성 우선. |
| 멀티 인스턴스 dedup | **PK 충돌 → ON CONFLICT DO NOTHING** | 같은 (pair, tf, opened_at) 는 같은 봉이어야 함. seqn 검사는 mci-price 내부에서. |

## DDL

```sql
-- 1) 본 테이블 (TimescaleDB hypertable)
CREATE TABLE IF NOT EXISTS quote_bars (
    pair        TEXT             NOT NULL,    -- 'USD/KRW'
    tf          TEXT             NOT NULL,    -- '1s'|'1m'|'5m'|'15m'|'1h'|'1d'
    opened_at   TIMESTAMPTZ      NOT NULL,    -- 봉 시작 시각 (포함)
    closed_at   TIMESTAMPTZ      NOT NULL,    -- 봉 종료 시각 (불포함)
    open_bid    DOUBLE PRECISION NOT NULL,
    open_ask    DOUBLE PRECISION NOT NULL,
    high_bid    DOUBLE PRECISION NOT NULL,
    high_ask    DOUBLE PRECISION NOT NULL,
    low_bid     DOUBLE PRECISION NOT NULL,
    low_ask     DOUBLE PRECISION NOT NULL,
    close_bid   DOUBLE PRECISION NOT NULL,
    close_ask   DOUBLE PRECISION NOT NULL,
    tick_count  INTEGER          NOT NULL,
    PRIMARY KEY (pair, tf, opened_at)
);

-- 2) hypertable 변환 (chunk = 1일)
SELECT create_hypertable(
    'quote_bars', 'opened_at',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists => TRUE
);

-- 3) 쿼리 가속 인덱스 (PK 외에 descending 시간 정렬용)
CREATE INDEX IF NOT EXISTS idx_quote_bars_pair_tf_time
    ON quote_bars (pair, tf, opened_at DESC);

-- 4) 압축 정책 — 7일 후 압축. segment_by 는 (pair, tf) 그룹 단위.
ALTER TABLE quote_bars SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pair, tf',
    timescaledb.compress_orderby   = 'opened_at DESC'
);
SELECT add_compression_policy('quote_bars', INTERVAL '7 days', if_not_exists => TRUE);

-- 5) retention 정책 — 2년 이상 지난 chunk 자동 삭제.
SELECT add_retention_policy('quote_bars', INTERVAL '2 years', if_not_exists => TRUE);
```

## INSERT 패턴 (Archiver)

봉이 닫힐 때마다:

```sql
INSERT INTO quote_bars (
    pair, tf, opened_at, closed_at,
    open_bid, open_ask, high_bid, high_ask,
    low_bid, low_ask, close_bid, close_ask, tick_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (pair, tf, opened_at) DO NOTHING;
```

- 멀티 인스턴스 환경에서 같은 봉을 두 인스턴스가 INSERT 시도해도 안전.
- 봉 close 가 정확히 같은 시각이면 첫 번째만 저장 (mci-price 가 모든 인스턴스에서 동일한 봉 경계를 계산하므로 일관성 보장).

배치 INSERT 권장 (`COPY` 또는 `pgx.CopyFrom`):
- 봉 close 가 동시에 발생 (1초 단위) → 같은 트랜잭션에 묶어 throughput ↑
- 30 심볼 × 6 timeframe = 최대 180 row / batch (실제로는 close 시점이 다름)

## SELECT 패턴 (mci-chart)

```sql
-- 챠트 데이터 조회
SELECT opened_at, closed_at,
       open_bid, open_ask, high_bid, high_ask,
       low_bid, low_ask, close_bid, close_ask, tick_count
  FROM quote_bars
 WHERE pair      = $1
   AND tf        = $2
   AND opened_at >= $3
   AND opened_at <  $4
 ORDER BY opened_at;
```

(pair, tf, opened_at) 인덱스로 즉시 응답.

## Bar 데이터 모델 (Go ↔ DB 매핑)

| Go 필드 | DB 컬럼 | 비고 |
|---|---|---|
| `Pair`      | `pair`       | session.Pair (string) |
| `TF`        | `tf`         | quote.Timeframe (string) |
| `OpenedAt`  | `opened_at`  | time.Time (UTC) |
| `ClosedAt`  | `closed_at`  | time.Time (UTC) |
| `OpenBid` / `OpenAsk`   | `open_bid` / `open_ask`   | float64 |
| `HighBid` / `HighAsk`   | `high_bid` / `high_ask`   | float64 |
| `LowBid` / `LowAsk`     | `low_bid` / `low_ask`     | float64 |
| `CloseBid` / `CloseAsk` | `close_bid` / `close_ask` | float64 |
| `TickCount` | `tick_count` | int (uint32 도 가능; -1 senti 없음) |

## 용량 추정

- 30 심볼 × 6 timeframe × 86400 sec/일 / TF별 봉 수
  - 1s : 86400 봉/일/심볼 = 18M 행/일/30심볼 (모든 TF 중 가장 큼)
  - 1m : 1440
  - 5m : 288
  - 15m: 96
  - 1h : 24
  - 1d : 1
- 합계 (모든 TF): ~18M 행/일 × 30 = 540M / 일 (1s 봉 포함 시)

1s 봉이 지배적. 1s 봉을 뺄지 검토:
- 1s 봉 빼면: 30 × (1440+288+96+24+1) = 56,070 행/일. **연간 ~20M 행** — 가벼움.
- 1s 봉 유지 시: 압축 후 ~10:1 → 4~5GB/년. 여전히 OK.

**제안**: 1s 봉은 메모리 RingBuffer 만 두고 DB INSERT 는 1m 이상만. RingBuffer 가 1s 단위 실시간 챠트 제공.

→ DB 행수: **1m+5m+15m+1h+1d**, 약 5만 행/일, 연간 20M.

## 다음 작업

1. 위 DDL 을 `etc/sql/quote_bars.sql` 로 저장 (운영팀에 전달)
2. `pkg/quote/bar.go` 의 Bar 타입을 DB 매핑에 맞춰 정의
3. `internal/price/aggregator.go` 는 1s 단위만 메모리, 1m 이상만 DB INSERT 대상
