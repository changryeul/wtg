#!/usr/bin/env bash
# chaos-broker.sh — broker (mymqd) 를 잠시 죽였다 살려서 mci-* 의 reconnect
# 메트릭 + alert 발화를 라이브 검증.
#
# 사용:
#   ./scripts/chaos-broker.sh quick     # 10초 다운 + 메트릭 확인
#   ./scripts/chaos-broker.sh sustained # 60초 다운 + alert "for: 1m" 만족
#
# 사전 조건:
#   - mymqd docker container 실행 중 (wtg-broker 또는 mymqd)
#   - mci-price / mci-api 등 mci-* 서비스가 broker 에 연결
#   - Prometheus 가 mci-* /metrics scrape
#
# 결과:
#   - broker disconnect 직후 inflight_aborted spike
#   - downtime 동안 모든 reconnect 시도 실패 + backoff
#   - broker 복구 후 reconnect_duration_seconds histogram 갱신

set -euo pipefail

PROM_URL="${PROM_URL:-http://127.0.0.1:9095}"
GRAFANA_URL="${GRAFANA_URL:-http://127.0.0.1:3030}"
SCENARIO="${1:-}"

BROKER=$(docker ps --format '{{.Names}} {{.Image}}' | awk '/mymqd|wtg-broker|broker/ {print $1; exit}')

if [ -z "$SCENARIO" ]; then
    echo "사용법: $0 {quick|sustained}" >&2
    echo "  quick     — 10초 다운 + 즉시 검증"
    echo "  sustained — 60초 다운 + alert (for: 1m 만족) 발화 확인"
    exit 2
fi

if [ -z "$BROKER" ]; then
    echo "broker container 찾기 실패 (mymqd / wtg-broker 이름). docker ps 확인." >&2
    exit 1
fi

capture() {
    local label=$1
    echo "── $label ──"
    for metric in disconnects reconnects inflight_aborted heartbeat_timeout; do
        local q="sum(wtg_broker_${metric}_total)"
        local v
        v=$(curl -sS --get "$PROM_URL/api/v1/query" --data-urlencode "query=$q" 2>/dev/null \
            | python3 -c "import json,sys; d=json.load(sys.stdin); r=d['data']['result']; print(r[0]['value'][1] if r else '0')" 2>/dev/null)
        printf "  %-25s %s\n" "$metric" "${v:-0}"
    done
}

echo "─────────────────────────────────────────────────────────────"
echo "chaos-broker [$SCENARIO]  broker=$BROKER"
echo "─────────────────────────────────────────────────────────────"
capture "Before"

case "$SCENARIO" in
    quick) DOWN=10 ;;
    sustained) DOWN=60 ;;
    *) echo "알 수 없는 시나리오: $SCENARIO" >&2; exit 2 ;;
esac

echo
echo "▶ broker stop — ${DOWN}s downtime..."
docker stop "$BROKER" > /dev/null
START=$(date +%s)

while [ $(($(date +%s) - START)) -lt "$DOWN" ]; do
    sleep 5
    capture "During (t+$(($(date +%s) - START))s)"
done

echo
echo "▶ broker start — 재연결 관찰..."
docker start "$BROKER" > /dev/null

LAST_T=0
for t in 5 15 30; do
    sleep $((t - LAST_T))
    capture "After (t+${t}s)"
    LAST_T=$t
done

echo
echo "── Grafana broker alert state ──"
curl -sS -u admin:admin "$GRAFANA_URL/api/prometheus/grafana/api/v1/rules" 2>&1 | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    found = False
    for g in d.get('data',{}).get('groups',[]):
        if 'broker' not in g.get('name',''):
            continue
        for r in g.get('rules',[]):
            v = r.get('alerts',[{}])[0].get('value','') if r.get('alerts') else ''
            print(f\"  {r['name'][:55]:<55} state={r['state']:<10} value={v[:25]}\")
            found = True
    if not found:
        print('  broker alert 그룹 미등록 — Grafana provisioning 확인')
except Exception as e:
    print(f'  파싱 실패: {e}')
"

echo
echo "전체 시나리오 완료. broker 복구 후 1~2분 더 기다리면 alert 가 자동 해제됩니다."
