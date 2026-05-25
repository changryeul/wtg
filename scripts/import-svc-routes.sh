#!/usr/bin/env bash
#
# import-svc-routes.sh — broker (mymqd) 가 알고 있는 svc → (xchg, rkey) 매핑을
# routing.Registry 에 alias 로 일괄 등록.
#
# 동작:
#   1. mci-admin 의 svc-io 카탈로그에서 모든 svc code 를 가져온다.
#   2. 각 code 에 대해 broker 의 GET_WHOIS (argv1=code) 로 진짜 매핑 조회.
#   3. 매칭이 있으면 PUT /v1/admin/routes/<code> 로 alias 등록.
#   4. 매칭 없는 code 는 skip — broker 가 모르는 svc 라 무시.
#
# 사고 방지: splitSvcCode 휴리스틱(5+나머지) 으로 잘못 라우팅된 transaction 이
# broker 에 도달하는 일이 잦았음. 이 스크립트가 broker 의 답을 진실의 원천으로
# 삼아 정확한 alias 매핑을 일괄 등록한다. 이후 svc-io wire test 는 이 alias 로
# 라우팅 결정 (svcio.go 의 routing.Resolve 참조).
#
# 사용법:
#   ./scripts/import-svc-routes.sh                   # default 127.0.0.1:9090
#   MCI_ADMIN_URL=http://10.0.0.20:9090 \
#     X_WTG_USER=admin01 \
#     ./scripts/import-svc-routes.sh                 # 커스텀 endpoint + admin
#
#   ./scripts/import-svc-routes.sh --dry-run          # 등록은 안 하고 매핑만 출력
#   ./scripts/import-svc-routes.sh --filter 'W1*'     # code glob 필터
#   ./scripts/import-svc-routes.sh --overwrite        # 기존 alias 도 덮어쓰기
#
# 의존성: jq, curl

set -euo pipefail

ADMIN_URL="${MCI_ADMIN_URL:-http://127.0.0.1:9090}"
USER_ID="${X_WTG_USER:-admin01}"
DRY_RUN=0
OVERWRITE=0
FILTER=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)   DRY_RUN=1; shift ;;
    --overwrite) OVERWRITE=1; shift ;;
    --filter)    FILTER="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,28p' "$0"
      exit 0 ;;
    *) echo "알 수 없는 옵션: $1" >&2; exit 1 ;;
  esac
done

command -v jq >/dev/null || { echo "jq 미설치 (brew install jq)" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl 미설치" >&2; exit 1; }

api() {
  curl -sS -H "X-WTG-User: ${USER_ID}" "$@"
}

echo "==> mci-admin: ${ADMIN_URL}"
echo "==> svc-io 카탈로그 조회"

SVC_CODES=$(api "${ADMIN_URL}/v1/admin/svc-io?max=10000" \
  | jq -r '.items[]?.code // empty' \
  | sort -u)

if [[ -z "${SVC_CODES}" ]]; then
  echo "svc-io 카탈로그 비어있음 — mci-admin 의 -svc-inc-dir 가 설정되어 있나?" >&2
  exit 1
fi

if [[ -n "${FILTER}" ]]; then
  SVC_CODES=$(echo "${SVC_CODES}" | grep -E "$(echo "${FILTER}" | sed 's/\*/.*/g')" || true)
fi

TOTAL=$(echo "${SVC_CODES}" | wc -l | tr -d ' ')
echo "==> 대상 svc code: ${TOTAL}개"

# 기존 routes 캐시 (overwrite 결정용)
EXISTING=$(api "${ADMIN_URL}/v1/admin/routes" | jq -r '.rules[]?.alias // empty' | sort -u)

REGISTERED=0
SKIPPED=0
NOTFOUND=0
EXISTS=0
FAILED=0

while IFS= read -r CODE; do
  [[ -z "${CODE}" ]] && continue

  if echo "${EXISTING}" | grep -qx "${CODE}"; then
    if [[ ${OVERWRITE} -eq 0 ]]; then
      EXISTS=$((EXISTS + 1))
      continue
    fi
  fi

  # broker 에 진짜 매핑 조회 — argv1=code (rkey 검색)
  WHOIS=$(api "${ADMIN_URL}/v1/admin/whois?argv1=${CODE}" 2>/dev/null || echo '{}')
  ENTRY=$(echo "${WHOIS}" | jq -c '.data.whois[0] // empty' 2>/dev/null || true)

  if [[ -z "${ENTRY}" || "${ENTRY}" == "null" ]]; then
    NOTFOUND=$((NOTFOUND + 1))
    continue
  fi

  XCHG=$(echo "${ENTRY}" | jq -r '.xchg // empty')
  RKEY=$(echo "${ENTRY}" | jq -r '.rkey // empty')
  APPL=$(echo "${ENTRY}" | jq -r '.appl // empty')

  if [[ -z "${XCHG}" || -z "${RKEY}" ]]; then
    SKIPPED=$((SKIPPED + 1))
    continue
  fi

  printf "  %-12s → xchg=%-8s rkey=%-16s (appl=%s)\n" "${CODE}" "${XCHG}" "${RKEY}" "${APPL}"

  if [[ ${DRY_RUN} -eq 1 ]]; then
    REGISTERED=$((REGISTERED + 1))
    continue
  fi

  RESP=$(api -X PUT \
    -H 'Content-Type: application/json' \
    -d "$(jq -n \
        --arg x "${XCHG}" \
        --arg r "${RKEY}" \
        --arg c "broker whois import (appl=${APPL})" \
        '{exchange: $x, routing_key: $r, active: true, comment: $c}')" \
    "${ADMIN_URL}/v1/admin/routes/${CODE}")

  if echo "${RESP}" | jq -e '.alias' >/dev/null 2>&1; then
    REGISTERED=$((REGISTERED + 1))
  else
    FAILED=$((FAILED + 1))
    echo "    ! 등록 실패: $(echo "${RESP}" | jq -c '.')" >&2
  fi
done <<< "${SVC_CODES}"

echo
echo "==> 결과 요약"
echo "  등록 (또는 dry-run 대상): ${REGISTERED}"
echo "  broker 미매핑 (skip):     ${NOTFOUND}"
echo "  이미 등록 (skip):         ${EXISTS}  (--overwrite 로 덮어쓰기)"
echo "  매핑 빈 값 (skip):        ${SKIPPED}"
echo "  등록 실패:                ${FAILED}"

if [[ ${DRY_RUN} -eq 1 ]]; then
  echo
  echo "  (dry-run 모드 — 실제로는 등록 안 됨. --dry-run 빼고 다시 실행)"
fi
