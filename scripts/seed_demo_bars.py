#!/usr/bin/env python3
"""WTG demo 봉 seed — USD/KRW, EUR/KRW 의 1m / 5m / 15m / 1h 봉을
random walk 로 생성. INSERT SQL 을 stdout 으로 출력 — psql 로 파이프해서 사용.

브라우저에서 mci-chart 의 모든 TF / range 토글이 데이터가 있는 상태로
검증되도록, 1m 은 7일치 (10080봉), 상위 TF 는 aggregation 으로 직접
생성한다 (mci-price 의 Aggregator 와 동일한 1m → 5m → 15m → 1h roll-up).
"""

import random
from datetime import datetime, timedelta, timezone

PAIRS = [
    ("USD/KRW", 1400.0, 0.05),  # base, bid/ask spread
    ("EUR/KRW", 1520.0, 0.08),
    ("JPY/KRW",    9.2, 0.005),
]
DAYS = 7
SECONDS_PER_BAR = {"1m": 60, "5m": 300, "15m": 900, "1h": 3600, "1d": 86400}

NOW = datetime.now(timezone.utc).replace(second=0, microsecond=0)


def bucket_floor(t: datetime, tf: str) -> datetime:
    sec = SECONDS_PER_BAR[tf]
    epoch = int(t.timestamp())
    floored = (epoch // sec) * sec
    return datetime.fromtimestamp(floored, tz=timezone.utc)


def gen_1m_bars(pair: str, base: float, spread: float, days: int):
    """1m 봉 시리즈 — random walk + 봉당 20틱."""
    n = days * 24 * 60
    mid = base
    bars = []  # (opened_at, o, h, l, c, ticks)
    end = bucket_floor(NOW, "1m")
    start = end - timedelta(minutes=n)
    for i in range(n):
        opened_at = start + timedelta(minutes=i)
        o = mid
        ticks = [o]
        for _ in range(19):
            ticks.append(ticks[-1] + random.gauss(0, base * 0.0001))
        h, l, c = max(ticks), min(ticks), ticks[-1]
        bars.append((opened_at, o, h, l, c, len(ticks)))
        mid = c
    return bars


def rollup(bars_1m, tf: str):
    """1m 봉 → 상위 TF 로 OHLC aggregation."""
    if tf == "1m":
        return bars_1m
    buckets = {}
    for opened_at, o, h, l, c, tc in bars_1m:
        b = bucket_floor(opened_at, tf)
        if b not in buckets:
            buckets[b] = [o, h, l, c, tc]
        else:
            existing = buckets[b]
            existing[1] = max(existing[1], h)
            existing[2] = min(existing[2], l)
            existing[3] = c
            existing[4] += tc
    out = []
    for b in sorted(buckets.keys()):
        o, h, l, c, tc = buckets[b]
        out.append((b, o, h, l, c, tc))
    return out


def emit(pair: str, tf: str, bars, spread: float):
    sec = SECONDS_PER_BAR[tf]
    for opened_at, o, h, l, c, tc in bars:
        closed_at = opened_at + timedelta(seconds=sec)
        half = spread / 2
        print(
            "INSERT INTO quote_bars (pair, tf, opened_at, closed_at, "
            "open_bid, open_ask, high_bid, high_ask, low_bid, low_ask, "
            "close_bid, close_ask, tick_count) VALUES ("
            f"'{pair}', '{tf}', '{opened_at.isoformat()}', '{closed_at.isoformat()}', "
            f"{o - half:.5f}, {o + half:.5f}, {h - half:.5f}, {h + half:.5f}, "
            f"{l - half:.5f}, {l + half:.5f}, {c - half:.5f}, {c + half:.5f}, {tc}) "
            "ON CONFLICT (pair, tf, opened_at) DO NOTHING;"
        )


def main():
    random.seed(42)  # 재실행해도 같은 데이터 — debugging 일관성.
    print("BEGIN;")
    for pair, base, spread in PAIRS:
        bars_1m = gen_1m_bars(pair, base, spread, DAYS)
        for tf in ("1m", "5m", "15m", "1h", "1d"):
            emit(pair, tf, rollup(bars_1m, tf), spread)
    print("COMMIT;")


if __name__ == "__main__":
    main()
