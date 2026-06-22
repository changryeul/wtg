#!/usr/bin/env python3
"""WTG dev tick generator — mci-price /v1/dev/tick 을 200ms 주기로 호출.

6 pair × 5 tick/s = 30 tick/s. random walk 로 호가 흔듦.
브라우저 라이브 ws 페이지 / 마진 계산기 / swap-lock 시현 모두 본 스크립트가 시세 source.

사용 :
    nohup python3 scripts/wtg-dev-tickloop.py > logs/dev-tick.log 2>&1 &
    또는 wtg-stack-up.sh 가 자동 호출.

환경변수 :
    WTG_PRICE_HTTP   default http://127.0.0.1:8082
    WTG_TICK_PERIOD  default 0.2 (초)
"""
import json
import os
import random
import time
import urllib.request
import urllib.error

pairs = {
    'USDKRW': (1380.0, 1.2),
    'EURKRW': (1500.0, 1.5),
    'JPYKRW': (9.50,   0.02),
    'GBPKRW': (1730.0, 1.8),
    'EURUSD': (1.0800, 0.0003),
    'USDJPY': (150.00, 0.05),
}
base_url = os.environ.get('WTG_PRICE_HTTP', 'http://127.0.0.1:8082')
period = float(os.environ.get('WTG_TICK_PERIOD', '0.2'))
url = base_url.rstrip('/') + '/v1/dev/tick'

print(f'tickloop: {url}  period={period}s  pairs={len(pairs)}', flush=True)
while True:
    for sym, (base, spread) in pairs.items():
        bid = round(base + (random.random() - 0.5) * base * 0.001, 5)
        ask = round(bid + spread, 5)
        body = json.dumps({'symbol': sym, 'bid': bid, 'ask': ask}).encode()
        req = urllib.request.Request(
            url, data=body,
            headers={'Content-Type': 'application/json'},
        )
        try:
            urllib.request.urlopen(req, timeout=1).close()
        except urllib.error.URLError:
            pass
    time.sleep(period)
