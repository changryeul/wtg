# WTG 배포 소프트웨어 명세

> WTG (Winway Trading Gateway) 를 운영 환경에 배포할 때 **설치해야 하는 모든 소프트웨어** 의 단일 출처.
> 본 문서가 빠뜨린 항목은 곧 운영 사고 — 새 의존성이 도입되면 본 파일을 먼저 갱신.

대상 환경:
- **운영 서버 (Internal/DMZ)** — Linux (RHEL 8+/Ubuntu 22.04+)
- **빌드 머신** — Linux 또는 macOS (cross-compile)
- **운영자 워크스테이션** — 브라우저 + admin UI 접근

---

## 1. 한눈에 — 배포 토폴로지

```
                ┌────────────────────────────────────────────────────────────┐
                │  운영자 / 외부 클라이언트                                    │
                │  - 브라우저 (admin UI 9090)                                 │
                │  - 외부 매매 시스템 (HTTPS 8090, ws 8083/8084/8087)         │
                └─────────────────┬──────────────────────────────────────────┘
                                  │ HTTPS / WSS  (TLS termination at DMZ)
                                  ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  DMZ                                                                        │
│    mci-edge-api    :8090    TLS + JWT + IP allowlist + rate-limit          │
│    mci-edge-price  :8083    gRPC-WS bridge (raw tick + Profile + 5L)       │
│    mci-edge-push   :8084    PushService fan-out                             │
│    mci-edge-chart  :8087    mci-chart reverse proxy                         │
└─────────────────┬───────────────────────────────────────────────────────────┘
                  │ mTLS (선택) / 사설망 직결
                  ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│  Internal                                                                   │
│    mci-api    :8080  (인증/매매 envelope)                                   │
│    mci-push   :8081  (broker rep receiver + HTTP push, mTLS/secret)        │
│    mci-price  :8082 / :50051  (시세 fan-out + 마진 + QuoteID + swap-lock)  │
│    mci-chart  :8086  (TimescaleDB historical 봉 + 라이브 SubscribeBar)     │
│    mci-admin  :9090  (운영 콘솔)                                            │
│    quote-forwarder  UDP :30044~30051  (UDP FIX → broker/gRPC publish)      │
└──┬───────────────────────────────┬───────────────────────────────┬─────────┘
   │ mymq wire                     │ gRPC                          │
   ▼                               ▼                               ▼
┌──────────────┐         ┌──────────────────┐           ┌────────────────┐
│  mymqd       │         │  etcd cluster    │           │  Redis         │
│  + 매매 AP   │         │  (3/5 node)      │           │  (Sentinel /   │
│  (C engine)  │         │  TLS / mTLS      │           │  Cluster)      │
└──────────────┘         └──────────────────┘           └────────────────┘
   │
   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  관측 / 영속 (선택이지만 강력 권장)                                       │
│    TimescaleDB / PostgreSQL  (quote_bars)                                │
│    Prometheus + Alertmanager  (메트릭 + 알람)                            │
│    Grafana                    (대시보드)                                  │
│    OpenTelemetry Collector    (trace 수집)                               │
│    Tempo / Jaeger             (trace 저장)                               │
│    Loki                       (로그 집계)                                 │
└──────────────────────────────────────────────────────────────────────────┘
```

각 박스마다 어떤 소프트웨어가 무슨 역할로 필요한지 §2~7 에서 자세히.

---

## 2. WTG 자체 산출물

`make build` 로 생성되는 산출물. 운영 서버에는 **바이너리만** 배포 — Go runtime 안 필요.

### 2.1 Go 바이너리 (Linux x86_64 / arm64)

