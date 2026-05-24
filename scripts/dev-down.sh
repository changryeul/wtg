#!/usr/bin/env bash
# WTG dev 환경 정리 — TimescaleDB 컨테이너 중지 + 데이터 삭제.
#
# 컨테이너 중지만 원하면 docker stop wtg-timescale-dev 직접 사용.
# 이 스크립트는 stop + rm — 데이터 wipe.

set -euo pipefail

CONTAINER=${CONTAINER:-wtg-timescale-dev}

if docker ps -a --format '{{.Names}}' | grep -qx "$CONTAINER"; then
  echo "==> 중지 + 삭제: $CONTAINER"
  docker stop "$CONTAINER" >/dev/null 2>&1 || true
  docker rm "$CONTAINER" >/dev/null
  echo "✓ 컨테이너 제거 완료"
else
  echo "==> 컨테이너 $CONTAINER 가 없음 — skip"
fi
