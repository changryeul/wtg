#!/usr/bin/env bash
# WTG dev 환경 부트스트랩 — TimescaleDB Docker 컨테이너 + DDL + 데모 봉 seed.
#
# 결과: localhost:5432 에 wtg/wtg user 로 접속 가능한 TimescaleDB.
# 사용 후: mci-chart --dsn 'postgres://wtg:secret@localhost:5432/wtg?sslmode=disable'
#
# 컨테이너 재사용 — 이미 같은 이름이 있으면 start 만 한다. 데이터 영속.
# 완전 초기화는 ./scripts/dev-down.sh 후 다시 실행.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"

CONTAINER=${CONTAINER:-wtg-timescale-dev}
IMAGE=${IMAGE:-timescale/timescaledb:latest-pg16}
DB_USER=${DB_USER:-wtg}
DB_PASS=${DB_PASS:-secret}
DB_NAME=${DB_NAME:-wtg}
DB_PORT=${DB_PORT:-5432}

if ! command -v docker >/dev/null; then
  echo "ERROR: docker 가 PATH 에 없음" >&2
  exit 1
fi

echo "==> 기존 컨테이너 확인"
if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER"; then
  echo "    재사용: $CONTAINER"
  docker start "$CONTAINER" >/dev/null
else
  echo "    새로 생성: $IMAGE → :$DB_PORT"
  docker run -d --name "$CONTAINER" \
    -e POSTGRES_USER="$DB_USER" \
    -e POSTGRES_PASSWORD="$DB_PASS" \
    -e POSTGRES_DB="$DB_NAME" \
    -p "$DB_PORT:5432" \
    "$IMAGE" >/dev/null
fi

echo "==> Postgres ready 대기 (최대 60s)"
for i in $(seq 1 60); do
  if docker exec "$CONTAINER" pg_isready -U "$DB_USER" -d "$DB_NAME" -q 2>/dev/null; then
    echo "    OK (${i}s)"
    break
  fi
  if [ "$i" = "60" ]; then
    echo "ERROR: TimescaleDB ready timeout" >&2
    docker logs --tail 30 "$CONTAINER" >&2
    exit 1
  fi
  sleep 1
done

echo "==> DDL 적용 (etc/sql/quote_bars.sql)"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -q -v ON_ERROR_STOP=1 \
  < "$REPO_ROOT/etc/sql/quote_bars.sql"

echo "==> 데모 봉 seed (USD/KRW · EUR/KRW · JPY/KRW, 1m~1d, 7일치)"
python3 "$HERE/seed_demo_bars.py" \
  | docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -q -v ON_ERROR_STOP=1

echo "==> seed 검증"
docker exec "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -t -A -c \
  "SELECT pair, tf, COUNT(*) FROM quote_bars GROUP BY pair, tf ORDER BY pair, tf;" \
  | sed 's/^/    /'

DSN="postgres://$DB_USER:$DB_PASS@localhost:$DB_PORT/$DB_NAME?sslmode=disable"

cat <<EOF

============================================================
✓ TimescaleDB 준비 완료
   DSN: $DSN

다음 단계:
   make build
   ./build/bin/mci-chart --listen :8086 --dsn '$DSN'
   open http://localhost:8086/

종료 / 초기화:
   ./scripts/dev-down.sh
============================================================
EOF