| 바이너리 | 영역 | 포트 | 역할 |
|---|---|---|---|
| `mci-edge-api` | DMZ | 8090 | TLS + JWT + IP allowlist + rate-limit (외부 HTTPS) |
| `mci-edge-push` | DMZ | 8084 | mci-push fan-out → 외부 ws |
| `mci-edge-price` | DMZ | 8083 | raw tick broadcast + Profile/5L customer quote |
| `mci-edge-chart` | DMZ | 8087 | mci-chart reverse proxy |
| `mci-api` | Internal | 8080 | `/v1/tx` `/v1/login` `/v1/refresh` |
| `mci-push` | Internal | 8081 | broker rep receiver + HTTP push (mTLS / secret) |
| `mci-price` | Internal | 8082 / 50051 | 시세 fan-out + 마진 + QuoteID + swap-lock + BestConsumer + Aggregator + Archiver + PricingConsumer |
| `mci-chart` | Internal | 8086 | TimescaleDB historical 봉 + 라이브 SubscribeBar |
| `mci-admin` | Internal | 9090 | 운영 콘솔 + 라우팅/정책/카탈로그 CRUD |
| `quote-forwarder` | Internal | UDP 30044~30051 | UDP FIX → broker / gRPC publish |
| `mci-test` | 검증 | — | Phase 1 ckey echo (GO/NO-GO) |
| `load-gen` | 검증 | — | UDP 시세 부하 생성기 |
| `dev-bar-faker` | 검증 | — | gRPC SubscribeBar mock (chart 단독) |
| `quote-diff` | 검증 | — | 두 ws envelope 비교 (legacy/best 검증) |
| `fx-sync` | 운영 도구 | — | 외환 운영 DB → etcd 미러 CLI |

**빌드** : `make build` → `build/bin/*`
**배포** : `make install PREFIX=/opt/wtg` → `/opt/wtg/bin/*`, `/opt/wtg/etc/*`

### 2.2 정적 자산 (`etc/`)

- `etc/symbols.json` — pair × symbol 매핑 (정적 모드 / etcd 비활성 시)
- `etc/pricing.json` — PricingTable 초기 seed
- `etc/profiles.json` — Profile 목록 seed
- `etc/sql/quote_bars.sql` — TimescaleDB hypertable + 압축 + retention 정책
- `etc/grafana/mci-price-swaplock-alerts.yml` — Prometheus rule (swap_lock 부분실패율 등)

### 2.3 C SDK (외부 매매 엔진 측에 배포)

운영 C 매매 엔진이 broker 우회 push / 시세 잠금을 호출할 때 사용. mymq 의존성 0, POSIX socket + HTTP/1.1 minimal.

| 산출물 | 빌드 명령 | 용도 |
|---|---|---|
| `cside/wtgpush/libwtgpush.a` + `wtgpush.h` | `make cside` | C svc → `POST mci-push HTTP push` |
| `cside/wtgprice/libwtgprice.a` + `wtgprice.h` | `make wtgprice` | 매칭 엔진 → `POST /v1/quote/swap/lock` |

운영 매매 엔진 (AIX / Solaris / HP-UX / Linux / Darwin) 에서 그대로 빌드. wire 호환은 `make test-cside` / `make test-wtgprice` 로 검증.

---

## 3. 필수 외부 의존성 (Production blocker)

이게 없으면 **운영이 동작하지 않는다**. 모든 항목 클러스터/HA 권장.

### 3.1 MyMQ broker + 매매 AP (mymqd)

- **무엇** — WTG 의 매매 backend. C 로 작성된 wire protocol broker + AP 들 (`test_service`, `WECHO`, `W*` / `BW*`)
- **위치** — `/Users/winwaysystems/mywork/mymq` (소스), 운영은 별도 배포 단위
- **포트** — broker `:11217`, cluster `:11218`
- **수정** — **WTG 코드와 별개의 비즈니스 로직**. wire schema 확장 (예: mqhdr 끝의 `trcid[16]`) 같은 인프라 변경만 양측 동시 deploy
- **HA** — broker cluster (active-standby + cluster port)
- **로그** — `DEV_MAIN_LOG` (구조화 log, `../dev-main.md`)
- **TLS** — `../broker-tls.md` 의 합의안
- **재연결** — WTG 측 supervisor goroutine (`../broker-reconnect.md`)

