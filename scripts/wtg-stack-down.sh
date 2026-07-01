#!/usr/bin/env bash
# WTG dev 스택 일괄 종료 — wtg-stack-up.sh 가 띄운 모든 서비스 + docker broker.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"

echo "==> WTG host 서비스 종료"
for svc in mci-admin mci-api mci-edge-price mci-edge-fix mci-edge-md mci-chart quote-forwarder mci-price prometheus; do
  pids=$(pgrep -f "build/bin/$svc" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    echo "    $svc  pid=$pids"
    kill $pids 2>/dev/null || true
  fi
done
pkill -f "wtg-dev-tickloop.py" 2>/dev/null && echo "    tickloop 종료"
pkill -f "build/bin/load-gen" 2>/dev/null && echo "    load-gen 종료"

echo
echo "==> docker broker 종료"
if docker info >/dev/null 2>&1; then
  if docker ps --format '{{.Names}}' | grep -q '^mymqd$'; then
    docker rm -f mymqd >/dev/null 2>&1 && echo "    mymqd 컨테이너 제거"
  else
    echo "    mymqd 컨테이너 없음"
  fi
else
  echo "    (docker daemon 없음 — broker skip)"
fi

sleep 1
echo
echo "==> 남은 프로세스 검사"
remain=$(ps aux | grep -E "mci-(admin|api|price|edge-price|chart|push)|quote-forwarder|wtg-dev-tickloop|build/bin/load-gen" | grep -v grep | awk '{print "    "$2"  "$11" "$12}')
[ -n "$remain" ] && echo "$remain" || echo "    (없음)"
docker ps --format '{{.Names}} {{.Image}}' 2>/dev/null | grep -E "mymqd|wtg-" | sed 's/^/    docker: /' || true
