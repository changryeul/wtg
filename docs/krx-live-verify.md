# KRX 시세 트랙2 실전 검증 (mci-edge-krx)

WTG 가 KRX 원 TR 을 직접 파싱하는 트랙2(`mci-edge-krx --mcast`)를 **실 바이너리**로
검증하는 런북. 두 경로:

- **경로 A — 라이브 (장중)**: 실제 KRX 파생/채권 멀티캐스트를 수신해 관측.
- **경로 B — 재생 (장외 언제나)**: 결정적 캡처를 UDP 멀티캐스트로 재생 (`scripts/krx-replay-e2e.sh`).

값 정확성 자체는 정적 대사(`docs/krx-sise-design.md §11`)와 런타임 대조(`make verify-krx`,
C 오라클↔Go)로 이미 보장된다. 본 문서는 **소켓 수신 → 파싱 → ws fan-out 파이프라인**을
실 배포 형태로 확인하는 절차다.

## 0. KRX 멀티캐스트 카탈로그 (sise `cfg/*.conf` 기준)

| 시장 | 그룹 | 포트 | 내용 | 운영시간 |
|-----|------|-----|------|---------|
| 파생 | 227.10.20.10 | 60641 | A0 (A006F 종목마스터) | 08:00–15:45 |
| 파생 | 227.10.20.10 | 60642 | A3·G7·B6 (A306F 체결/B606F 호가/G706F) | |
| 파생 | 227.10.20.10 | 60643 | 기타 (H306F 정산가 등) | |
| 채권 | 227.10.20.10 | 60631 | A3·G7·B6 (A301K 체결/B601K 호가) | 08:00–15:30 |
| 채권 | 227.10.20.10 | 60632 | 기타 (A001B 종목마스터) | |

`mci-edge-krx` 기본값(`internal/krx/config.go`)이 이미 5포트 전부 join → 선물+채권 커버.

> **중요 (EC2 망 제약)**: `227.10.20.10` 은 KRX 회원 전용 회선에서만 흐른다. AWS EC2
> public 서브넷에서는 라이브 mcast 가 **직접 도달하지 않을 수 있다**. 라이브 검증은
> 기존 C 피드(`kbfut_sise`)가 이미 그룹을 수신 중인 **동일 호스트**에서 실행하는 것이
> 정석 — Go 의 `ListenMulticastUDP` 는 `SO_REUSEADDR` 를 설정하므로 kbfut_sise 와
> **동시 수신(co-join)** 가능하다. 라이브가 안 되는 환경이면 경로 B 로 검증한다.

## 1. 빌드 (EC2)

```bash
cd /home/winway/common/wtg   # 소스 미러 (project_nh_source_mirror)
make build                                # build/bin/mci-edge-krx 포함
go build -o build/bin/krx-tester ./cmd/krx-tester/
go build -o build/bin/krx-verify ./cmd/krx-verify/
```

## 2. 경로 A — 라이브 (장중, sise 호스트에서)

### 2-1. 라이브 도달 확인 (수신 전 먼저)
```bash
# 그룹 트래픽이 실제로 이 호스트에 오는가 (파생 시세 포트)
sudo tcpdump -ni any host 227.10.20.10 and udp port 60642 -c 5
# IGMP join 상태 / 소켓 확인
ip maddr show ; ss -u -a | grep -E '6064|6063'
```
5초 안에 패킷이 잡히면 라이브 수신 가능. 안 잡히면 → 망 제약(경로 B로).

### 2-2. mci-edge-krx 기동
```bash
# 수신 인터페이스가 여러 개면 --mcast-iface 로 명시 (tcpdump 에서 확인한 iface).
./build/bin/mci-edge-krx --mcast --listen :8085
# 기본 group/ports = 227.10.20.10 / 60641,60642,60643,60631,60632
# 30초마다 "KRX mcast stats" 로그: packets/fanout/unknown/read_errs
```

### 2-3. ws 수신 관측
```bash
# 그날 상장 근월물 코드로 구독 (예: KOSPI200 선물 근월물). tcpdump 로 code 확인 가능.
./build/bin/krx-tester --url ws://127.0.0.1:8085/v1/subscribe \
    --symbols 101V6000 --timeout 20s
```
- `fut.trade`/`fut.book` 이 실시간으로 흐르고 `last/diff/rate/sign` 이 채워지면 OK
  (전일대비는 A006F 마스터 수신 후부터 — 장 초 마스터가 먼저 와야 diff≠0).
- 서버측: `GET /healthz` (conns), stats 로그의 `fanout` 증가 확인.

## 3. 경로 B — 재생 (장외, 어디서나)

```bash
scripts/krx-replay-e2e.sh
# gen → replay(UDP 239.9.9.9) → mci-edge-krx --mcast → parse → ws → krx-tester
# [OK] 실 바이너리 e2e 통과 … 가 나오면 파이프라인 정상.
```
수동으로 실제 그룹/포트를 흉내내려면:
```bash
./build/bin/krx-verify gen /tmp/cap.dat
./build/bin/mci-edge-krx --mcast --mcast-group 239.9.9.9 --mcast-ports 61099 --listen :8085 &
./build/bin/krx-tester --url ws://127.0.0.1:8085/v1/subscribe \
    --symbols 101V6000,KR1035020310,201S3000 --count 6 &
./build/bin/krx-verify replay /tmp/cap.dat 239.9.9.9:61099 5 100ms
```

## 4. 판정 기준

| 항목 | 기대 |
|-----|------|
| edge 기동 | "KRX 멀티캐스트 수신 시작" 로그 (group/ports/iface) |
| 수신 | stats `packets` 증가, `unknown` 미증가(≈0) |
| 파싱 | `fanout` == packets (미지원 TR 없으면) |
| ws | krx-tester 가 구독 종목의 envelope 수신 (rc=0) |
| 필터 | 미구독 종목은 수신 안 됨 (종목구독 fan-out) |

## 5. 트러블슈팅

- **패킷 안 잡힘**: iface 불일치. `--mcast-iface eth0` 명시. 송신자와 수신자가
  같은 인터페이스여야 join 유효 (로컬 테스트 시 iface 생략 = 기본 라우팅).
- **stats packets=0 인데 tcpdump 는 잡힘**: IGMP join 이 iface 에 안 걸림 → iface 명시.
  방화벽/`rp_filter`(reverse path) 로 mcast drop 가능 → `sysctl net.ipv4.conf.all.rp_filter`.
- **unknown 증가**: 5바이트 TR코드가 미지원(G706F/G701K 등 미구현) — 정상, 해당 TR 은
  아직 파서 없음. fanout=packets-unknown.
- **kbfut_sise 와 포트 충돌**: Go 는 `SO_REUSEADDR` 로 co-join 하므로 충돌 아님.
  그래도 안 되면 kbfut_sise 가 `SO_REUSEADDR` 없이 bind 했는지 확인.
- **EC2 public 에서 라이브 0**: 예상된 망 제약 — 경로 B 로 검증하고, 라이브는 sise
  호스트/온프렘에서.

## 6. 관련
- `docs/krx-sise-design.md` — 트랙2 설계 + §11 정답지 대사
- `make verify-krx` — C 오라클↔Go 값 런타임 대조 (파싱 정확성)
- `cmd/krx-tester` — ws 수신 덤퍼 / `cmd/krx-verify` — gen·decode·replay
