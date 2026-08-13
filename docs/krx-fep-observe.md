# KRX 시세 트랙2 — FEP 가상 시세 관측 절차

NH FEP(Front-End Processor)가 **가상 시세**를 KRX 멀티캐스트 그룹에 흘릴 때,
`mci-edge-krx`(트랙2)가 이를 수신·파싱·ws fan-out 하는지 관측하는 절차.
레거시 C 피드(FEP 축)와 mci-edge-krx(MCI 축)가 **같은 멀티캐스트를 동시 수신**하는
이중 축(dual-run) 구성을 전제로 한다.

```
FEP(가상 시세 송신) ──멀티캐스트 227.10.20.10 : 60641-3(파생)/60631-2(채권)──┐
                                                                          ├─▶ [FEP 축] 레거시 C 피드(kbfut_sise RECV → SHM/push)
                                                                          └─▶ [MCI 축] mci-edge-krx --mcast → web ws (:8085)
```

- 대상 호스트: EC2 `ip-10-0-1-106` (rocky SSH / winway 앱 유저).
- 바이너리: `/home/winway/common/wtg/bin/{mci-edge-krx,krx-tester}` (deploy 반영).
- 시각 주의: 관측 자체는 아무 때나 가능(FEP 가 가상 송신 중이면). 실 KRX 회선은 무관.

## 0. 사전 상태 확인 (기동돼 있는가)

```bash
# SSH: ssh fxec2  (rocky)
WTG=/home/winway/common/wtg

# (a) mci-edge-krx 프로세스 (비-systemd, winway 백그라운드)
pgrep -af './bin/mci-edge-krx'
# (b) ws healthz
curl -s http://127.0.0.1:8085/healthz          # → ok conns=N
# (c) 프로세스 상태판
cd $WTG/src/scripts && timeout 20 ./wtg-status.sh | grep -i krx    # ● UP
# (d) admin 패널 (DevMode 헤더 필요)
curl -s -H 'X-WTG-User: admin' http://127.0.0.1:9090/v1/admin/mci-health \
  | python3 -c 'import sys,json;[print(s["name"],s["up"]) for s in json.load(sys.stdin)["services"] if "krx" in s["name"]]'
```

미기동 시 (systemd 아님 — 수동 백그라운드 기동):
```bash
sudo -u winway bash -c "cd $WTG && setsid nohup ./bin/mci-edge-krx --mcast --listen :8085 \
  >$WTG/mci-edge-krx.out 2>&1 < /dev/null &"
# 기본 group/ports = 227.10.20.10 / 60641,60642,60643,60631,60632 (config.go)
```

## 1. FEP 도달 확인 (수신 전 먼저)

FEP 가상 시세가 이 호스트의 그룹에 실제로 도착하는지 (sudo 필요):
```bash
# 파생 시세 포트(A306F 체결/B606F 호가)
sudo tcpdump -ni any host 227.10.20.10 and udp port 60642 -c 10
# 파생 마스터(A006F)
sudo tcpdump -ni any host 227.10.20.10 and udp port 60641 -c 5
# 채권 시세(A301K/B601K)
sudo tcpdump -ni any host 227.10.20.10 and udp port 60631 -c 5
```
- 패킷이 잡히면 FEP 가상 시세가 도달 중. `-A` 를 붙이면 앞부분에서 TR코드(A306F 등)와
  종목코드(offset 17~28)를 눈으로 확인 가능 → 구독할 종목코드 파악.
- 안 잡히면 → FEP 송신/네트워크 경로 문제 (§5 트러블슈팅).

## 2. MCI 축 수신 관측

### 2-1. 수신·파싱 카운터 (종목코드 몰라도 확인)
mci-edge-krx 는 30초마다 mcast 통계를 로그한다:
```bash
tail -f $WTG/mci-edge-krx.out | grep -i "mcast stats"
# msg="KRX mcast stats" packets=.. fanout=.. unknown=.. read_errs=.. conns=..
```
- `packets` 증가 → 수신 정상. `fanout` 증가 → 파싱+구독전송 정상.
- `unknown` 만 증가 → 미지원 TR(G706F/G701K 등) 이거나 그룹 오설정.

### 2-2. ws envelope 관측 (종목 지정)
tcpdump 로 확인한(또는 FEP 시나리오상 알려진) 종목코드로 구독:
```bash
$WTG/bin/krx-tester --url ws://127.0.0.1:8085/v1/subscribe \
  --symbols 101V6000,105V3000 --timeout 30s
# fut.trade / fut.book / bond.trade / bond.book 요약 라인 실시간 출력
# --json 을 붙이면 원본 JSON 그대로
```
- `fut.trade` 에 `last/open/high/low/cdiff/csign`(직전대비) 가 채워지면 체결 파싱 OK.
- 전일대비(`diff/rate/sign`)는 **A006F 마스터가 먼저 도착한 뒤부터** 채워짐
  (FEP 가 장 초 마스터를 먼저 송신해야 함). 정산가(`settle`)는 H306F 수신 후.
- `fut.book` 에 `ask[5]/bid[5]` 5단이 오면 호가 파싱 OK.

## 2b. "client 로 전달됐는지" 확인 (정식 화면 없이)

정식 web 화면 전이라도, mci-edge-krx 의 외부 ws(`/v1/subscribe`)에 붙는 무엇이든
"client" 다. 아래 중 하나로 전달을 확인한다.

### 방법 1 — krx-tester (헤드리스 client, 권장)
`krx-tester` 자체가 ws client 다. 여기 envelope 이 찍히면 **MCI 수신→client 전달**이
증명된 것 (실제 fan-out 경로를 그대로 탐).
```bash
$WTG/bin/krx-tester --url ws://127.0.0.1:8085/v1/subscribe --symbols 101V6000 --json
```

