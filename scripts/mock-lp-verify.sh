#!/usr/bin/env bash
# mock-lp-verify.sh — mock LP 시세 경로 결정적 e2e 검증.
#
# broker/etcd 없이 최소 스택을 띄우고, mock-lp 로 알려진 시나리오를 UDP FIX 로
# 쏜 뒤, AlgoStream(SubscribeAlgo) 수신값을 기대값과 대사한다:
#
#   mock-lp ──UDP FIX──▶ quote-forwarder ──gRPC──▶ mci-price ──▶ algo-tester
#            (SMB/KMB 결정적 호가+체결)   (--publish-mode grpc)   (AlgoQuote JSON)
#
# 검증 항목 (결정적):
#   - BEST 모드: bid=max(SMB,KMB bid), ask=min(SMB,KMB ask), mid=(bid+ask)/2,
#     last=체결가, source=BEST
#   - per-source(SMB) 모드: 그 원천의 raw 호가 그대로, source=SMB
#
# cross(CNH/KRW)는 etcd PairMaster formula 의존이라 로컬 etcd 바이너리가 없으면
# shell 로 띄우기 어렵다 → embedded etcd 통합 테스트로 커버:
#   go test -tags integration ./internal/price/ -run TestMockLP_CrossE2E
# (mds worse-side div 산식과 값 일치까지 결정적 검증)
#
# 사용: scripts/mock-lp-verify.sh   (repo 루트에서)
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=build/bin
HTTP_PORT=18082
GRPC_PORT=15051
UDP_SMB=40044
UDP_KMB=40045
TMP=$(mktemp -d)
PIDS=()

cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "==> 빌드"
CGO_ENABLED=0 make build >/dev/null

# ── 기대값 (mock-lp 시나리오와 일치) ──
SMB_BID=1380.10; SMB_ASK=1380.25; SMB_LAST=1380.15
KMB_BID=1380.05; KMB_ASK=1380.20
BEST_BID=1380.10   # max(SMB,KMB bid)
BEST_ASK=1380.20   # min(SMB,KMB ask)
BEST_MID=1380.15   # (best_bid+best_ask)/2
BEST_LAST=1380.15  # SMB 체결가 persist

cat > "$TMP/scn.json" <<JSON
{"quotes":[
  {"lp":"SMB","pair":"USDKRW","bid":$SMB_BID,"ask":$SMB_ASK,"last":$SMB_LAST,"last_qty":500000},
  {"lp":"KMB","pair":"USDKRW","bid":$KMB_BID,"ask":$KMB_ASK}
]}
JSON

echo "==> mci-price 기동 (--no-broker, gRPC :$GRPC_PORT, algo-stream)"
$BIN/mci-price --no-broker --dev --listen ":$HTTP_PORT" \
  --grpc "127.0.0.1:$GRPC_PORT" --algo-stream >"$TMP/price.log" 2>&1 &
PIDS+=($!)

# /v1/ping 대기
for i in $(seq 1 50); do
  curl -sf "http://127.0.0.1:$HTTP_PORT/v1/ping" >/dev/null 2>&1 && break
  sleep 0.2
  [ "$i" = 50 ] && { echo "FAIL: mci-price 부팅 실패"; cat "$TMP/price.log"; exit 1; }
done

echo "==> quote-forwarder 기동 (--publish-mode grpc, --multi SMB:$UDP_SMB,KMB:$UDP_KMB)"
$BIN/quote-forwarder --publish-mode grpc --price-grpc "127.0.0.1:$GRPC_PORT" \
  --multi "SMB:$UDP_SMB,KMB:$UDP_KMB" --bind 127.0.0.1 >"$TMP/fwd.log" 2>&1 &
PIDS+=($!)
sleep 1

# ── assert 헬퍼 (부동소수 허용오차 1e-4) ──
FAILED=0
assert_eq() { # name got want
  local name="$1" got="$2" want="$3"
  if awk -v a="$got" -v b="$want" 'BEGIN{d=a-b; if(d<0)d=-d; exit !(d<1e-4)}'; then
    echo "  ✓ $name = $got"
  else
    echo "  ✗ $name = $got (want $want)"; FAILED=1
  fi
}

run_algo() { # sources → 마지막 AlgoQuote JSON 라인
  local sources="$1"
  # algo-tester 를 백그라운드로 띄워 구독 후, mock-lp 를 반복 송신 (구독 시점 이후 tick 수신 보장).
  ( $BIN/algo-tester --target "127.0.0.1:$GRPC_PORT" --symbols USDKRW \
      --sources "$sources" --json --duration 2s >"$TMP/algo.out" 2>/dev/null ) &
  local ap=$!
  sleep 0.4
  for _ in $(seq 1 8); do
    $BIN/mock-lp --feeds "SMB:127.0.0.1:$UDP_SMB,KMB:127.0.0.1:$UDP_KMB" \
      --scenario "$TMP/scn.json" --once >/dev/null
    sleep 0.15
  done
  wait $ap 2>/dev/null || true
  grep '^{' "$TMP/algo.out" | tail -1
}

echo "==> [1] BEST 모드 검증"
LINE=$(run_algo "")
[ -z "$LINE" ] && { echo "FAIL: BEST tick 수신 없음"; cat "$TMP/price.log" "$TMP/fwd.log"; exit 1; }
echo "     recv: $LINE"
assert_eq "BEST source" "$(echo "$LINE" | jq -r .source)" "BEST" || true
assert_eq "BEST bid"    "$(echo "$LINE" | jq .bid)"       "$BEST_BID"
assert_eq "BEST ask"    "$(echo "$LINE" | jq .ask)"       "$BEST_ASK"
assert_eq "BEST mid"    "$(echo "$LINE" | jq .mid)"       "$BEST_MID"
assert_eq "BEST last"   "$(echo "$LINE" | jq .last)"      "$BEST_LAST"

echo "==> [2] per-source(SMB) 모드 검증"
LINE=$(run_algo "SMB")
[ -z "$LINE" ] && { echo "FAIL: SMB tick 수신 없음"; exit 1; }
echo "     recv: $LINE"
SRC=$(echo "$LINE" | jq -r .source)
[ "$SRC" = "SMB" ] && echo "  ✓ source = SMB" || { echo "  ✗ source = $SRC (want SMB)"; FAILED=1; }
assert_eq "SMB bid" "$(echo "$LINE" | jq .bid)" "$SMB_BID"
assert_eq "SMB ask" "$(echo "$LINE" | jq .ask)" "$SMB_ASK"

echo
if [ "$FAILED" = 0 ]; then
  echo "✅ mock-lp e2e 검증 통과 (BEST + per-source)"
else
  echo "❌ 검증 실패 — 위 ✗ 항목 확인. 로그: $TMP (cleanup 됨)"; exit 1
fi
