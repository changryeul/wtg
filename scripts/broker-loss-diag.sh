#!/usr/bin/env bash
# broker-loss-diag.sh — broker 측 publish 손실 추적 진단.
#
# load-gen 부하 중 broker / TCP / 양측 카운터를 동시 샘플링해 어디서 손실이
# 발생하는지 추정. C 엔진 무수정 정책상 broker 내부 카운터는 못 보지만 외부에서:
#   - broker log 의 카테고리별 메시지 빈도 (Lost / Published 0/N / WARN 등)
#   - broker 의 TCP socket Recv-Q / Send-Q (커널 backpressure 가시화)
#   - forwarder 의 published_total Δ
#   - mci-price 의 received / sub_drops Δ
#
# 결과는 logs/broker-diag-<ts>.txt 로.
#
# 사용:
#   ./scripts/broker-loss-diag.sh                  # HIGH 시나리오 30s
#   ./scripts/broker-loss-diag.sh mid              # MID 시나리오
#   DURATION=15s ./scripts/broker-loss-diag.sh     # 시간 override

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
LOGDIR="$REPO_ROOT/logs"
mkdir -p "$LOGDIR"

SCENARIO="${1:-high}"
DURATION="${DURATION:-30s}"
TS=$(date +%Y%m%d-%H%M%S)
OUT="$LOGDIR/broker-diag-${SCENARIO}-${TS}.txt"

# broker container name (docker mymqd)
BROKER_CTR=mymqd

# 1) baseline 캡처 — forwarder + price + broker log 위치
echo "════ broker-loss-diag [$SCENARIO, $DURATION] ════" | tee "$OUT"
echo "시작: $(date)" | tee -a "$OUT"
echo | tee -a "$OUT"

# broker 의 현재 mymqd log file path
BROKER_LOG=$(docker exec "$BROKER_CTR" sh -c 'ls -t /opt/mymq/log/mymqd-*.log 2>/dev/null | head -1' || echo "")
if [ -z "$BROKER_LOG" ]; then
    echo "ERR: broker log 파일 못 찾음" | tee -a "$OUT"
    exit 2
fi
echo "broker log: $BROKER_LOG" | tee -a "$OUT"

# baseline 카운터
F_BEFORE=$(curl -sS http://127.0.0.1:9091/stats 2>/dev/null \
           | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("received_total",0),d.get("published_total",0))' \
           || echo "0 0")
P_BEFORE=$(curl -sS http://127.0.0.1:8082/v1/price-stats 2>/dev/null \
           | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("received",0),d.get("ticks",0),d.get("sub_drops",0))' \
           || echo "0 0 0")
# broker log line count baseline (delta = 부하 중 증가량)
BROKER_LINES_BEFORE=$(docker exec "$BROKER_CTR" sh -c "wc -l < '$BROKER_LOG'" 2>/dev/null | awk '{print $1}' || echo 0)
BROKER_LINES_BEFORE=${BROKER_LINES_BEFORE:-0}

# 2) 부하 + 백그라운드 샘플러
echo "▶ 부하 시작 + 백그라운드 socket / log 샘플링" | tee -a "$OUT"
SAMPLE_TMP=$(mktemp)
trap 'rm -f "$SAMPLE_TMP"' EXIT

# socket Q 샘플러 — 1초마다 broker 의 socket 상태 캡처
(
    while true; do
        ts=$(date +%H:%M:%S)
        # broker 의 listen socket (11217) 의 outbound 소켓들 Recv-Q / Send-Q
        # macOS netstat 는 broker (Docker) 내부에서. host netstat 는 container 의 socket 못 봄.
        docker exec "$BROKER_CTR" sh -c "netstat -an 2>/dev/null | grep ':11217 ' | grep ESTABLISHED" 2>/dev/null \
            | awk -v t="$ts" '{ print t, $2, $3, $5 }' >> "$SAMPLE_TMP" || true
        sleep 1
    done
) &
SAMPLER_PID=$!

# 부하
"$REPO_ROOT/scripts/load-test.sh" "$SCENARIO" 2>&1 | tail -10 | tee -a "$OUT"

# 샘플러 중지
kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true

# 부하 종료 후 2s settling — TCP recv buffer drain 시간. 부하 종료 직후
# broker 가 forwarder 의 마지막 write 들을 못 읽었을 수 있어 측정 race.
sleep 2

# 3) 종료 후 카운터 + delta
F_AFTER=$(curl -sS http://127.0.0.1:9091/stats 2>/dev/null \
          | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("received_total",0),d.get("published_total",0))' \
          || echo "0 0")
P_AFTER=$(curl -sS http://127.0.0.1:8082/v1/price-stats 2>/dev/null \
          | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("received",0),d.get("ticks",0),d.get("sub_drops",0))' \
          || echo "0 0 0")
BROKER_LINES_AFTER=$(docker exec "$BROKER_CTR" sh -c "wc -l < '$BROKER_LOG'" 2>/dev/null | awk '{print $1}' || echo 0)
BROKER_LINES_AFTER=${BROKER_LINES_AFTER:-0}