### 방법 2 — 브라우저 화면 (눈으로)
**내장 인스펙터 (권장)** — mci-edge-krx 가 `GET /` 로 확인 화면을 직접 서빙한다.
브라우저로 **`http://<host>:8085/`** 만 열면 됨 (ws URL 은 접속 주소에서 자동 도출).
종목 입력 → Connect → last/전일대비/직전대비/호가/정산 실시간 표(상승 빨강/하락 파랑).

내 노트북에서 EC2(사설망)를 볼 때는 SSH 포트포워드:
```bash
ssh -L 8085:127.0.0.1:8085 fxec2      # 터널 유지
# 브라우저로 http://127.0.0.1:8085/ 열기
```
(파일로 열고 싶으면 `docs/krx-ws-test-client.html` 단일 파일도 동일 기능 — file:// 사용 시
URL 을 수동 입력.)

### 방법 3 — websocat 한 줄 (CLI 대안)
```bash
# 접속 후 구독 메시지 전송, 이후 수신 덤프
( echo '{"type":"subscribe","symbols":["101V6000"]}'; sleep 30 ) \
  | websocat ws://127.0.0.1:8085/v1/subscribe
```

### 방법 4 — 서버측 카운터 corroboration (내용 없이 전달여부만)
클라 없이도 "붙었는지 / fan-out 됐는지" 를 서버에서 확인:
```bash
curl -s http://127.0.0.1:8085/healthz          # → ok conns=N  (연결된 client 수)
tail -f $WTG/mci-edge-krx.out | grep "mcast stats"   # fanout 증가 = 구독자에게 송신됨
```
- `conns` 가 client 접속 시 증가 → 접속 확인.
- `fanout` 이 시세 유입 중 증가 → 구독자에게 전달 확인 (구독자 0 이면 증가해도 실 전송 0).
- 즉 **conns≥1 + fanout 증가**면 "받아서 client 로 전달" 성립.

## 3. 이중 축 대사 (FEP 축 ↔ MCI 축)

같은 종목·시각의 값을 두 축에서 비교 (C 피드 = 정답지):
- **FEP 축(C)**: 레거시 C 피드가 SHM(`/dev/shm/mfsise`) 또는 push 로 노출하는 현재가.
  (C 피드 운영 도구 `fut_real`/`fcheg_file` 로 덤프)
- **MCI 축(Go)**: `krx-tester` 의 `fut.trade` JSON.
- 파싱 정확성 자체는 이미 `make verify-krx`(C 오라클↔Go, 7종 전필드 일치)로 보장되므로,
  FEP 테스트에서는 **동일 종목이 두 축 모두에 흐르는지(누락 없는지)** 와 값 일치를 확인.

같은 호스트 공동 수신 조건: **양 축 소켓 모두 `SO_REUSEADDR`**. Go(MCI)는 설정함.
C 피드 RECV 가 미설정이고 먼저 bind 했으면 mci-edge-krx join 이 실패할 수 있음 → §5.

## 4. 판정 기준

| 항목 | 기대 |
|-----|------|
| FEP 도달 | tcpdump 로 227.10.20.10 패킷 관측 |
| MCI 수신 | mcast stats `packets` 증가 |
| MCI 파싱 | `fanout` ≈ packets (unknown 미증가) |
| ws | krx-tester 가 구독 종목 envelope 수신 |
| 이중 축 | 동일 종목이 FEP 축·MCI 축 모두에 출현, 값 일치 |

## 5. 트러블슈팅

- **tcpdump 는 잡히는데 stats packets=0**: IGMP join iface 불일치. mci-edge-krx 를
  `--mcast-iface eth0`(tcpdump 에서 본 iface)로 재기동. `ip maddr show | grep 227.10.20.10`
  으로 join 확인. `rp_filter` 로 mcast drop 가능 → `sysctl net.ipv4.conf.all.rp_filter=2`.
- **mci-edge-krx join 실패 / bind 오류**: C 피드 RECV 와 포트 충돌.
  Go 는 `SO_REUSEADDR` 사용 — C 피드 RECV 도 `SO_REUSEADDR`(또는 `SO_REUSEPORT`)여야
  같은 group:port 공동 수신. 불가 시 mci-edge-krx 를 mcast 도달하는 별도 호스트에 배치.
- **unknown 만 증가**: 미지원 TR(G706F/G701K/H306F 외 결합 TR). 현재 파서 대상은
  A306F/B606F/A006F/H306F/A301K/B601K/A001B (docs/krx-sise-design.md §11).
- **전일대비가 계속 0**: A006F 마스터 미수신(FEP 가 마스터를 안 보냈거나 60641 미도달).
  마스터 도착 후 후속 체결부터 채워짐.
- **종목이 하나도 안 뜸(ws)**: 구독 종목코드 불일치. tcpdump `-A` 로 실제 코드 확인 후
  `--symbols` 에 정확히 지정 (fan-out 은 종목구독 기반).

## 6. 종료 / 정리

```bash
pkill -f './bin/mci-edge-krx'          # 관측용 프로세스 종료 (systemd 아님)
```

## 관련
- `docs/krx-live-verify.md` — 라이브/재생 e2e 런북 (경로 A/B) + KRX mcast 카탈로그
- `docs/krx-sise-design.md §11` — 정답지 대사 (오프셋/수식/런타임 C↔Go)
- `make verify-krx` (C 오라클↔Go 값 대조) / `make krx-e2e` (재생 e2e)
- `cmd/krx-tester` (ws 덤퍼) / `cmd/krx-verify` (gen·decode·replay)