### 3.2 etcd

- **무엇** — 라우팅 룰 / 정책 / 시세 카탈로그 (symbols / pricing / profiles / currency / pair-master) 저장소
- **버전** — 3.5+
- **클러스터** — 3 또는 5 node (홀수, quorum)
- **TLS / mTLS** — `--etcd-tls-cert / -key / -ca / -sni`. 모든 WTG 서비스가 같은 PKI
- **권한** — root 외 별도 user 권장 (mci-admin write, 그 외 read-only)
- **스토리지** — SSD, snapshot 자동 (retention 1 week+)
- **버킷 키 prefix** — `wtg/routes/`, `wtg/policy`, `wtg/pricing/`, `wtg/quoteid/`, `wtg/ratelimit/`
- **백업** — etcdctl snapshot save 매일
- **모니터링** — `up{job="etcd"}`, raft leadership, db size

설치 (Linux):
```bash
# RPM/DEB 또는 binary
wget https://github.com/etcd-io/etcd/releases/download/v3.5.21/etcd-v3.5.21-linux-amd64.tar.gz
# 또는 RHEL: dnf install etcd
# Ubuntu:    apt install etcd-server
```

### 3.3 Redis

- **무엇** — 세션 + cookie_t (매매 엔진 발급) + quoteid Registry (active-active 공유) + idempotency key store
- **버전** — 7.0+
- **HA** — Sentinel (3+) 또는 Cluster
- **AUTH** — password 필수, ACL 권장 (서비스별 user)
- **TLS** — `--tls-port` + cert
- **persistence** — AOF (everysec) — 세션/쿠키는 재시작 후 복구 필요
- **메모리** — quoteid Registry 가 quoteid × validity (보통 500ms) × rate 만큼. 운영 부하 측정 후 결정
- **maxmemory-policy** — `noeviction` (cookie 잃으면 모든 사용자 재로그인)
- **사용 위치** — `pkg/auth.RedisStore`, `pkg/quoteid.RedisRegistry` (SwapIndex 포함), `pkg/idempotency.Redis`

설치 (Linux):
```bash
# RHEL: dnf install redis
# Ubuntu: apt install redis-server
# 또는 redis.io 의 공식 패키지
```

### 3.4 TLS 인증서 / PKI

- **DMZ TLS** — 외부에 노출되는 edge-api / edge-push / edge-price / edge-chart 의 server cert. Let's Encrypt 또는 사내 CA
- **mTLS (내부)** — Internal ↔ DMZ, gRPC, etcd, Redis 모두 mTLS 권장. 사내 CA 로 통일
- **broker TLS** — `../broker-tls.md`. broker 와 WTG 가 같은 사내 CA
- **회전 정책** — cert 6개월~1년. `../push-secret-rotation.md` 의 회전 절차 참조 (secret 도 동일 패턴)
- **저장** — `/etc/pki/wtg/` (mode 0600, wtg user 만 read)

도구: `step-ca`, `cert-manager`, HashiCorp Vault PKI 등.

### 3.5 운영 OS (Linux)

- **버전** — RHEL 8/9 또는 Ubuntu 22.04 LTS+
- **glibc** — Go 빌드한 GLIBC 와 호환
- **systemd** — 서비스 lifecycle 관리 (절대 nohup 으로 띄우지 말 것)
- **firewalld / nftables** — DMZ ↔ Internal ↔ 외부 경계 명시
- **fail2ban** — login brute force 방어 (DMZ)
- **chrony / ntp** — 시각 동기 (시세 latency 측정 정확도, broker mqhdr ts 일관성)
- **logrotate** — WTG 서비스 로그 일별 회전
- **journald** — 구조화 log 수집

---

## 4. 강력 권장 외부 의존성

없어도 WTG 자체는 동작하지만, 운영 사고 시 진단/복구가 거의 불가능해진다. **운영 배포 시 사실상 필수.**

