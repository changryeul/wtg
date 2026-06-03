#!/usr/bin/env bash
# load-test-ratelimit.sh — mci-edge-api 의 rate limit 룰을 강제 발화시키는 부하 시나리오.
#
# 사용:
#   ./scripts/load-test-ratelimit.sh login   # POST /v1/login brute force 시나리오
#   ./scripts/load-test-ratelimit.sh tx      # POST /v1/tx 봇 시나리오
#   ./scripts/load-test-ratelimit.sh mixed   # 두 path 동시 + ping (정상) 섞어서
#
# 사전 조건:
#   - mci-edge-api 가 :8090 listen
#   - Prometheus 가 8090/metrics scrape
#   - (optional) etcd 정책 PUT 됨 (정적 default 도 OK)
#
# 결과:
#   - 200 / 429 카운트 + 거부율
#   - metrics endpoint 의 wtg_ratelimit_{allowed,denied}_total before/after delta
#   - Grafana alert state 변화는 별도 확인 (브라우저 또는 API)

set -euo pipefail

EDGE_URL="${EDGE_URL:-http://127.0.0.1:8090}"
METRICS_URL="${METRICS_URL:-${EDGE_URL}/metrics}"
SCENARIO="${1:-}"

if [ -z "$SCENARIO" ]; then
    echo "사용법: $0 {login|tx|mixed}" >&2
    exit 2
fi

# rate limit metric 의 합산 (kind 무관) 캡처.
capture_metric() {
    curl -sS "$METRICS_URL" 2>/dev/null | awk -F' ' '
        /^wtg_ratelimit_allowed_total/ { a += $2 }
        /^wtg_ratelimit_denied_total/  { d += $2 }
        END { printf "%d %d\n", a+0, d+0 }
    '
}

# 단일 path 에 N번 빠른 POST (또는 GET).
flood() {
    local method=$1 path=$2 count=$3 user=$4
    local got200=0 got429=0 other=0
    local body='{}'
    for i in $(seq 1 "$count"); do
        # -o /dev/null 로 본문 버림, -w 로 status code 만.
        local code
        if [ "$method" = "POST" ]; then
            code=$(curl -sS -o /dev/null -w "%{http_code}" -X POST \
                -H "Content-Type: application/json" \
                -H "X-WTG-User: $user" \
                --data "$body" \
                "$EDGE_URL$path" || echo "000")
        else
            code=$(curl -sS -o /dev/null -w "%{http_code}" \
                -H "X-WTG-User: $user" \
                "$EDGE_URL$path" || echo "000")
        fi
        case "$code" in
            200) got200=$((got200+1)) ;;
            429) got429=$((got429+1)) ;;
            *)   other=$((other+1)) ;;
        esac
    done
    printf "  %-30s 200=%-4d 429=%-4d other=%-3d (user=%s)\n" \
        "$method $path" "$got200" "$got429" "$other" "$user"
}

echo "─────────────────────────────────────────────────────────────"
echo "load-test-ratelimit [$SCENARIO]  target=$EDGE_URL"
echo "─────────────────────────────────────────────────────────────"

read -r a_before d_before <<<"$(capture_metric)"
printf "Before — allowed=%d denied=%d\n" "$a_before" "$d_before"

case "$SCENARIO" in
    login)
        # POST /v1/login default 한도: rate=5/s burst=10.
        # 한 user 가 빠르게 30번 → 처음 10 통과, 이후 약 5/s 보충, 나머지 거부.
        echo "▶ POST /v1/login × 30 (한 user) — burst 10 후 ~80% 거부 예상"
        flood POST /v1/login 30 "attacker-001"
        ;;
    tx)
        # POST /v1/tx default: rate=50/s burst=100. 한 user 200 호출.
        echo "▶ POST /v1/tx × 200 (한 user) — burst 100 후 50% 거부 예상"
        flood POST /v1/tx 200 "bot-007"
        ;;
    mixed)
        # 정상 + 공격 섞기 — 정상 사용자가 영향 받지 않는지 검증.
        echo "▶ 정상 사용자 alice — GET /v1/ping × 50 (한도 1000/s 안)"
        flood GET /v1/ping 50 "alice"
        echo "▶ 공격자 attacker-001 — POST /v1/login × 30"
        flood POST /v1/login 30 "attacker-001"
        echo "▶ 정상 사용자 alice — GET /v1/ping × 50 (여전히 통과해야)"
        flood GET /v1/ping 50 "alice"
        ;;
    *)
        echo "알 수 없는 시나리오: $SCENARIO" >&2
        exit 2
        ;;
esac

# metrics 안정화 대기 (Prometheus scrape 주기).
sleep 1
read -r a_after d_after <<<"$(capture_metric)"
printf "After  — allowed=%d denied=%d  (Δ allowed=%d, Δ denied=%d)\n" \
    "$a_after" "$d_after" \
    "$((a_after-a_before))" "$((d_after-d_before))"

# Grafana alert state 확인 안내 — 정확한 endpoint 는 환경에 따라.
cat <<EOF

다음으로 Grafana 알람 발화 확인:
  curl -s -u admin:admin http://127.0.0.1:3030/api/prometheus/grafana/api/v1/rules \\
    | python3 -c "import json,sys; d=json.load(sys.stdin); \\
        [print(r['name'], '—', r['state']) for g in d['data']['groups'] \\
         for r in g['rules'] if 'ratelimit' in g['name']]"

또는 브라우저: http://127.0.0.1:3030/alerting/list
EOF
