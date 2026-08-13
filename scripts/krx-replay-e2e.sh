#!/usr/bin/env bash
# krx-replay-e2e.sh — mci-edge-krx 실 바이너리 e2e (장외에서도 가능한 재생 경로).
#
# gen(결정적 캡처) → replay(UDP 멀티캐스트) → mci-edge-krx --mcast → 파싱 → ws
# → krx-tester 수신 을 실 바이너리로 한 번에 검증. KRX 라이브 회선 불요.
# 로컬/EC2 어디서나 동작 (테스트용 그룹 239.9.9.9 사용 — 실 227.10.20.10 아님).
#
# 라이브(장중) 검증은 docs/krx-live-verify.md 경로 A 참고.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GROUP="${GROUP:-239.9.9.9}"
PORT="${PORT:-61099}"
LISTEN="${LISTEN:-127.0.0.1:18099}"
SYMS="${SYMS:-101V6000,KR1035020310,201S3000}"

cd "$ROOT"
echo "== 빌드"
go build -o build/bin/mci-edge-krx ./cmd/mci-edge-krx/
go build -o build/bin/krx-tester ./cmd/krx-verify/../krx-tester/
go build -o build/bin/krx-verify ./cmd/krx-verify/

CAP="$(mktemp)"; trap 'rm -f "$CAP"; [ -n "${EDGE:-}" ] && kill "$EDGE" 2>/dev/null || true' EXIT
./build/bin/krx-verify gen "$CAP" >/dev/null

echo "== mci-edge-krx --mcast ($GROUP:$PORT → $LISTEN)"
./build/bin/mci-edge-krx --mcast --mcast-group "$GROUP" --mcast-ports "$PORT" \
	--listen "$LISTEN" >/tmp/krx-e2e-edge.log 2>&1 &
EDGE=$!
sleep 0.7

echo "== krx-tester 구독 ($SYMS)"
RX="$(mktemp)"
./build/bin/krx-tester --url "ws://$LISTEN/v1/subscribe" --symbols "$SYMS" \
	--count 4 --timeout 8s >"$RX" 2>&1 &
TESTER=$!
sleep 0.4

echo "== 캡처 재생 (3회)"
./build/bin/krx-verify replay "$CAP" "$GROUP:$PORT" 3 40ms

if wait "$TESTER"; then
	echo "== 수신 결과"; cat "$RX"
	echo "[OK] 실 바이너리 e2e 통과 (gen→mcast→parse→ws→tester)"
else
	echo "== 수신 결과 (실패)"; cat "$RX"
	echo "[FAIL] 수신 0 — edge 로그:"; tail -5 /tmp/krx-e2e-edge.log
	rm -f "$RX"; exit 1
fi
rm -f "$RX"