### 4.1 TimescaleDB / PostgreSQL (mci-chart 의 historical 봉)

- **무엇** — `quote_bars` hypertable (1분+ 봉 영속). mci-price.Archiver INSERT, mci-chart.Repository SELECT
- **버전** — PostgreSQL 14+ + TimescaleDB 2.13+
- **HA** — streaming replication 또는 Patroni 클러스터
- **스키마** — `etc/sql/quote_bars.sql` 적용 (CREATE TABLE + create_hypertable + 압축 정책 + add_compression_policy)
- **chunk** — 1일 interval (default)
- **압축** — 7일 후 자동 (90%+ 압축률)
- **retention** — `add_retention_policy` 로 N개월 보관 후 자동 삭제
- **연결** — pgx 풀 (default 10), `--pool` flag
- **차트 없으면** — mci-chart / mci-edge-chart 안 띄우면 됨. mci-price 의 Archiver 도 비활성

설치:
```bash
# Timescale 공식 repo + apt install timescaledb-2-postgresql-16
# 또는 docker run timescale/timescaledb:latest-pg16
```

**참고** — TimescaleDB 가 어렵다면 일반 PostgreSQL + 평범 table 로도 동작 (`etc/sql/quote_bars.sql` 의 hypertable / 압축 / retention 줄 제거). 단 데이터 누적되면 성능 하락.

### 4.2 Prometheus

- **무엇** — `/metrics` 시계열 수집 + PromQL. mci-admin 운영 모니터링 페이지의 카드/sparkline 의 source
- **버전** — 2.51+ (3.x 권장)
- **scrape targets** — 모든 WTG 바이너리 + 외부 의존성 (etcd, Redis, postgres_exporter 등)
- **scrape_interval** — 5s (시세) ~ 15s (정책)
- **retention** — 15일 단기 + Thanos / Mimir 로 장기 보관
- **rule files** — `etc/grafana/mci-price-swaplock-alerts.yml` 등 alert rule
- **mci-admin 연결** — `--prom-url http://prometheus.internal:9091`

설치:
```bash
# 공식 binary 또는 prom/prometheus docker image
# RHEL: dnf install prometheus  (EPEL)
# 또는 brew install prometheus (개발)
```

### 4.3 Alertmanager

- **무엇** — Prometheus alert 의 라우팅/그룹/silence/notify (Slack / PagerDuty / Email)
- **수신처** — 운영팀 Slack 채널 + 야간 oncall 폰
- **silence** — 정비창 동안 알람 자동 silence 권장

### 4.4 Grafana

- **무엇** — Prometheus / Loki / Tempo 위의 시각화 + alert UI. mci-admin 의 firing alerts banner 의 source
- **버전** — 10.4+
- **dashboards** — `../monitoring.md`, `../push-monitoring.md` 의 panel 명세를 dashboard 로 작성
- **alerts** — Prometheus rule 과 통합 — `/api/prometheus/grafana/api/v1/rules` 가 mci-admin 의 `wtg-*` 그룹 source
- **mci-admin 연결** — `--grafana-url http://grafana.internal:3000` + Basic auth 또는 SSO

### 4.5 OpenTelemetry Collector

- **무엇** — WTG 의 OTel gRPC export 수신 → Tempo/Jaeger 로 전달 + 샘플링
- **버전** — collector 0.95+
- **수신** — OTLP gRPC (4317) 또는 HTTP (4318)
- **샘플링** — head-based 1~5% (운영 부하 따라)
- **trace_id** — mymq mqhdr 의 `trcid[16]` 와 호환 (broker 까지 trace 전파)
- **연결** — 각 WTG 서비스 `--otel-endpoint otel-col.internal:4317`

### 4.6 Tempo / Jaeger

- **무엇** — 분산 trace 저장. mci-admin 의 매매 감사 페이지 trace_id 클릭으로 점프
- **선택** — Grafana Tempo (Grafana 생태계) 또는 Jaeger (CNCF)
- **retention** — 7~14일

