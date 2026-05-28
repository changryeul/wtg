#!/usr/bin/env bash
# load-test.sh — mci-price 파이프라인 부하 시나리오 실행기.
#
# 사용:
#   ./scripts/load-test.sh low      # baseline (~640 tick/s)
#   ./scripts/load-test.sh mid      # typical 변동성 (~6.4k tick/s)
#   ./scripts/load-test.sh high     # extreme burst (~64k tick/s)
#   ./scripts/load-test.sh custom RATE PAIRS_CSV [DURATION]
#
# 사전 조건:
#   wtgctl status      # broker / mci-price / mci-chart / quote-forwarder up
#   WTG_PRICE=1 wtgctl start
#
# 결과:
#   build/bin/load-gen 가 진행 라인 + 종료 summary 를 stdout 으로.
#   --csv 옵션으로 logs/load-<scenario>-<ts>.csv 저장 (run 간 비교용).
#
# 주의:
#   high 시나리오는 ~64k tick/s 를 30s 동안 (1.9M 패킷). kernel UDP 큐 한계
#   초과 시 send err 발생 — sysctl net.inet.udp.recvspace / sendspace 조정
#   고려. macOS 기본 9216 너무 작음 — sudo sysctl -w net.inet.udp.maxdgram=9216.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
LOADGEN="$REPO_ROOT/build/bin/load-gen"
LOGDIR="$REPO_ROOT/logs"

if [ ! -x "$LOADGEN" ]; then
    echo "build/bin/load-gen 부재 — 빌드합니다..."
    (cd "$REPO_ROOT" && go build -o build/bin/load-gen ./cmd/load-gen)
fi
mkdir -p "$LOGDIR"

scenario="${1:-}"
if [ -z "$scenario" ]; then
    echo "사용법: $0 {low|mid|high|custom}" >&2
    exit 2
fi

ts=$(date +%Y%m%d-%H%M%S)
csv="$LOGDIR/load-${scenario}-${ts}.csv"

case "$scenario" in
    low)
        # baseline: 4 feed × 4 pair × 40 tick/s = 640 tick/s
        rate=40 pairs=USDKRW,EURUSD,USDJPY,GBPUSD duration=30s
        ;;
    mid)
        # typical 변동성: 4 feed × 8 pair × 200 tick/s = 6400 tick/s
        rate=200 pairs=USDKRW,EURUSD,USDJPY,GBPUSD,AUDUSD,EURKRW,JPYKRW,GBPKRW duration=30s
        ;;
    high)
        # extreme burst: 4 feed × 8 pair × 2000 tick/s = 64000 tick/s
        rate=2000 pairs=USDKRW,EURUSD,USDJPY,GBPUSD,AUDUSD,EURKRW,JPYKRW,GBPKRW duration=30s
        ;;
    custom)
        rate="${2:?custom RATE 필요}"
        pairs="${3:?custom PAIRS_CSV 필요}"
        duration="${4:-30s}"
        ;;
    *)
        echo "알 수 없는 시나리오: $scenario (low|mid|high|custom)" >&2
        exit 2
        ;;
esac

echo "─────────────────────────────────────────────────────────────"
echo "load-test [$scenario]"
echo "  rate=$rate/stream  pairs=$pairs  duration=$duration"
echo "  csv=$csv"
echo "─────────────────────────────────────────────────────────────"

# baseline 카운터 미리 캡처 — 종료 후 delta 계산 (선택).
before=$(curl -sS http://127.0.0.1:8082/v1/price-stats 2>/dev/null || echo "")
if [ -n "$before" ]; then
    echo "Before: $before"
fi

"$LOADGEN" \
    --rate "$rate" \
    --pairs "$pairs" \
    --duration "$duration" \
    --csv "$csv"

after=$(curl -sS http://127.0.0.1:8082/v1/price-stats 2>/dev/null || echo "")
if [ -n "$after" ]; then
    echo "After:  $after"
fi
