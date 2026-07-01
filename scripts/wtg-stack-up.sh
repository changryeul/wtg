#!/usr/bin/env bash
# WTG dev 스택 일괄 부팅 — mci-admin + mci-price + mci-edge-price + tickloop 기본.
# 기존 scripts/dev-up.sh 는 TimescaleDB 컨테이너만 다룬다. 본 스크립트는 WTG 바이너리 + (선택) docker broker.
#
# 사용 :
#   ./scripts/wtg-stack-up.sh                    # 기본 (단순화 v3 — swap-lock off)
#   ./scripts/wtg-stack-up.sh --with-chart       # mci-chart 도 띄움 (PostgreSQL 또는 TimescaleDB 필요)
#   ./scripts/wtg-stack-up.sh --with-forwarder   # quote-forwarder 도 띄움
#   ./scripts/wtg-stack-up.sh --with-prom        # Prometheus + 운영 모니터링
#   ./scripts/wtg-stack-up.sh --with-swap-lock   # mci-price 에 --enable-swap-lock 추가
#   ./scripts/wtg-stack-up.sh --with-broker      # docker mymqd (broker + test_service + WECHO) 같이
#   ./scripts/wtg-stack-up.sh --with-api         # mci-api 까지 (broker 필요 → --with-broker 자동)
#   ./scripts/wtg-stack-up.sh --with-fix         # mci-edge-fix (FIX 4.4 주문 DMZ gateway)
#   ./scripts/wtg-stack-up.sh --with-md          # mci-edge-md  (FIX 4.4 시세 DMZ gateway, Phase A skeleton)
#   ./scripts/wtg-stack-up.sh --with-all         # 모든 컴포넌트 (chart 제외 — DB 의존)
#
# 환경변수 override :
#   LISTEN_ADMIN=:9090  LISTEN_PRICE=:8082  LISTEN_API=:8080
#   CHART_DSN="postgres://winwaysystems@localhost/wtg?sslmode=disable"
#   BROKER_IMAGE=wtg-mymqd:arm64-trcid-fix    # 기본은 가장 최근 build 된 image 자동 선택
#   ETCD_URL=http://127.0.0.1:2379            # --with-fix 시 admin embedded etcd 대신 외부 etcd 지정

set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
cd "$REPO"

# default flags
WITH_CHART=0
WITH_FORWARDER=0
WITH_PROM=0
WITH_SWAP_LOCK=0
WITH_BROKER=0
WITH_API=0
WITH_FIX=0
WITH_MD=0
for arg in "$@"; do
  case "$arg" in
    --with-chart)     WITH_CHART=1 ;;
    --with-forwarder) WITH_FORWARDER=1 ;;
    --with-prom)      WITH_PROM=1 ;;
    --with-swap-lock) WITH_SWAP_LOCK=1 ;;
    --with-broker)    WITH_BROKER=1 ;;
    --with-api)       WITH_API=1; WITH_BROKER=1 ;;     # api 는 broker 필수
    --with-fix)       WITH_FIX=1 ;;
    --with-md)        WITH_MD=1 ;;
    --with-all)       WITH_FORWARDER=1; WITH_PROM=1; WITH_SWAP_LOCK=1; WITH_BROKER=1; WITH_API=1; WITH_FIX=1; WITH_MD=1 ;;
    *) echo "unknown arg: $arg"; exit 1 ;;
  esac
done

mkdir -p logs

# 기존 host 서비스 종료 (idempotent)
for svc in mci-admin mci-api mci-price mci-edge-price mci-edge-fix mci-edge-md quote-forwarder wtg-dev-tickloop prometheus mci-chart; do
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

# (선택) docker mymqd — broker + test_service + WECHO
if [ "$WITH_BROKER" = "1" ]; then
  if ! docker info >/dev/null 2>&1; then
    echo "==> docker daemon 안 떠 있음 — broker skip (open -a Docker 후 재시도)"
  else
    # 기존 컨테이너 정리
    docker rm -f mymqd >/dev/null 2>&1 || true
    # image 결정 — env override 가장 우선, 없으면 wtg-mymqd 계열 중 가장 최근
    IMAGE="${BROKER_IMAGE:-$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^wtg-mymqd:' | head -1)}"
    if [ -z "$IMAGE" ]; then
      echo "==> wtg-mymqd image 없음 — cd ~/mywork/mymq && docker build -f scripts/Dockerfile.runtime -t wtg-mymqd ."
    else
      echo "==> mymqd 컨테이너 띄움 :: $IMAGE"
      docker run --rm -d -p 11217:11217 --name mymqd "$IMAGE" >/dev/null
      # broker port ready 까지 polling
      for i in {1..15}; do
        nc -z 127.0.0.1 11217 2>/dev/null && { echo "    broker ready (${i}회차)"; break; }
        sleep 1
      done
    fi
  fi
fi

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

# mci-price (단순화 v3 — 5-Layer → 3-Layer. swap-lock 은 --with-swap-lock 옵션)
PRICE_ARGS=(--dev --no-broker \
  --listen "${LISTEN_PRICE:-:8082}" --grpc "${GRPC_PRICE:-:50051}" \
  --symbols etc/symbols.json \
  --pricing etc/pricing.json \
  --profiles etc/profiles.json)