echo | tee -a "$OUT"
echo "════ 카운터 Δ ════" | tee -a "$OUT"
python3 - "$F_BEFORE" "$F_AFTER" "$P_BEFORE" "$P_AFTER" <<'EOF' | tee -a "$OUT"
import sys
fb = sys.argv[1].split(); fa = sys.argv[2].split()
pb = sys.argv[3].split(); pa = sys.argv[4].split()
fr  = int(fa[0])-int(fb[0])   # forwarder UDP packets received
fpE = int(fa[1])-int(fb[1])   # forwarder published envelopes (batch * msg)
prM = int(pa[0])-int(pb[0])   # broker→mci-price MESSAGE delta
ptE = int(pa[1])-int(pb[1])   # mci-price tick (envelope) delta
pd  = int(pa[2])-int(pb[2])   # subDrops delta
# 평균 batch — mci-price 의 ticks / messages 가 가장 정확.
avg_batch = ptE / prM if prM > 0 else 1.0
# forwarder 의 message-수 환산 (envelope / avg_batch)
fpM = int(fpE / avg_batch) if avg_batch > 0 else 0
print(f"forwarder UDP recv     : {fr:>10,}")
print(f"forwarder published env: {fpE:>10,}")
print(f"forwarder published msg: {fpM:>10,}  (envelopes / avg_batch {avg_batch:.2f})")
print(f"broker→mci-price msg   : {prM:>10,}  ({prM/max(fpM,1)*100:5.1f}% of pub msg)")
print(f"mci-price ticks        : {ptE:>10,}")
print(f"mci-price sub_drops    : {pd:>10,}")
loss = fpM - prM
print()
print(f"broker side 손실 추정  : forwarder pub msg - broker→mci msg = {loss:>10,} messages")
print(f"                        = {loss/max(fpM,1)*100:.1f}% 부하 중 broker 쪽에서 사라짐")
EOF

# 4) broker log delta 분석 — 부하 중 추가된 line 만 grep
echo | tee -a "$OUT"
echo "════ broker log Δ 분석 (부하 중 신규 라인) ════" | tee -a "$OUT"
NEW_LINES=$(( BROKER_LINES_AFTER - BROKER_LINES_BEFORE ))
echo "신규 line: $NEW_LINES" | tee -a "$OUT"

docker exec "$BROKER_CTR" sh -c "tail -n $NEW_LINES $BROKER_LOG 2>/dev/null" > /tmp/broker_delta.log 2>/dev/null || true

# 카테고리별 카운트
echo | tee -a "$OUT"
echo "─ 카테고리별 빈도:" | tee -a "$OUT"
for cat in "Published " "Lost " "WRN " "ERROR" "Failed" "drop" "Publish broadcasting"; do
    cnt=$(grep -c "$cat" /tmp/broker_delta.log 2>/dev/null | head -1)
    cnt=${cnt:-0}
    cnt=$(echo "$cnt" | tr -d '[:space:]')
    if [ "${cnt:-0}" != "0" ] 2>/dev/null && [ -n "$cnt" ]; then
        printf "  %-25s %s\n" "$cat" "$cnt" | tee -a "$OUT"
    fi
done

# "Published N/M" 의 N 분포 — 매번 0 인지, 가끔 0 인지
echo | tee -a "$OUT"
echo "─ Published N/M 의 N(전송 성공 client 수) 분포:" | tee -a "$OUT"
grep -oE "Published [0-9]+/[0-9]+" /tmp/broker_delta.log 2>/dev/null \
    | awk '{print $2}' | sort | uniq -c | sort -rn | head -10 | tee -a "$OUT" || true

# 5) socket Q 통계 — 적체 패턴
echo | tee -a "$OUT"
echo "════ broker socket 11217 의 Send-Q / Recv-Q ════" | tee -a "$OUT"
if [ -s "$SAMPLE_TMP" ]; then
    python3 - <<EOF | tee -a "$OUT"
sums = {}
counts = {}
with open("$SAMPLE_TMP") as f:
    for line in f:
        parts = line.split()
        if len(parts) < 4: continue
        ts, recvq, sendq = parts[0], parts[1], parts[2]
        peer = parts[3]
        try:
            r, s = int(recvq), int(sendq)
        except ValueError:
            continue
        key = peer
        sums.setdefault(key, [0, 0, 0, 0])  # max_r, max_s, sum_r, sum_s
        counts[key] = counts.get(key, 0) + 1
        sums[key][0] = max(sums[key][0], r)
        sums[key][1] = max(sums[key][1], s)
        sums[key][2] += r
        sums[key][3] += s
print(f"{'peer':<28} {'samples':>8} {'max RecvQ':>10} {'max SendQ':>10} {'avg RecvQ':>10} {'avg SendQ':>10}")
for peer, (mr, ms, sr, ss) in sorted(sums.items(), key=lambda x: -x[1][1]):
    n = counts[peer]
    print(f"{peer:<28} {n:>8} {mr:>10} {ms:>10} {sr//n:>10} {ss//n:>10}")
EOF
else
    echo "socket 샘플 없음 (broker 내부 netstat 비활성)" | tee -a "$OUT"
fi

echo | tee -a "$OUT"
echo "═════════════════════════════════════════════" | tee -a "$OUT"
echo "결과: $OUT"
