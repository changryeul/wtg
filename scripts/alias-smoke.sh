#!/usr/bin/env bash
# alias-smoke.sh — 모든 active alias 를 mci-api 통해 호출 + 응답 검증.
#
# 운영 배포 전 smoke test, alias 카탈로그 health check, AP 도달성 검증에 사용.
#
# 사용:
#   ./scripts/alias-smoke.sh                  # DevMode (X-WTG-User=smoke)
#   USER=trader01 ./scripts/alias-smoke.sh    # 다른 user 지정
#   API=http://10.0.0.20:8080 ./scripts/alias-smoke.sh    # remote mci-api
#   FAIL_FAST=1 ./scripts/alias-smoke.sh      # 첫 실패에 즉시 종료
#
# 결과: pass/fail 별 alias 목록 + latency 표 + exit code (실패 시 1).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"

API="${API:-http://127.0.0.1:8080}"
ADMIN="${ADMIN:-http://127.0.0.1:9090}"
USER="${USER:-smoke}"
TIMEOUT="${TIMEOUT:-5}"
FAIL_FAST="${FAIL_FAST:-0}"

# 라우팅 룰 조회 — mci-admin /v1/admin/routes 가 단일 진실 (env 등)
ROUTES_JSON=$(curl -sS --max-time "$TIMEOUT" -H "X-WTG-User: $USER" \
              "$ADMIN/v1/admin/routes" 2>/dev/null)
if [ -z "$ROUTES_JSON" ]; then
    echo "ERR: mci-admin :9090 응답 없음 — 기동 확인" >&2
    exit 2
fi

# active alias 목록 추출
ACTIVE_ALIASES=$(echo "$ROUTES_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for r in d.get('rules', []):
    if r.get('active'):
        print(r['alias'])
")

if [ -z "$ACTIVE_ALIASES" ]; then
    echo "active alias 0개 — mci-admin /v1/admin/routes 확인" >&2
    exit 2
fi

count=$(echo "$ACTIVE_ALIASES" | wc -l | tr -d ' ')
echo "════════════════════════════════════════"
echo "alias smoke — $count active alias 호출 시작"
echo "API   : $API"
echo "USER  : $USER"
echo "════════════════════════════════════════"

pass=0
fail=0
declare -a PASS_LIST
declare -a FAIL_LIST

while IFS= read -r alias; do
    body="{\"alias\":\"$alias\",\"data\":\"smoke\"}"
    t0=$(python3 -c 'import time; print(int(time.time()*1000))')
    resp=$(curl -sS --max-time "$TIMEOUT" \
                -X POST "$API/v1/tx" \
                -H "Content-Type: application/json" \
                -H "X-WTG-User: $USER" \
                -d "$body" 2>&1) || resp="__curl_err__"
    t1=$(python3 -c 'import time; print(int(time.time()*1000))')
    ms=$((t1 - t0))

    # 에러 분류:
    #   - curl_err               : 네트워크 / 5xx
    #   - JSON 본문에 errn != 0  : 비즈니스 거부 (정상 — broker 통한 거 자체는 OK)
    #   - JSON 본문에 errn == 0  : 성공
    #   - "unknown_alias"        : 라우팅 룰이 mci-api 한테 안 전파됨 (실패)
    if [ "$resp" = "__curl_err__" ]; then
        result="FAIL_NET"
        fail=$((fail+1))
        FAIL_LIST+=("$alias: 네트워크/timeout (${ms}ms)")
    elif echo "$resp" | grep -q '"code":"unknown_alias"'; then
        result="FAIL_UNKNOWN"
        fail=$((fail+1))
        FAIL_LIST+=("$alias: unknown_alias (mci-api 에 룰 미전파)")
    elif echo "$resp" | grep -qE '"errn":\s*[1-9]'; then
        # broker 도달 했지만 매매 엔진이 비즈니스 거부 — smoke 관점에선 OK
        # (alias 자체는 작동, 페이로드가 적합하지 않을 뿐)
        result="PASS_BIZ_REJECT"
        pass=$((pass+1))
        PASS_LIST+=("$alias: ${ms}ms (broker 도달, biz reject)")
    else
        result="PASS"
        pass=$((pass+1))
        PASS_LIST+=("$alias: ${ms}ms")
    fi

    printf "  %-12s %-18s %6dms\n" "$result" "$alias" "$ms"

    if [ "$FAIL_FAST" = "1" ] && [[ "$result" == FAIL_* ]]; then
        echo
        echo "FAIL_FAST=1 — 종료"
        exit 1
    fi
done <<< "$ACTIVE_ALIASES"

echo
echo "════════ 요약 ════════"
echo "PASS: $pass / FAIL: $fail / 총 $count"
echo

if [ $fail -gt 0 ]; then
    echo "─ FAIL ─"
    printf "  %s\n" "${FAIL_LIST[@]}"
    exit 1
fi

# mci-api /v1/admin/alias-stats 로 누적 확인 (선택)
stats=$(curl -sS --max-time "$TIMEOUT" -H "X-WTG-User: $USER" \
        "$API/v1/admin/alias-stats" 2>/dev/null || true)
if [ -n "$stats" ]; then
    echo "─ mci-api alias 누적 stats (현 인스턴스) ─"
    echo "$stats" | python3 -c "
import json, sys
try: d = json.load(sys.stdin)
except: sys.exit(0)
for a in d.get('aliases', [])[:10]:
    print(f\"  {a['alias']:<18} calls={a['calls']:>5} err={a['errors']:>4} avg={a['avg_latency_ms']:>6.1f}ms max={a['max_latency_ms']:>6.1f}ms\")
"
fi