[ "$WITH_SWAP_LOCK" = "1" ] && PRICE_ARGS+=(--enable-swap-lock)
start mci-price ./build/bin/mci-price "${PRICE_ARGS[@]}"

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

# mci-chart (선택) — default DSN 은 brew postgres 기준
if [ "$WITH_CHART" = "1" ]; then
  start mci-chart ./build/bin/mci-chart \
    --listen "${LISTEN_CHART:-:8086}" \
    --upstream "127.0.0.1${GRPC_PRICE:-:50051}" \
    --dsn "${CHART_DSN:-postgres://$(whoami)@localhost/wtg?sslmode=disable}"
fi

# mci-admin (마지막에 — 다른 서비스 url 가져옴)
ADMIN_FLAGS=(--dev --no-broker --listen "${LISTEN_ADMIN:-:9090}")
[ "$WITH_PROM" = "1" ] && ADMIN_FLAGS+=(--prom-url http://127.0.0.1:9095)
start mci-admin ./build/bin/mci-admin "${ADMIN_FLAGS[@]}"

# mci-api (broker 필수 — --with-api 옵션 시 활성)
if [ "$WITH_API" = "1" ]; then
  start mci-api ./build/bin/mci-api \
    --dev --listen "${LISTEN_API:-:8080}" \
    --broker-host 127.0.0.1 --broker-port 11217
fi

# mci-edge-fix (--with-fix) — FIX 4.4 DMZ gateway.
# admin UI 의 /fix-counterparties.html 에서 carrier 등록 + fix-tester CLI 로 smoke.
if [ "$WITH_FIX" = "1" ]; then
  # admin embedded etcd URL 결정.
  #   1) ETCD_URL 환경변수 우선 (외부 etcd 또는 알고 있는 dev port 강제).
  #   2) 없으면 admin log 의 "client_url":"http://127.0.0.1:PORT" line 을 poll.
  # 예전에는 sleep 1 + grep 1회로 URL 이 아직 안 찍힌 순간에 빈값을 잡아
  # --etcd "" 로 조용히 부팅 → counterparty 0개 → SIGHUP 해도 반영 안 됨.
  if [ -z "${ETCD_URL:-}" ]; then
    for i in {1..20}; do
      ETCD_URL=$(grep -oE '"client_url":"http://127\.0\.0\.1:[0-9]+"' logs/mci-admin.log 2>/dev/null \
                 | head -1 | grep -oE 'http://127\.0\.0\.1:[0-9]+' || true)
      [ -n "${ETCD_URL:-}" ] && break
      sleep 0.5
    done
  fi
  if [ -z "${ETCD_URL:-}" ]; then
    echo "==> mci-edge-fix: admin embedded etcd URL 을 10초 안에 못 잡음"
    echo "    ETCD_URL=http://127.0.0.1:PORT 로 명시 지정하거나 mci-admin 로그 확인"
    exit 1
  fi
  echo "==> mci-edge-fix: etcd_url=$ETCD_URL"
  mkdir -p /tmp/wtg-fix-store
  start mci-edge-fix ./build/bin/mci-edge-fix \
    --port "${FIX_PORT:-5001}" \
    --stats "${FIX_STATS:-:5002}" \
    --sender "${FIX_SENDER:-WTG}" \
    --tx-forward "${FIX_TX_FORWARD:-}" \
    --push-secret "${FIX_PUSH_SECRET:-dev-secret}" \
    --store-dir "${FIX_STORE_DIR:-/tmp/wtg-fix-store}" \
    --etcd "$ETCD_URL" \
    --log-level info
fi

# mci-edge-md (--with-md) — FIX 4.4 시세 DMZ gateway (Phase A skeleton).
# 정적 카운터파티 seed 만. 하드코딩 quote 로 35=W 응답. Phase B 는 etcd + gRPC upstream.
if [ "$WITH_MD" = "1" ]; then
  # Phase A 는 etcd 미연결. seed 는 env 로 override 가능 (미지정 시 데모용 1개).
  # 형식: 'ID=PASSWORD,SITE,TIER,USID' (반복 가능하려면 MD_SEED_CP1/CP2/... 로 확장 예정)
  MD_SEED_CP="${MD_SEED_CP:-ECN_MD_TEST_01=test-pw,HQ,VIP,ECN_MD_TEST_01}"
  start mci-edge-md ./build/bin/mci-edge-md \
    --port "${MD_PORT:-5011}" \
    --stats "${MD_STATS:-127.0.0.1:5012}" \
    --sender "${MD_SENDER:-WTG_MD}" \
    --seed-cp "$MD_SEED_CP" \
    --log-level info
fi

# tickloop (dev tick generator) — 영구 위치 scripts/wtg-dev-tickloop.py
TICKLOOP="$REPO/scripts/wtg-dev-tickloop.py"
if [ -f "$TICKLOOP" ]; then
  echo "==> tickloop 시작"
  nohup python3 "$TICKLOOP" > logs/dev-tick.log 2>&1 &
  echo "    pid=$!"
else
  echo "==> tickloop 스크립트 없음 ($TICKLOOP) — 시세 흐름 안 함"
fi

sleep 2
echo
echo "==> 부팅 완료. status :"
"$REPO/scripts/wtg-status.sh" 2>/dev/null || echo "    (wtg-status.sh 없음 — make 후 재실행)"
