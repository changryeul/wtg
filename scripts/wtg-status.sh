#!/bin/bash
# WTG dev 스택 status — 프로세스 + 포트 + 핵심 카운터 한 화면.
# 사용 :
#   ./scripts/wtg-status.sh           # 1회 스냅샷
#   watch -tcn 2 ./scripts/wtg-status.sh   # 2초 주기 갱신
set -u
NAMES=(mci-admin mci-price mci-edge-price quote-forwarder prometheus wtg-dev-tickloop load-gen mci-chart)
printf "\033[1m=== WTG dev 스택 — %s ===\033[0m\n" "$(date '+%Y-%m-%d %H:%M:%S')"
printf "%-22s %-8s %-8s %-12s %s\n" "프로세스" "PID" "상태" "RSS" "명령"
for n in "${NAMES[@]}"; do
  pid=$(pgrep -f "$n" | head -1)
  if [ -n "$pid" ]; then
    rss=$(ps -o rss= -p "$pid" 2>/dev/null | awk '{printf "%.1fMB", $1/1024}')
    cmd=$(ps -o command= -p "$pid" 2>/dev/null | sed 's|.*/||' | head -c 60)
    printf "%-22s %-8s \033[32m%-8s\033[0m %-12s %s\n" "$n" "$pid" "● UP" "${rss:-?}" "$cmd"
  else
    printf "%-22s %-8s \033[31m%-8s\033[0m %-12s %s\n" "$n" "—" "● DOWN" "—" "—"
  fi
done
echo
printf "\033[1m=== HTTP 헬스체크 ===\033[0m\n"
check() {
  local label=$1 url=$2
  code=$(curl -s -o /dev/null -w "%{http_code}" -m 1 "$url" 2>/dev/null || echo "ERR")
  if [ "$code" = "200" ] || [ "$code" = "401" ]; then color=32; else color=31; fi
  printf "  \033[${color}m●\033[0m %-22s HTTP %s  %s\n" "$label" "$code" "$url"
}
check "mci-admin /"               "http://127.0.0.1:9090/"
check "mci-price /price-stats"    "http://127.0.0.1:8082/v1/price-stats"
check "mci-edge-price /metrics"   "http://127.0.0.1:8083/metrics"
check "quote-forwarder /stats"    "http://127.0.0.1:9091/stats"
check "prometheus /-/ready"       "http://127.0.0.1:9095/-/ready"
echo
printf "\033[1m=== 시세 카운터 (mci-price) ===\033[0m\n"
curl -s -m 1 http://127.0.0.1:8082/v1/price-stats 2>/dev/null | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
    print(f'  received={d[\"received\"]:,} matched={d[\"matched\"]:,} dropped={d[\"dropped\"]:,} sub_drops={d[\"sub_drops\"]:,}')
    c=d.get('conflation',{})
    print(f'  conflation: symbols={c.get(\"Symbols\",0)} updates={c.get(\"Updates\",0):,} swaps={c.get(\"Swaps\",0):,}')
except Exception as e: print(f'  (응답 파싱 실패: {e})')
" 2>/dev/null
echo
printf "\033[1m=== forwarder 카운터 ===\033[0m\n"
curl -s -m 1 http://127.0.0.1:9091/stats 2>/dev/null | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
    iv=d['invalid_quotes']; bad=sum(iv.values())
    print(f'  received={d[\"received_total\"]:,} published={d[\"published_total\"]:,} errors={d[\"publish_errors\"]} uptime={d[\"uptime_sec\"]:.0f}s')
    print(f'  invalid={bad}' + (f' ({\", \".join(f\"{k}={v}\" for k,v in iv.items() if v)})' if bad else ''))
except Exception as e: print('  (forwarder 미기동 또는 응답 실패)')
" 2>/dev/null
