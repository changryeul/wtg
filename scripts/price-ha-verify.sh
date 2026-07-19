#!/usr/bin/env bash
# price-ha-verify.sh — 시세 gRPC-only HA e2e 자동 검증 (docs/price-ha-grpc.md).
#
# 1 forwarder(hub) → 2 mci-price(dial-in 구독) Active-Active 를 실 바이너리로 검증:
#
#   mock-lp ─UDP─▶ forwarder(--publish-mode hub) ─┬─SubscribeTicks─▶ mci-price A
#                                                   └─SubscribeTicks─▶ mci-price B
#
# 검증 항목:
#   [1] Active-Active 결정성 — A/B 두 인스턴스가 같은 BEST 산출 (bit-identical)
#   [2] warm-up gate       — tick 수신 + warmup 경과 후 /v1/ready 200
#   [3] failover           — A 를 죽여도 B 가 계속 BEST 서빙 (full 스트림 보유)
#
# dedup(dual-active forwarder)는 단위 테스트(tick_dedup_test)로 커버.
#
# 사용: scripts/price-ha-verify.sh   (repo 루트에서)
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=build/bin
HUB=127.0.0.1:15060      # forwarder TickIngestService
A_HTTP=18082; A_GRPC=127.0.0.1:15051
B_HTTP=18092; B_GRPC=127.0.0.1:15052
UDP_SMB=40044; UDP_KMB=40045
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

wait_ping() { # http_port
  for _ in $(seq 1 50); do
    curl -sf "http://127.0.0.1:$1/v1/ping" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "FAIL: :$1 부팅 실패"; return 1
}
best() { # http_port → "bid ask" (USDKRW)
  curl -s "http://127.0.0.1:$1/v1/best-stats" 2>/dev/null \
    | jq -r '.symbols.USDKRW | "\(.best_bid) \(.best_ask)"' 2>/dev/null
}

echo "==> forwarder (hub 모드, tick-listen $HUB)"
$BIN/quote-forwarder --publish-mode hub --tick-listen "$HUB" \
  --multi "SMB:$UDP_SMB,KMB:$UDP_KMB" --bind 127.0.0.1 --metrics 127.0.0.1:19091 >"$TMP/fwd.log" 2>&1 &
PIDS+=($!); sleep 1

echo "==> mci-price A (:$A_HTTP) + B (:$B_HTTP) — 둘 다 hub dial-in, warmup 1s"
for spec in "A $A_HTTP $A_GRPC" "B $B_HTTP $B_GRPC"; do
  set -- $spec
  $BIN/mci-price --no-broker --dev --listen ":$2" --grpc "$3" \
    --tick-source "$HUB" --symbols etc/symbols.json --warmup 1 --warmup-max 10 \
    >"$TMP/price-$1.log" 2>&1 &
  PIDS+=($!)
done
wait_ping "$A_HTTP"; wait_ping "$B_HTTP"

echo "==> mock-lp 송신 (BEST 기대 1380.10 / 1380.20)"
for _ in $(seq 1 8); do
  $BIN/mock-lp --feeds "SMB:127.0.0.1:$UDP_SMB,KMB:127.0.0.1:$UDP_KMB" --once >/dev/null
  sleep 0.2
done
sleep 1.5

FAILED=0
echo
echo "==> [1] Active-Active 결정성 (A == B)"
BA=$(best "$A_HTTP"); BB=$(best "$B_HTTP")
echo "     A BEST = $BA"
echo "     B BEST = $BB"
if [ -n "$BA" ] && [ "$BA" = "$BB" ] && [ "$BA" = "1380.1 1380.2" ]; then
  echo "  ✓ 두 인스턴스 BEST 동일 + 기대값 일치"
else
  echo "  ✗ 불일치 (A=$BA B=$BB, want '1380.1 1380.2')"; FAILED=1
fi

echo "==> [2] warm-up gate — 둘 다 /v1/ready 200"
RA=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$A_HTTP/v1/ready")
RB=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$B_HTTP/v1/ready")
if [ "$RA" = "200" ] && [ "$RB" = "200" ]; then
  echo "  ✓ A ready=$RA, B ready=$RB"
else
  echo "  ✗ A ready=$RA, B ready=$RB (want 200/200)"; FAILED=1
fi

echo "==> [3] failover — A 종료 후 B 계속 서빙"
# A 의 PID 는 PIDS[1] (forwarder=0, A=1, B=2).
kill "${PIDS[1]}" 2>/dev/null || true
sleep 0.5
for _ in $(seq 1 5); do
  $BIN/mock-lp --feeds "SMB:127.0.0.1:$UDP_SMB,KMB:127.0.0.1:$UDP_KMB" --once >/dev/null
  sleep 0.2
done
sleep 1
if ! curl -sf "http://127.0.0.1:$A_HTTP/v1/ping" >/dev/null 2>&1; then
  echo "  ✓ A 종료 확인"
else
  echo "  ✗ A 가 아직 살아있음"; FAILED=1
fi
BB2=$(best "$B_HTTP")
if [ "$BB2" = "1380.1 1380.2" ]; then
  echo "  ✓ B 계속 BEST 서빙 = $BB2 (A 죽어도 무중단)"
else
  echo "  ✗ B BEST = $BB2 (failover 후 서빙 실패)"; FAILED=1
fi

echo
if [ "$FAILED" = 0 ]; then
  echo "✅ price HA e2e 통과 (Active-Active 결정성 + warm-up + failover)"
else
  echo "❌ 검증 실패 — 위 ✗ 확인. 로그: $TMP (cleanup 됨)"; exit 1
fi