### 4.7 Loki

- **무엇** — WTG 서비스 로그 집계
- **input** — Promtail / Vector agent 가 journald → Loki
- **label** — service, instance, severity, trace_id
- **연결** — Grafana 의 Explore 에서 로그 확인

---

## 5. 선택 / 보조 도구

운영 안정성/편의를 더 끌어올리는 도구들. 필수는 아님.

| 도구 | 용도 |
|---|---|
| **etcd-keeper / etcdctl** | etcd key 직접 조회/편집 (운영자 점검용) |
| **redis-cli** | Redis 직접 점검 |
| **psql** | TimescaleDB 직접 점검 |
| **HashiCorp Vault** | secret / PKI 통합 관리 |
| **HAProxy / nginx** | 외부 LB (mci-edge-* 앞단 — TLS 1.3 / HTTP/2 / WS) |
| **Cloudflare / 사내 WAF** | DDoS / 봇 방어 |
| **Ansible / Terraform** | 인프라 IaC |
| **Argo CD / FluxCD** | k8s 환경 GitOps |
| **Kubernetes** | container orchestration (선택 — bare-metal systemd 도 가능) |

---

## 6. 빌드 머신 (Build host)

운영 서버에는 binary 만 가지만, 빌드 머신에는 다음이 필요:

### 6.1 필수

