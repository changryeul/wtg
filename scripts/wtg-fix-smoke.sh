#!/usr/bin/env bash
# WTG mci-edge-fix e2e smoke — 카운터파티 등록 → SIGHUP reload → fix-tester Logon
# → ExecutionReport drop copy → stats 5단계 원샷 검증. --with-fix 로 부팅된
# 스택에서 반복 실행 가능 (idempotent).
#
# 사용 :
#   ./scripts/wtg-fix-smoke.sh                                # default ECN_SMOKE / smoke-pw
#   ./scripts/wtg-fix-smoke.sh ECN_MYTEST test-pw-2026
#   ./scripts/wtg-fix-smoke.sh ECN_MYTEST test-pw-2026 USD/KRW:buy:1000000:1378.55
#
# 환경변수 override :
#   ADMIN_URL=http://127.0.0.1:9090                # mci-admin base
#   FIX_STATS_URL=http://127.0.0.1:5002            # mci-edge-fix stats base
#   FIX_LISTEN=127.0.0.1:5001                      # mci-edge-fix FIX acceptor
#   ADMIN_USER=admin                               # DevMode X-WTG-User 값
#   PUSH_SECRET=dev-secret                         # mci-edge-fix --push-secret
#   CHANNEL=FIX  SITE=HQ  TIER=VIP                 # Profile 3-tuple
#   ORDER_ALIAS=                                   # 빈값=FIX_NEW_ORDER default
#   WAIT_SEC=12                                    # fix-tester 대기 (drop-copy 도착 여유)
#
# 종료 코드 :
#   0 = 전 단계 통과
#   1 = 사전 조건 실패 (스택 미기동 / 바이너리 없음)
#   2 = 카운터파티 등록 실패
#   3 = SIGHUP reload 실패 (mci-edge-fix pid 못 찾음 등)
#   4 = fix-tester LOGON 실패
#   5 = drop-copy POST 실패
#   6 = ExecutionReport 수신 실패 (tester 가 35=8 못 봄)

set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
cd "$REPO"

CID="${1:-ECN_SMOKE}"
PW="${2:-smoke-pw}"
SEND_ORDER="${3:-}"

ADMIN_URL="${ADMIN_URL:-http://127.0.0.1:9090}"
FIX_STATS_URL="${FIX_STATS_URL:-http://127.0.0.1:5002}"
FIX_LISTEN="${FIX_LISTEN:-127.0.0.1:5001}"
ADMIN_USER="${ADMIN_USER:-admin}"
PUSH_SECRET="${PUSH_SECRET:-dev-secret}"
CHANNEL="${CHANNEL:-FIX}"
SITE="${SITE:-HQ}"
TIER="${TIER:-VIP}"
ORDER_ALIAS="${ORDER_ALIAS:-}"
WAIT_SEC="${WAIT_SEC:-12}"

# 색상 (TTY 일 때만).
if [ -t 1 ]; then
  GRN=$'\033[32m'; RED=$'\033[31m'; YLW=$'\033[33m'; DIM=$'\033[2m'; RST=$'\033[0m'
else
  GRN=""; RED=""; YLW=""; DIM=""; RST=""
fi

step() { echo; echo "${YLW}==> $*${RST}"; }
ok()   { echo "  ${GRN}✓${RST} $*"; }
fail() { echo "  ${RED}✗${RST} $*"; }

# ── 사전 조건 검증 ───────────────────────────────────────────────
step "사전 조건 확인"
if [ ! -x ./build/bin/fix-tester ]; then
  fail "./build/bin/fix-tester 없음 — make build 후 재실행"
  exit 1
fi
if ! curl -sSf -o /dev/null "${ADMIN_URL}/" 2>/dev/null; then
  fail "mci-admin 응답 없음 (${ADMIN_URL}) — ./scripts/wtg-stack-up.sh --with-fix"
  exit 1
fi
if ! curl -sSf -o /dev/null "${FIX_STATS_URL}/stats" 2>/dev/null; then
  fail "mci-edge-fix stats 응답 없음 (${FIX_STATS_URL}) — --with-fix 미부팅?"
  exit 1
fi
FIX_PID=$(pgrep -f "build/bin/mci-edge-fix" | head -1 || true)
if [ -z "$FIX_PID" ]; then
  fail "mci-edge-fix 프로세스 못 찾음"
  exit 1
fi
ok "admin=${ADMIN_URL} edge-fix pid=${FIX_PID} FIX=${FIX_LISTEN}"

