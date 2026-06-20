#!/usr/bin/env bash
# WTG dev 스택 일괄 종료 — wtg-stack-up.sh 가 띄운 서비스 모두.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"

echo "==> WTG 서비스 종료"
for svc in mci-admin mci-edge-price mci-chart quote-forwarder mci-price prometheus; do
  pids=$(pgrep -f "build/bin/$svc" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    echo "    $svc  pid=$pids"
    kill $pids 2>/dev/null || true
  fi
done
pkill -f "wtg-dev-tickloop.py" 2>/dev/null && echo "    tickloop 종료"

sleep 1
echo
echo "==> 남은 프로세스"
ps aux | grep -E "mci-(admin|price|edge-price|chart|push)|quote-forwarder|wtg-dev-tickloop|^/.*prometheus.*9095" | grep -v grep | awk '{print "    "$2"  "$11" "$12" "$13}' || echo "    (없음)"
