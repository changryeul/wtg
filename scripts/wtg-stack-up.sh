#!/usr/bin/env bash
# WTG dev 스택 일괄 부팅 — mci-admin + mci-price + mci-edge-price + quote-forwarder + tickloop.
# 기존 scripts/dev-up.sh 는 TimescaleDB 컨테이너만 다룬다. 본 스크립트는 WTG 바이너리.
#
# 사용 :
#   ./scripts/wtg-stack-up.sh                  # 기본 (단순화 v3 — swap-lock off)
#   ./scripts/wtg-stack-up.sh --with-chart     # mci-chart 도 띄움 (TimescaleDB 필요)
#   ./scripts/wtg-stack-up.sh --with-forwarder # quote-forwarder 도 띄움
#   ./scripts/wtg-stack-up.sh --with-prom      # Prometheus + 운영 모니터링
#
# 환경변수 override :
#   LISTEN_ADMIN=:9090  LISTEN_PRICE=:8082  ...

set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
cd "$REPO"

# default flags
WITH_CHART=0
WITH_FORWARDER=0
WITH_PROM=0
for arg in "$@"; do
  case "$arg" in
    --with-chart)     WITH_CHART=1 ;;
    --with-forwarder) WITH_FORWARDER=1 ;;
    --with-prom)      WITH_PROM=1 ;;
    *) echo "unknown arg: $arg"; exit 1 ;;
  esac
done

mkdir -p logs

# 기존 서비스 종료 (idempotent)
for svc in mci-admin mci-price mci-edge-price quote-forwarder wtg-dev-tickloop prometheus mci-chart; do
  pkill -f "build/bin/$svc" 2>/dev/null || true
done
pkill -f "wtg-dev-tickloop.py" 2>/dev/null || true
sleep 1

# 빌드 (필요시)
if [ ! -x ./build/bin/mci-admin ]; then
  echo "==> bin 없음, make build 실행"
  make build >/dev/null 2>&1 || { echo "make build 실패"; exit 1; }
fi

start() {
  local name=$1; shift
  echo "==> $name 시작 :: $*"
  "$@" > "logs/$name.log" 2>&1 &
  echo "    pid=$!"
  sleep 1
}

# Prometheus (선택)
if [ "$WITH_PROM" = "1" ]; then
  if ! command -v prometheus >/dev/null 2>&1; then
    echo "(prometheus 미설치 — brew install prometheus)"
  else
    start prometheus prometheus \
      --config.file=logs/prometheus.yml \
      --storage.tsdb.path=logs/prom-data \
      --web.listen-address=127.0.0.1:9095 \
      --storage.tsdb.retention.time=1h
  fi
fi

# mci-price (단순화 v3 — swap-lock 끔, 5-Layer → 3-Layer)
start mci-price ./build/bin/mci-price \
  --dev --no-broker \
  --listen "${LISTEN_PRICE:-:8082}" --grpc "${GRPC_PRICE:-:50051}" \
  --symbols etc/symbols.json \
  --pricing etc/pricing.json \
  --profiles etc/profiles.json

# mci-edge-price (3 stream 활성)
start mci-edge-price ./build/bin/mci-edge-price \
  --dev --listen "${LISTEN_EDGE_PRICE:-:8083}" \
  --upstream "127.0.0.1${GRPC_PRICE:-:50051}" \
  --quote-stream --customer-stream

# quote-forwarder (선택)
if [ "$WITH_FORWARDER" = "1" ]; then
  start quote-forwarder ./build/bin/quote-forwarder \
    --listen 127.0.0.1:30044 \
    --metrics 127.0.0.1:9091 \
    --publish-mode grpc \
    --price-grpc "127.0.0.1${GRPC_PRICE:-:50051}"
fi

# mci-chart (선택)
if [ "$WITH_CHART" = "1" ]; then
  start mci-chart ./build/bin/mci-chart \
    --listen "${LISTEN_CHART:-:8086}" \
    --upstream "127.0.0.1${GRPC_PRICE:-:50051}" \
    --dsn "${CHART_DSN:-postgres://wtg:secret@localhost:5432/wtg?sslmode=disable}"
fi

# mci-admin (마지막에 — 다른 서비스 url 가져옴)
ADMIN_FLAGS=(--dev --no-broker --listen "${LISTEN_ADMIN:-:9090}")
[ "$WITH_PROM" = "1" ] && ADMIN_FLAGS+=(--prom-url http://127.0.0.1:9095)
start mci-admin ./build/bin/mci-admin "${ADMIN_FLAGS[@]}"

# tickloop (dev tick generator)
if [ -f /tmp/wtg-dev-tickloop.py ]; then
  echo "==> tickloop 시작"
  nohup python3 /tmp/wtg-dev-tickloop.py > logs/dev-tick.log 2>&1 &
  echo "    pid=$!"
fi

sleep 2
echo
echo "==> 부팅 완료. status :"
"$REPO/scripts/wtg-status.sh" 2>/dev/null || echo "    (wtg-status.sh 없음 — make 후 재실행)"
