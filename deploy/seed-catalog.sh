#!/usr/bin/env bash
# WTG 카탈로그 etcd 시드 — 배포된 etc/*.json 을 etcd 에 주입 (EC2 에서 sudo 로 실행).
#
# 배경: mci-price 는 --etcd 가 설정되면 file flag (--symbols/--pricing/--profiles)
# 를 무시하고 etcd watch 만 사용한다. etcd 가 비어 있으면 quote fan-out 대상 0.
# 본 스크립트는 배포 디렉토리의 etc/*.json 을 mci-price 가 watch 하는 키로 넣는다:
#   wtg/pricing/table            ← etc/pricing.json (단일 키, 문서 통째)
#   wtg/price/profiles/<C.S.T>   ← etc/profiles.json (프로파일당 1키)
#   wtg/quote/symbols/<SYM>      ← etc/symbols.json (심볼당 1키)
# watcher 가 live 반영하므로 서비스 재시작 불필요. 재실행 안전 (idempotent put).
#
# 사용 (EC2 rocky):
#   sudo bash /home/winway/nh-fxallone-server/wtg/src/deploy/seed-catalog.sh

set -euo pipefail

WTG_HOME=${WTG_HOME:-/home/winway/nh-fxallone-server/wtg}
ETCDCTL=${ETCDCTL:-/usr/local/bin/etcdctl}
EP=${ETCD_ENDPOINTS:-http://127.0.0.1:2379}

echo "==> pricing table (wtg/pricing/table)"
"$ETCDCTL" --endpoints="$EP" put wtg/pricing/table "$(cat "$WTG_HOME/etc/pricing.json")" >/dev/null
echo "  put wtg/pricing/table"

echo "==> profiles + symbols"
python3 - "$WTG_HOME" <<'PYEOF' | while IFS=$'\t' read -r k v; do
import json, sys
home = sys.argv[1]
for p in json.load(open(f"{home}/etc/profiles.json")):
    key = f"wtg/price/profiles/{p['channel']}.{p['site']}.{p['tier']}"
    print(key, json.dumps(p, ensure_ascii=False), sep="\t")
for s in json.load(open(f"{home}/etc/symbols.json")):
    print(f"wtg/quote/symbols/{s['symbol']}", json.dumps(s, ensure_ascii=False), sep="\t")
PYEOF
  "$ETCDCTL" --endpoints="$EP" put "$k" "$v" >/dev/null
  echo "  put $k"
done

echo "==> 라우팅 alias (transaction alias → exchange/routing_key)"
"$ETCDCTL" --endpoints="$EP" put wtg/routes/W1101T01 \
  '{"alias":"W1101T01","exchange":"dom","routing_key":"W1101T01","active":true,"comment":"공인인증 테스트 트랜잭션 (dev)"}' >/dev/null
echo "  put wtg/routes/W1101T01 → dom/W1101T01"

echo "==> user-profiles (usid→Site/Tier — login JWT claims 용)"
"$ETCDCTL" --endpoints="$EP" put wtg/auth/user-profiles/tester01 \
  '{"site":"HQ","tier":"VIP"}' >/dev/null
echo "  put wtg/auth/user-profiles/tester01 (HQ/VIP)"
"$ETCDCTL" --endpoints="$EP" put wtg/auth/user-profiles/admin01 \
  '{"site":"HQ","tier":"VIP"}' >/dev/null
echo "  put wtg/auth/user-profiles/admin01 (HQ/VIP)"

echo "==> FIX 테스트 counterparty (개발 환경용 — 운영 전환 시 이 블록 제거)"
"$ETCDCTL" --endpoints="$EP" put wtg/fix/counterparties/ECN_TEST_01 \
  '{"password":"test-pw","channel":"FIX","site":"HQ","tier":"VIP","usid":"ECN_TEST_01"}' >/dev/null
echo "  put wtg/fix/counterparties/ECN_TEST_01 (주문 5001)"
"$ETCDCTL" --endpoints="$EP" put wtg/fix/counterparties/ECN_MD_TEST_01 \
  '{"password":"test-pw","channel":"FIX","site":"HQ","tier":"VIP","usid":"ECN_MD_TEST_01","md_req_role_set":["MD"]}' >/dev/null
echo "  put wtg/fix/counterparties/ECN_MD_TEST_01 (시세 5011, md_req_role_set=[MD])"

echo "==> 확인"
"$ETCDCTL" --endpoints="$EP" get wtg/ --prefix --keys-only | grep -v '^$' | sed 's/^/  /'