# ── 1. 카운터파티 등록 ──────────────────────────────────────────
step "1. 카운터파티 PUT — ${CID}"
BODY=$(cat <<EOF
{"password":"${PW}","channel":"${CHANNEL}","site":"${SITE}","tier":"${TIER}","usid":"${CID}","order_alias":"${ORDER_ALIAS}"}
EOF
)
HTTP=$(curl -sS -o /tmp/wtg-fix-smoke.put.out -w "%{http_code}" \
  -H "X-WTG-User: ${ADMIN_USER}" -H "Content-Type: application/json" \
  -X PUT "${ADMIN_URL}/v1/admin/fix-counterparties/${CID}" -d "$BODY" || echo "000")
if [ "$HTTP" != "200" ]; then
  fail "PUT 실패 HTTP=${HTTP} body=$(cat /tmp/wtg-fix-smoke.put.out)"
  exit 2
fi
ok "PUT OK — $(cat /tmp/wtg-fix-smoke.put.out)"

# ── 2. SIGHUP reload ────────────────────────────────────────────
step "2. SIGHUP reload — pid=${FIX_PID}"
if ! kill -HUP "$FIX_PID" 2>/dev/null; then
  fail "SIGHUP 전송 실패"
  exit 3
fi
sleep 2
if ! grep -q "mci-edge-fix reload" logs/mci-edge-fix.log 2>/dev/null; then
  fail "reload 로그 없음 (logs/mci-edge-fix.log 확인)"
  exit 3
fi
RELOAD_LINE=$(grep "mci-edge-fix reload" logs/mci-edge-fix.log | tail -1)
ok "reload — ${DIM}${RELOAD_LINE}${RST}"

# ── 3. fix-tester Logon (백그라운드) + 4. drop-copy 병행 ─────────
step "3. fix-tester Logon 시작 (백그라운드, wait=${WAIT_SEC}s)"
TESTER_OUT=$(mktemp)
TESTER_ARGS=(
  --target "$FIX_LISTEN"
  --sender "$CID"
  --target-comp WTG
  --password "$PW"
  --wait "${WAIT_SEC}s"
)
[ -n "$SEND_ORDER" ] && TESTER_ARGS+=(--send-order "$SEND_ORDER")

./build/bin/fix-tester "${TESTER_ARGS[@]}" > "$TESTER_OUT" 2>&1 &
TESTER_PID=$!
sleep 3
if ! grep -q "LOGON OK" "$TESTER_OUT" 2>/dev/null; then
  fail "LOGON 3초 안에 못 통과 — tester 출력:"
  sed 's/^/    /' "$TESTER_OUT"
  # tester 강제 종료
  kill "$TESTER_PID" 2>/dev/null || true
  wait "$TESTER_PID" 2>/dev/null || true
  rm -f "$TESTER_OUT"
  exit 4
fi
ok "LOGON OK 확인"

step "4. ExecutionReport drop copy POST"
DC_BODY=$(cat <<EOF
{"target_sender_comp_id":"${CID}","order_id":"ORD-SMOKE-$$","exec_id":"EXEC-$$","exec_type":"2","ord_status":"2","side":"buy","symbol":"USD/KRW","last_qty":1000000,"last_px":1378.55}
EOF
)
DC_HTTP=$(curl -sS -o /tmp/wtg-fix-smoke.dc.out -w "%{http_code}" \
  -H "X-Push-Secret: ${PUSH_SECRET}" -H "Content-Type: application/json" \
  -X POST "${FIX_STATS_URL}/v1/internal/exec-report" -d "$DC_BODY" || echo "000")
if [ "$DC_HTTP" != "200" ]; then
  fail "drop-copy POST 실패 HTTP=${DC_HTTP} body=$(cat /tmp/wtg-fix-smoke.dc.out)"
  kill "$TESTER_PID" 2>/dev/null || true
  wait "$TESTER_PID" 2>/dev/null || true
  rm -f "$TESTER_OUT"
  exit 5
fi
ok "drop-copy POST OK — $(cat /tmp/wtg-fix-smoke.dc.out)"

# fix-tester 세션 종료 대기.
wait "$TESTER_PID" || true

# ── 5. 수신 확인 + stats ────────────────────────────────────────
step "5. fix-tester 결과 확인"
if ! grep -q "EXEC_REPORT" "$TESTER_OUT" 2>/dev/null; then
  fail "tester 가 35=8 못 받음 — 출력:"
  sed 's/^/    /' "$TESTER_OUT"
  rm -f "$TESTER_OUT"
  exit 6
fi
grep -E "LOGON OK|EXEC_REPORT|NewOrderSingle|recv_count" "$TESTER_OUT" | sed 's/^/    /'
ok "ExecutionReport 수신 확인"

step "stats 요약"
curl -sS "${FIX_STATS_URL}/stats" | python3 -m json.tool

rm -f "$TESTER_OUT" /tmp/wtg-fix-smoke.put.out /tmp/wtg-fix-smoke.dc.out
echo
echo "${GRN}✅ e2e smoke PASS — ${CID}${RST}"
