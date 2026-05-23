-- WTG (Winway Trading Gateway) chart 저장소
-- TimescaleDB hypertable: quote_bars
-- 자세한 설계 결정 근거: docs/chart-schema.md

-- 1) 본 테이블
CREATE TABLE IF NOT EXISTS quote_bars (
    pair        TEXT             NOT NULL,    -- 'USD/KRW'
    tf          TEXT             NOT NULL,    -- '1m'|'5m'|'15m'|'1h'|'1d' (1s 봉은 메모리만)
    opened_at   TIMESTAMPTZ      NOT NULL,    -- 봉 시작 (포함)
    closed_at   TIMESTAMPTZ      NOT NULL,    -- 봉 종료 (불포함)
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

-- 3) descending 시간 정렬용 인덱스
CREATE INDEX IF NOT EXISTS idx_quote_bars_pair_tf_time
    ON quote_bars (pair, tf, opened_at DESC);

-- 4) 압축 정책 (7일 후)
ALTER TABLE quote_bars SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pair, tf',
    timescaledb.compress_orderby   = 'opened_at DESC'
);
SELECT add_compression_policy('quote_bars', INTERVAL '7 days', if_not_exists => TRUE);

-- 5) retention 정책 (2년)
SELECT add_retention_policy('quote_bars', INTERVAL '2 years', if_not_exists => TRUE);