| 도구 | 버전 | 용도 |
|---|---|---|
| **Go** | 1.23+ (1.25 권장) | `make build`. CI 는 1.23 으로도 통과해야 — 1.24+ 전용 API 도입 시 주의 |
| **make** | GNU 4.x | Makefile |
| **git** | 2.30+ | 소스 받기 |
| **C 컴파일러** | gcc 9+ / clang 13+ | cside C SDK 빌드 (선택) |
| **bash** | 4.x+ (macOS 면 brew install bash) | scripts/* — 일부 associative array 사용 |
| **curl / jq** | 최신 | scripts/load-test.sh 등 |

### 6.2 protobuf 갱신 시

| 도구 | 버전 | 용도 |
|---|---|---|
| **protoc** | 23+ | `.proto` 컴파일 |
| **protoc-gen-go** | 1.34+ | `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` |
| **protoc-gen-go-grpc** | 1.4+ | `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` |

`make proto` 실행 시 위 3개 필요.

### 6.3 CI

| 도구 | 용도 |
|---|---|
| **staticcheck** | `make lint` 의 staticcheck (`go install honnef.co/go/tools/cmd/staticcheck@latest`) |
| **govulncheck** | `make vulncheck` (`go install golang.org/x/vuln/cmd/govulncheck@latest`) |
| **embedded etcd** | 통합 테스트 `test/etcdtest` 자동 사용 — 별도 설치 불필요 |

### 6.4 cross-compile (macOS 빌드 → Linux 배포)

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build
```

CGO 가 0 이라 glibc 무관. 운영 서버에 Go runtime 안 필요.

---

## 7. 환경별 설치 명령 모음

### 7.1 RHEL 8/9 (운영 서버, 권장)

```bash
# 시스템 도구
sudo dnf install -y systemd-resolved chrony fail2ban logrotate firewalld

# etcd
sudo dnf install -y etcd

# Redis
sudo dnf install -y redis

# PostgreSQL 16 + TimescaleDB
sudo dnf install -y postgresql16-server postgresql16-contrib
# TimescaleDB: timescale.com 의 yum repo 추가 후
sudo dnf install -y timescaledb-2-postgresql-16

# Prometheus + Alertmanager (EPEL)
sudo dnf install -y epel-release
sudo dnf install -y prometheus alertmanager

# Grafana (공식 repo)
# /etc/yum.repos.d/grafana.repo 추가 후
sudo dnf install -y grafana

# OpenTelemetry Collector (공식 binary 또는 docker)
# (RPM 없으면 .tar.gz 또는 image 권장)
```

### 7.2 Ubuntu 22.04 LTS

```bash
sudo apt update
sudo apt install -y chrony fail2ban logrotate ufw

sudo apt install -y etcd-server
sudo apt install -y redis-server

# PostgreSQL 16
sudo apt install -y postgresql-16 postgresql-contrib-16
# TimescaleDB
sudo add-apt-repository -y ppa:timescale/timescaledb-ppa
sudo apt install -y timescaledb-2-postgresql-16

# Prometheus / Alertmanager / Grafana 는 공식 repo 또는 binary
```

### 7.3 macOS (개발 / 빌드 머신)

```bash
brew install go protobuf jq make bash watch
brew install postgresql@16 redis etcd prometheus grafana
brew install golangci-lint  # 선택

# protoc plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

### 7.4 Docker Compose (테스트 / 스테이징)

```yaml
services:
  etcd:
    image: quay.io/coreos/etcd:v3.5.21
    command: etcd --advertise-client-urls http://etcd:2379 --listen-client-urls http://0.0.0.0:2379
  redis:
    image: redis:7-alpine
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    environment:
      POSTGRES_PASSWORD: ...
  prometheus:
    image: prom/prometheus:v3.0.0
    volumes: [./prometheus.yml:/etc/prometheus/prometheus.yml]
  grafana:
    image: grafana/grafana:11.4.0
  alertmanager:
    image: prom/alertmanager:v0.27.0
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.95.0
  tempo:
    image: grafana/tempo:2.6.0
  loki:
    image: grafana/loki:3.3.0
```

### 7.5 Kubernetes 매니페스트 (선택)

- WTG binary 들은 statically linked → distroless container 추천 (`gcr.io/distroless/static`)
- 운영 권장 : Helm chart 또는 ArgoCD GitOps
- 시크릿 : Sealed Secrets / External Secrets Operator + Vault

---

## 8. 부트스트랩 순서 (first-time)

순서가 어긋나면 서비스가 startup gate 에서 fail 함. 다음 순서가 안전:

```
1. OS 준비   (chrony 동기, firewalld 룰, wtg user/group, /etc/pki/wtg/ TLS 배치)
2. etcd 클러스터   (모든 노드 ready 확인 — endpoint health)
3. Redis  (Sentinel/Cluster ready)
4. TimescaleDB  (DB 생성 + quote_bars 스키마 적용)
5. mymqd broker + 매매 AP  (port 11217 listen, ckey echo 테스트)
6. WTG 핵심 :
     5.1 mci-admin   (dev-seed 라우팅 룰 + 정책 doc + 카탈로그 초기 입력)
     5.2 mci-api / mci-push / mci-price / mci-chart  (모두 동시 가능)
     5.3 mci-edge-{api,push,price,chart}  (5.2 ready 후)
7. quote-forwarder  (UDP feed source 가 ready 된 후)
8. 관측 stack : Prometheus → Alertmanager → Grafana → OTel collector → Tempo/Loki
9. 검증 :
     mci-test --ckey-echo                 (Phase 1 GO/NO-GO)
     curl /v1/admin/prom-query?query=up   (모든 target up 인지)
     운영 모니터링 페이지 모든 카드 0 인지
     /tmp/wtg-dev-status.sh (개조해서 운영용으로)
```

---

## 9. 배포 직전 체크리스트

배포 시작 전 모든 항목 ✅ 확인:

- [ ] 모든 WTG 바이너리가 같은 git commit + Go version 으로 빌드됨 (`mci-* --version`)
- [ ] OS 시각 동기 — `chronyc tracking` 모든 노드
- [ ] etcd quorum 정상 (`etcdctl endpoint health` 3/5 node OK)
- [ ] Redis 모든 master 살아있음 + failover 테스트 1회 완료
- [ ] TimescaleDB `quote_bars` 스키마 적용 + 압축/retention 정책 활성
- [ ] mymqd broker 와 ckey echo 정상 (`mci-test --ckey-echo`)
- [ ] TLS cert 만료까지 60일+ 남음, broker / etcd / Redis 모두
- [ ] PKI CA / intermediate 가 모든 노드에 배포됨
- [ ] systemd unit 파일 `/etc/systemd/system/wtg-*.service` 배치 + enable + `--restart on-failure`
- [ ] logrotate `/etc/logrotate.d/wtg` 일별 회전 설정
- [ ] firewalld / nftables — DMZ ↔ Internal 의 포트만 열림. 그 외 closed
- [ ] Prometheus 모든 scrape target `up=1`
- [ ] Alertmanager 의 Slack/PagerDuty webhook 동작 (테스트 alert)
- [ ] Grafana dashboard import 완료 + 권한 설정
- [ ] OTel collector 가 sample trace 받음
- [ ] mci-admin 에서 dev-seed 가 아닌 운영 라우팅 룰 / 정책 / 카탈로그 적용됨
- [ ] PricingTable (HQ/Site/Customer/Window/Swap 5 layer) 모두 적절 — `margin-calc` 페이지로 샘플 시뮬레이션
- [ ] `users` 페이지에서 운영자 계정 + role 부여
- [ ] `quoteid-engines` 페이지에서 매매 AP 엔진 등록 + secret 안전 보관
- [ ] 운영 SOP (`../operations.md`) 가 운영팀에 공유됨
- [ ] 비상 시 Kill Switch 절차 + Maintenance Window 입력 절차 운영자가 숙지
- [ ] 백업 — etcd snapshot / Redis AOF / TimescaleDB pg_dump 자동화 동작
- [ ] DR — 다른 region 또는 다른 datacenter 에 cold standby (선택)

---

## 10. 참고 문서

본 문서가 가리키는 다른 명세:

- `../mci-architecture.md` — 컴포넌트 흐름
- `../operations.md` — 서비스별 flag/env + mci-admin 운영 작업
- `../conventions.md` — ApplName / Channel / Exchange / RoutingKey / Queue 카탈로그
- `../auth.md` — JWT + Redis store + Session.Profile + cookie_t passthrough
- `../cooker-quote-schema.md` — UDP FIX → broker → mci-price 시세 wire
- `../cooker-patch.md` — Cooker 가 myrqd + mymqd 양쪽 publish 패치
- `../chart-schema.md` — TimescaleDB hypertable 설계
- `../margin-business-spec.md` — 마진 업무 정의
- `../margin-policy.md` — Pricing Policy 명세
- `../swap-trade-spec.md` — FX swap 2-leg 잠금 (Phase S3)
- `../broker-tls.md` — broker TLS 합의안
- `../broker-tracing.md` — mqhdr 의 trace_id 확장
- `../broker-reconnect.md` — supervisor goroutine 재연결 정책
- `../broker-sigabrt-analysis.md` — broker SIGABRT 부하 사고 사후 분석
- `../push-secret-rotation.md` — HTTP push secret 회전 절차
- `../quoteid-validation-rfc.md` — quote_id 검증 RFC (sync/async 모드, store 선택)
- `../quoteid-operations.md` — quote_id allowlist 운영
- `../mci-price-ha.md` — mci-price 다중 인스턴스 HA
- `docs/phase-2.7-rollout.md` — broker 우회 HTTP push rollout
- `../observability.md` — 운영 진단/관측 통합 가이드
- `../monitoring.md` — Prometheus / Grafana 가이드
- `../push-monitoring.md` — mci-push 가시화 dashboard / rules
- `../cs-ws-migration.md` — legacy cs framework → mci-edge-price ws 마이그레이션
- `admin-ui-manual.md` — admin UI 37 페이지 운영 매뉴얼
- `../admin-ui-test-guide.md` — admin UI 페이지별 테스트 시나리오
- `../testing.md` — 단위/통합/e2e 단계별
- `../roadmap.md` — Phase 1~9 일정
