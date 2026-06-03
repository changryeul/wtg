# 운영 세팅 (Operations)

> WTG 서비스를 실제로 가동하는 데 필요한 **인프라 전제 + 서비스별 세팅값 +
> mci-admin 의 control plane 작업** 을 한 곳에 모은 운영 레퍼런스. control panel
> 에서 다루는 모든 관리 항목 / 데이터 모델 / 필드별 의미는 **부록 A** 참조.
>
> 상위 문서: `docs/mci-architecture.md` (컴포넌트 흐름), `docs/routing.md`
> (라우팅 상세), `docs/auth.md` (인증 위임), `docs/conventions.md`
> (ApplName/Channel/Exchange 카탈로그).

---

## 0. 전제 인프라

| 항목 | 용도 | 필수 시점 |
|---|---|---|
| **mymqd** (broker, 포트 11217) | 모든 매매/푸시/시세의 통과지점 | 항상 (DevMode 만 예외) |
| **etcd** (단일 또는 cluster) | 라우팅 룰 + 정책 동기화 | 다중 인스턴스 운영 시 필수, 단일이면 in-memory 가능 |
| **Redis** | 세션 / refresh token | 운영. dev 는 `pkg/auth/memstore.go` |
| **매매 AP** (`test_service` / `WECHO` / `W*`/`BW*`) | 실제 transaction 처리 | broker 가 자동 기동하는 것은 broker 가 처리, 별도 AP 는 운영팀이 기동 |
| **TLS 인증서** | mTLS (broker / DMZ↔Internal / 외부 HTTPS) | 운영. dev 는 plain TCP |
| **TimescaleDB** (`quote_bars` hypertable) | 봉 영속 + 압축/retention | mci-chart 활성 시 필수 |

### Broker publish 손실 진단 (HIGH 부하 한정)

64k tick/s 같은 burst 부하에서 forwarder publish 와 broker→mci-price 도달 사이
**~14-18% 손실**이 관측됨. C 엔진 무수정 정책상 broker 안에는 직접 카운터를
못 넣지만 외부 진단 도구:

```bash
./scripts/broker-loss-diag.sh high           # HIGH 시나리오 30s
./scripts/broker-loss-diag.sh mid            # MID
DURATION=15s ./scripts/broker-loss-diag.sh   # 시간 override
```

각 단계 메시지 수와 broker log 카테고리별 빈도를 출력:

```
forwarder UDP recv     :  1,919,820
forwarder published env:  1,309,839
forwarder published msg:    123,716  (envelopes / avg_batch 10.59)
broker→mci-price msg   :    105,842  ( 85.6% of pub msg)
mci-price ticks        :  1,120,590
mci-price sub_drops    :          0

broker side 손실 추정  : 17,874 messages (14.4%)
```

broker log 의 `Published N/M` 카운트가 `forwarder published msg` 보다 적으면
broker 의 `publish_packet` 진입 자체를 못한 것 — 손실은 **forwarder
TCP write → broker TCP recv 사이** (broker 단일 reader thread 가 draining 못
따라감 → kernel TCP recv buffer 적체 → forwarder write 가 timeout 으로 drop).

**완화 옵션** (broker 무수정 제약 안에서):

1. **forwarder pacing** — `--batch-max` 키워서 broker publish 빈도 줄임
   (이미 default 14 까지 튜닝됨, msgb 1512B 한계)
2. **broker TCP recv buffer 키우기** — host sysctl `kern.ipc.maxsockbuf`,
   `net.inet.tcp.recvspace` 상한 늘리기. Docker container 의 broker 도 host
   설정 따라감.
3. **broker 외부 화이트박스 측정** — `tcpdump -i lo port 11217` 으로 패킷
   바이트 audit (forwarder pub bytes vs broker→mci-price bytes 비교)

> 본 손실은 운영의 typical 시세량 (수 k tick/s) 에선 안 보임. burst 부하
> 한정 — load test 시나리오 LOW/MID 는 100% delivery.

### TimescaleDB 운영 점검

`etc/sql/quote_bars.sql` 부트스트랩 후 정책 모니터링:

```sql
-- 정책 jobs 등록 확인
SELECT job_id, application_name, schedule_interval, next_start
FROM timescaledb_information.jobs
WHERE application_name LIKE 'Columnstore%' OR application_name LIKE 'Retention%';

-- 7일 이상 chunk 가 압축됐는지
SELECT chunk_name, range_start::DATE, is_compressed
FROM timescaledb_information.chunks
WHERE hypertable_name='quote_bars'
ORDER BY range_start DESC LIMIT 10;

-- 압축률 실측
SELECT
  pg_size_pretty(SUM(before_compression_total_bytes)) AS uncompressed_eq,
  pg_size_pretty(SUM(after_compression_total_bytes))  AS compressed_actual,
  round((1 - SUM(after_compression_total_bytes)::numeric /
            NULLIF(SUM(before_compression_total_bytes),0)) * 100, 1) AS savings_pct
FROM chunk_compression_stats('quote_bars')
WHERE after_compression_total_bytes IS NOT NULL;
```

기준값 (실측):
- 압축률: ~75% (2584 kB → 648 kB)
- 압축 정책: 7일 이상 chunk, 12h 주기
- retention: 2년 이상 chunk drop, 1일 주기

상세 + 검증 절차: `docs/chart-schema.md` 의 "운영 — 정책 검증 / 모니터링".

---

## 1. 서비스별 핵심 세팅값

flag / env 우선순위는 **flag > env > default**. 모든 서비스 공통으로
`-log-level`, `-dev` 가 존재 — 표에서는 생략.

### 1.1. mci-api — Internal REST (`:8080`)

| flag | env | 의미 | 운영 필수도 |
|---|---|---|---|
| `-broker-host`, `-broker-port` | `WTG_API_BROKER_HOST`, `_PORT` | mymqd 위치 | **필수** |
| `-etcd` | `WTG_API_ETCD` | etcd 엔드포인트 (콤마) — 비면 in-memory | 다중 인스턴스 시 **필수** |
| `-etcd-prefix` | `WTG_API_ETCD_PREFIX` | 라우팅 룰 prefix (default `wtg/routes/`) | mci-admin 과 **반드시 일치** |
| `-etcd-policy-key` | `WTG_API_ETCD_POLICY_KEY` | 정책 키 (default `wtg/policy`) | mci-admin 과 **반드시 일치** |
| `-trust-edge` | `WTG_API_TRUST_EDGE` | `X-WTG-SID` 헤더 신뢰 | mci-edge-api 뒤에서만 ON |
| `-tls-cert/-key/-client-ca` | `WTG_API_TLS_*` | 서버 TLS / mTLS | Internal 망에서도 mTLS 권장 |
| `-broker-tls-*` | `WTG_API_BROKER_TLS_*` | broker 측 TLS | broker 가 TLS listener 시 |
| `-call-timeout` | — | broker Call 기본 timeout (def 5s) | 트랜잭션 응답이 느리면 상향 |
| `-appl`, `-instance` | `WTG_API_APPL`, `_INSTANCE` | ApplName + 인스턴스 일련번호 | 다중 인스턴스 시 instance 다르게 |
| `-dev` | `WTG_API_DEV_MODE` | JWT 우회 (X-WTG-User) | **운영 절대 금지** |
| `-dev-routes-file`, `-dev-routes-policy` | `WTG_API_DEV_ROUTES_FILE/POLICY` | DevMode 시드 JSON + `additive`/`sync` | dev 전용 |
| `-dev-policy-url` | `WTG_API_DEV_POLICY_URL` | DevMode 정책 poll URL (mci-admin) | dev 전용 |
| `-redis` | `WTG_API_REDIS_ADDR` | session/refresh 공유 redis 주소 (단일 host:port 또는 콤마 분리) | **운영 다중 인스턴스 필수** |
| `-redis-password`, `-redis-db`, `-redis-prefix` | `WTG_API_REDIS_*` | redis 인증/DB/키 prefix | 운영 |
| `-redis-mode`, `-redis-master` | `WTG_API_REDIS_MODE/MASTER` | topology (direct/sentinel/cluster) | sentinel/cluster 시 |
| `-idempotency`, `-idempotency-ttl` | — | `Idempotency-Key` 헤더 처리 활성 (def off) / reservation TTL (def 5m). 활성 시 중복 매매 차단 (더블 클릭 / 모바일 retry / 멀티 인스턴스). 현재 memory store — 다중 인스턴스 공유는 Redis 후속 | 운영 권장 |

> Redis 미설정 시 in-memory store. 다중 mci-api 인스턴스 운영 시 한 인스턴스가
> 발급한 refresh token 이 다른 인스턴스 `/v1/refresh` 에서 unknown 으로 거부됨.

**Idempotency-Key**: 활성 시 클라이언트가 `Idempotency-Key: <opaque>` 헤더 첨부
→ 같은 (usid, key) + body 의 재요청은 broker 호출 X + 캐시 응답 +
`Idempotency-Cached: true` 헤더. 같은 키 + 다른 body → 409 `idempotency_conflict`.
같은 키 + 같은 body 의 동시 in-flight → 409 `idempotency_in_flight`.
broker network 에러 (5xx) 는 캐시 X (rollback) — 즉시 재시도 가능. 비즈니스
에러 (422) 는 캐시 — 결정적 응답.

### 1.2. mci-admin — Internal control plane (`:9090`)

| flag | env | 의미 |
|---|---|---|
| `-allow-cidrs` | `WTG_ADMIN_ALLOW_CIDRS` | 사내망 CIDR 화이트리스트. **운영시 필수 채워라** (비면 모두 허용) |
| `-etcd`, `-etcd-prefix`, `-etcd-policy-key` | `WTG_ADMIN_ETCD*` | mci-api 와 **동일한 값** 으로 설정 — 안 그러면 admin 변경이 api 에 안 닿음 |
| `-tls-cert/-key/-client-ca` | `WTG_ADMIN_TLS_*` | 운영자 mTLS — 관리자 PC 발급 client cert 만 허용 |
| `-broker-host`, `-broker-port`, `-broker-tls-*` | `WTG_ADMIN_BROKER_*` | mymqd 위치 + TLS |
| `-svc-inc-dir`, `-svc-common-header` | `WTG_ADMIN_SVC_INC_DIR`, `_SVC_COMHDR` | 매매 svc 헤더 디렉터리/`comhdr.h` — 부팅 시 일괄 파싱 → svc-io UI |
| `-upstream-api` | `WTG_ADMIN_UPSTREAM_API` | mci-api base URL — Tx 테스터용. **dev 전용**, 운영 비활성 |
| `-trust-edge` | `WTG_ADMIN_TRUST_EDGE` | `X-WTG-SID` 신뢰 (사내망 mTLS 환경에서만) |
| `-dev`, `-no-broker` | `WTG_ADMIN_DEV`, `_NO_BROKER` | DevMode + broker 미연결 (UI 시각 검증) |
| `-dev-routes-file`, `-dev-routes-policy` | `WTG_ADMIN_DEV_ROUTES_*` | DevMode 시드 JSON + `additive`/`sync` |

### 1.3. mci-push — Internal WS fan-out (`:8081`)

| flag | env | 의미 |
|---|---|---|
| `-broker-host`, `-broker-port` | `WTG_PUSH_BROKER_*` | mymqd 위치 |
| `-queue` | `WTG_PUSH_QUEUE` | **빈 값 유지 (default).** 비어야 broker 가 `_CLIENT_` 로 등록해 representative receiver 동작 (`publish.c:185-189`) |
| `-grpc` | `WTG_PUSH_GRPC` | `mci-edge-push` 가 붙는 gRPC stream listen (예: `:50052`). 비면 비활성 |
| `-grpc-tls-cert/-key/-client-ca` | `WTG_PUSH_GRPC_TLS_*` | edge↔push mTLS |
| `-broker-tls-*` | `WTG_PUSH_BROKER_TLS_*` | broker TLS |
| `-ws-ping`, `-ws-pong-timeout`, `-send-queue` | — | WS 라이프사이클 + 사용자별 큐 (def 256) |
| `-grpc-buf` | — | gRPC 구독자별 큐 크기 (def 1024) |

### 1.4. mci-price — Internal 시세 (`:8082`)

| flag | env | 의미 |
|---|---|---|
| `-broker-host/-port` | `WTG_PRICE_BROKER_*` | mymqd 위치 |
| `-queue`, `-exchange` | `WTG_PRICE_QUEUE`, `_EXCHANGE` | broker 큐(default `mci_price`) / 필터링 exchange(default `PRICE`) |
| `-grpc` | `WTG_PRICE_GRPC` | edge-price 가 붙는 endpoint (예: `:50051`). 비면 비활성 |
| `-grpc-tls-*` | `WTG_PRICE_GRPC_TLS_*` | edge↔price mTLS |
| `-broker-tls-*` | `WTG_PRICE_BROKER_TLS_*` | broker TLS |
| `-symbols`, `-profiles` | — | SymbolMap / Profile 카탈로그 정적 JSON (etcd 미사용 시) |
| `-best`, `-best-staleness` | — | 다중시장 best 호가 산정 (default ON, stale 30s 후 제외) |
| `-cross-staleness`, `-cross-debounce` | — | cross pair 합성 — leg quote stale 차단 (default 30s) / 같은 cross 중복 emit 윈도우 (default 10ms). PairMaster 에 cross 산식 등록 시 활성 |
| `-pricing-buffer` | — | PricingConsumer 비동기 buffer (0=synchronous, >0=channel+worker). 부하 시 publisher slow 가 broker subscribe upstream 을 block 하는 것 방지. 운영 권장 4096~16384. `/v1/price-stats` 의 `buffer_dropped` 모니터링 |
| `-sub-buffer` | — | broker subscribe 채널 버퍼 (default 256, sub_drops 모니터링 후 조정) |
| `-print N`, `-stats 5s` | — | 진단용 — 처음 N tick stdout / 통계 주기 |
| `-quoteid-instance` | — | QuoteID Generator instance prefix (예: `A`). 비면 quoteid 미발급. multi-instance 환경에서 인스턴스별 다른 prefix 필수 |
| `-quoteid-engines` | — | 매칭 엔진 allowlist 정적 콤마구분 (예: `matching-A,matching-B`). 빈값 = RBAC 비활성 |
| `-quoteid-engines-etcd` | — | etcd watch prefix (예: `wtg/quoteid/engines/`). 채우면 admin UI 의 QuoteID 엔진 페이지 변경이 hot reload. **운영 권장** — allowlist 변경 시 mci-price 재시작 불필요 |
| `-quoteid-redis-addr/-master/-mode/-password/-db` | — | QuoteID Registry — Redis (운영) 또는 비면 Memory (dev) |

**관측 endpoint**:
- `GET /v1/price-stats` — received / matched / dropped / ticks / sub_drops
- `GET /v1/best-stats` — symbol 별 active_sources / best_bid / best_ask / crossed_fallback
- `GET /metrics` — Prometheus
- `GET /debug/pprof/*` — DevMode 한정 CPU/heap/goroutine/mutex/block 프로파일

### 1.5. mci-edge-api — DMZ REST (`:8090`)

| flag | env | 의미 |
|---|---|---|
| `-upstream` | `WTG_EAPI_UPSTREAM` | Internal mci-api base URL (예: `http://10.0.0.20:8080` 또는 https) |
| `-allow-cidrs` | `WTG_EAPI_ALLOW_CIDRS` | 외부 접근 허용 IP (회사·파트너 출구 IP) |
| `-tls-cert/-key/-client-ca` | `WTG_EAPI_TLS_*` | 외부 HTTPS / B2B mTLS |
| `-upstream-tls-cert/-key/-ca/-sni` | `WTG_EAPI_UPSTREAM_TLS_*` | DMZ→Internal mTLS 클라이언트 |
| `-ip-rate`, `-ip-burst` | — | Rate limit **fallback** 한도 (룰 매칭 안 된 path. def 100 TPS / burst 200, 0=비활성) — 자세한 path-aware 룰 카탈로그는 `docs/ratelimit.md` |
| `-etcd` | — | etcd endpoints (콤마 구분). 채우면 정책 hot reload — 비면 컴파일 default 정적 |
| `-etcd-ratelimit-key` | — | etcd PolicyDoc key (def `wtg/ratelimit/edge-api`) |
| `-ratelimit-redis`, `-ratelimit-redis-pass`, `-ratelimit-redis-db` | — | Redis 분산 backend (다중 인스턴스 단일 카운터). 비면 in-memory. 자세히는 `docs/ratelimit.md` §5.5 |
| `-max-body` | `WTG_EAPI_MAX_BODY` | 본문 한도 (def 1 MiB, 0=무제한) |

mci-admin 의 신규 flag:

| flag | env | 의미 |
|---|---|---|
| `-prom-url` | `WTG_ADMIN_PROM_URL` | Prometheus base URL (예: `http://prometheus:9090`). 채우면 admin UI "운영 모니터링" 페이지 카드 활성. 자세히는 `docs/monitoring.md` |
| `-grafana-url` | `WTG_ADMIN_GRAFANA_URL` | Grafana base URL (예: `http://grafana:3000`). 채우면 admin UI 에 firing alert 표시 |
| `-grafana-user`, `-grafana-pass` | `WTG_ADMIN_GRAFANA_USER`, `WTG_ADMIN_GRAFANA_PASS` | Grafana Basic auth (옵션) |
| `-audit-redis` | `WTG_ADMIN_AUDIT_REDIS` | Redis addr — admin audit ring 영속 backend (host:port). 비면 in-memory only (재시작 시 손실) |
| `-audit-redis-pass` | `WTG_ADMIN_AUDIT_REDIS_PASS` | Audit Redis password (옵션) |
| `-audit-redis-db`, `-audit-redis-key`, `-audit-redis-maxlen` | — | DB index / LIST 키 (def `wtg:audit`) / 보존 길이 (def 1000) |

### audit Redis backend

자세한 동작:

- `Push` — Redis 활성 시 `LPUSH key + LTRIM key 0 (maxLen-1)` (pipeline atomic). 실패 → in-memory ring 만 보존 + `failCount` 증가
- `List` — Redis 활성 시 `LRANGE key 0 (limit-1)` 우선. 실패 → in-memory fallback
- 재시작 시 → 새 admin 이 같은 Redis key 에 붙으면 보존된 audit 그대로 조회
- Metric: `wtg_audit_redis_fails_total{service=mci-admin}` Counter — 0 이 아니면 Redis 장애 의심
- Grafana alert (wtg-p7-ratelimit 그룹의 `wtg-audit-redis-fails`): `rate(...[5m]) > 0` for 3m, severity=warning
| `-upstream-timeout` | — | upstream round-trip timeout (def 10s) |

### 1.6. mci-edge-push — DMZ WS (`:8084`) / mci-edge-price — DMZ WS (`:8083`)

두 서비스의 flag 표는 동일 (env prefix 만 다름: `WTG_EPUSH_*` vs `WTG_EPRICE_*`).

| flag | env | 의미 |
|---|---|---|
| `-upstream` | `WTG_EPUSH_UPSTREAM` / `WTG_EPRICE_UPSTREAM` | Internal mci-push/price gRPC 주소 (예: `10.0.0.21:50052`) |
| `-grpc-tls-cert/-key/-ca/-sni` | `*_GRPC_TLS_*` | DMZ→Internal mTLS 클라이언트 |
| `-tls-cert/-key/-client-ca` | `*_TLS_*` | 외부 HTTPS (보통 ingress 가 처리 → 비활성 가능) |
| `-allow-cidrs` | `*_ALLOW_CIDRS` | 외부 접근 허용 CIDR |
| `-ip-rate`, `-ip-burst` | — | IP 단위 rate limit |
| `-subscriber-id` | `*_SUBSCRIBER` | upstream 에 등록될 식별자 (default `<svc>@<host>`) |
| `-send-queue`, `-dial-timeout` | — | ws 클라이언트별 큐 / gRPC dial timeout |

**mci-edge-price 전용**:

| flag | env | 의미 |
|---|---|---|
| `-quote-stream` | `WTG_EPRICE_QUOTE_STREAM` | Profile-routed CustomerQuote stream 활성 (마진 적용된 시세) |
| `-quote-profiles` | `WTG_EPRICE_QUOTE_PROFILES` | 수신 profile 화이트리스트 (콤마, 비면 모두) |
| `-quote-seed-pairs` | `WTG_EPRICE_QUOTE_SEED_PAIRS` | Phase 2 권한 가드 시드 pair |
| `-stale-threshold` / `-stale-scan` | — | Phase 4 stale 감시 (default 30s / 5s) |
| `-envelope-format` | `WTG_EPRICE_ENVELOPE_FORMAT` | ws envelope: `best` (신규) / `legacy` (cs framework 호환) |
| `-customer-stream` | `WTG_EPRICE_CUSTOMER_STREAM` | **Phase 4c** — customer-specific 마진 (HQ+Site+Customer+Window) 활성. ws connect 시 Principal.Usid 를 customer-id 로 mci-price 에 자동 등록. SubscribeCustomerQuote stream 으로 customer-tag 된 quote 수신 → 매칭 ws 클라이언트로 fan-out |
| `-admin-allow-cidrs` | `WTG_EPRICE_ADMIN_ALLOW_CIDRS` | `/v1/admin/*` endpoint 접근 허용 CIDR (비면 거부) |

### 1.7. quote-forwarder — Internal UDP→broker

env 미사용, 모두 flag.

| flag | 의미 |
|---|---|
| `-multi` | 다중 feed 형식 `SMB:30044,KMB:30045,EBS:30046,REUT:30051`. 비면 단일 모드 |
| `-listen`, `-feed` | 단일 모드용 UDP 주소 + 거래소 라벨 |
| `-bind` | 다중 모드 listener bind 주소 (def `127.0.0.1`) |
| `-broker-host`, `-broker-port` | mymqd 위치 |
| `-appl`, `-instance` | broker 등록 ApplName (def `quote-fwd` / 1). feed 별 instance+i 로 자동 분배 |
| `-include-fix` | true 면 audit 로그에 raw FIX 포함 (감사용) |
| `-metrics` | Prometheus + `/stats` + `/debug/pprof/` HTTP listen (예: `127.0.0.1:9091`) |
| `-udp-rcvbuf` | UDP socket SO_RCVBUF (default 4MB, macOS kern.ipc.maxsockbuf 8MB 한계) |
| `-batch-max` | broker message 1회당 envelope 최대 개수 (default 14, msgb 1512B 한계). 1=batch 비활성 |
| `-batch-timeout` | batch 가 -max 에 도달 못해도 이 시간 후 flush (default 10ms) |
| `-feed-buffer` | reader→worker 채널 버퍼 (default 8192). 가득 시 queue_drops 증가 |

**관측 endpoint**:
- `GET /stats` — uptime / received_total / published_total / publish_errors
- `GET /metrics` — feed별 received / published / parse_errors / queue_drops / batch_size histogram / udp_rcvbuf_bytes

**아키텍처 (high-throughput 모드)**:

```
UDP socket (SO_RCVBUF 4MB)
  ↓
reader goroutine (per feed)  ─ pure ReadFromUDP, queue_drops 시 명시적 drop
  ↓ pktCh (buffer 8192)
worker goroutine (per feed)  ─ fastExtractV1 (single-scan FIX) → batch buffer
  ↓ batch flush (max=14 또는 timeout)
quote.EncodePushdataBatch (JSON 배열) → broker publish (per-feed connection)
```

**부하 측정** (load-gen 도구 `cmd/load-gen` + `scripts/load-test.sh`):
- LOW 640 tick/s → delivery 100%
- MID 6.4k tick/s → delivery 100%
- HIGH 64k tick/s → delivery ~62% (broker publisher thread ceiling)

---

## 2. mci-admin 에서 해야 하는 운영 작업

mci-admin 의 control plane endpoint 분류 — 이 중 **2.1 / 2.2 가 서비스 가동에 반드시 필요한 세팅**.

### 2.1. 라우팅 룰 등록 — 필수 (클라이언트 alias 가 동작하려면)

```bash
# alias 등록
PUT /v1/admin/routes/ORDER_NEW
  body: {"exchange":"ORDER","routing_key":"NEW","active":true,"comment":"신규주문"}

# 활성/비활성 토글 (룰은 보존, 트래픽만 차단)
POST /v1/admin/routes/ORDER_NEW/active  body: {"active":false}

# 조회 / 삭제
GET    /v1/admin/routes
GET    /v1/admin/routes/{alias}
DELETE /v1/admin/routes/{alias}
```

- etcd 에 쓰면 모든 mci-api 인스턴스가 watch 로 즉시 동기화 (수십 ms).
- alias 명시했는데 미등록이면 mci-api 가 HTTP 404 `unknown_alias` 반환 (보수적 거부).
- raw `(exchange, routing_key)` 는 alias 없으면 그대로 통과 (passthrough).
- 자세한 동작은 `docs/routing.md` 참조.

### 2.2. 정책 룰 — 비상 차단

```bash
GET  /v1/admin/policy                            # 현재 스냅샷
POST /v1/admin/policy/kill-switch                # 전 시스템 차단 (HTTP 503)
POST /v1/admin/policy/maintenance                # 정비창 (HTTP 503)
POST /v1/admin/policy/blocked-symbols            # 차단 종목 (HTTP 403)
POST /v1/admin/policy/blocked-routing-keys       # 차단 routing-key (HTTP 403)
```

- mci-api 가 **alias resolve 이전에** 정책을 체크 (`internal/api/handlers/transaction.go:64-88`).
- alias 차단을 raw envelope 으로 우회당하지 않으려면 `blocked-routing-keys` 도 함께 설정.

### 2.3. broker 진단 endpoint

```bash
POST /v1/admin/cmd                # generic FC_ADMIN/SubGet* passthrough
GET  /v1/admin/status             # broker status
GET  /v1/admin/clients            # 접속 client 리스트
GET  /v1/admin/exchanges          # exchange 카탈로그
GET  /v1/admin/users              # 로그인 사용자 (argv0=usid 패턴)
GET  /v1/admin/whois?argv0=ECHOSVC&argv1=PING  # transaction 라우팅 추적 (xchg/rkey/qnam)
```

### 2.4. Audit / WebSocket 콘솔

- `GET /v1/admin/audit` — 최근 200 건의 admin 변경 이력 (in-memory ring).
- `GET /v1/admin/stream` — WebSocket 실시간 변경 푸시 (route/policy 토글).
- 모든 admin 변경은 자동으로 `logger.Info(ADMIN_ACTION)` + AuditRing + `Hub.Broadcast`.

### 2.5. svc-io / Tx 테스터 (검증용)

- `GET /v1/admin/svc-io` — 부팅 시 `-svc-inc-dir` 가 일괄 파싱한 svc 목록.
- `GET /v1/admin/svc-io/headers`, `GET /v1/admin/svc-io/{code}` — 단건 조회.
- `POST /v1/admin/svc-io/{code}/test-wire` — wire frame 테스트.
- `GET/PUT /v1/admin/svc-io/{code}/source` — svc 소스 열람/저장 (개발 편의).
- `POST /v1/admin/tx-test` — `-upstream-api` 로 reverse proxy (**dev 전용**).
- `POST /v1/admin/push-test` — broker 에 unsolicited publish 트리거.

---

## 3. 가동 순서 (운영 부트스트랩)

```
[1] 인프라
    □ mymqd 가동 (broker 포트 11217 응답 확인 — make ckey-echo)
    □ etcd cluster 가동 (ETCDCTL_API=3 endpoint health)
    □ Redis 가동 (운영 시)
    □ TLS 인증서 배포 (broker / DMZ↔Internal / 외부 HTTPS / mTLS CA)

[2] mci-admin 부팅 — control plane 먼저
    □ -etcd, -etcd-prefix, -etcd-policy-key, -allow-cidrs, -tls-* 설정
    □ /v1/admin/routes 로 운영 alias 일괄 등록
    □ /v1/admin/policy 초기 상태 확인 (kill-switch=false 등)

[3] Internal 서비스 부팅 — mci-admin 과 동일한 etcd 좌표
    □ mci-api (-etcd, -etcd-prefix, -etcd-policy-key 가 mci-admin 과 일치)
    □ mci-push (-grpc 노출, -queue 는 빈 값)
    □ mci-price (-grpc 노출, -queue=mci_price, -exchange=PRICE)
    □ quote-forwarder (UDP listener)

[4] DMZ 서비스 부팅 — Internal 향 mTLS 설정
    □ mci-edge-api (-upstream, -upstream-tls-*, -allow-cidrs)
    □ mci-edge-push (-upstream= mci-push:50052, -grpc-tls-*)
    □ mci-edge-price (-upstream= mci-price:50051, -grpc-tls-*)

[5] 검증
    □ make ckey-echo (broker 응답)
    □ POST /v1/admin/cmd → status (broker 정상)
    □ POST /v1/tx alias=WECHO_PING (transaction round-trip)
    □ ws://.../push 접속 후 push-test 발사 (push 라인)
    □ ws://.../price 접속 (시세 라인)
    □ build/bin/load-gen --rate 100 --duration 10s (delivery 100% 확인 — 시세 파이프라인 안전성 검증)
```

**Dev 환경 단축** — `wtgctl` 한 줄로 전체 stack:

```bash
WTG_PRICE=1 wtgctl start         # broker + mci-api/push/admin + forwarder + price + chart
WTG_PRICE=1 WTG_EDGE=1 wtgctl start   # + edge-api/push/price/chart 도

wtgctl status                    # 전부 한 줄 dump
wtgctl burst start walk          # 시세 시뮬레이션 (단일 시장 4 통화쌍)
wtgctl burst start multi         # 다중시장 — best 호가 산정 검증용 (4 feed 모두 USDKRW)
wtgctl logs price                # tail -f /tmp/mci-price.log
wtgctl burst stop && wtgctl stop --all
```

**부하 진단** — `scripts/load-test.sh`:

```bash
./scripts/load-test.sh low       # 640 tick/s baseline
./scripts/load-test.sh mid       # 6.4k tick/s typical
./scripts/load-test.sh high      # 64k tick/s extreme

# CSV 자동 저장: logs/load-<scenario>-<ts>.csv
# 동시에 mci-price /v1/price-stats, /v1/best-stats 폴링해 delivery % / drop / sub_drops 측정
```

---

## 4. mci-admin ↔ mci-api 좌표 일치의 중요성

가장 흔한 운영 사고:

| 증상 | 원인 |
|---|---|
| admin 에서 룰 추가했는데 api 가 404 `unknown_alias` | `-etcd-prefix` 가 양쪽 다름 |
| kill-switch 켰는데 트래픽이 통과 | `-etcd-policy-key` 가 양쪽 다름, 또는 mci-api 가 etcd 미연결 (in-memory 모드) |
| admin 만 etcd 연결 / api 들은 in-memory | api 가 부팅 직후 룰 캐시 비어있음 → 모든 alias 404 |

→ **mci-admin 과 모든 mci-api 인스턴스는 `-etcd`, `-etcd-prefix`, `-etcd-policy-key`
세 값이 정확히 같아야 한다.** `etc/wtg-*.env` 같은 공용 env 파일 한 벌 만들어서
systemd unit 에서 `EnvironmentFile=` 으로 읽는 방식 권장.

---

## 5. DevMode 단축 경로 (운영과 비교)

| 항목 | 운영 | DevMode |
|---|---|---|
| broker | 필수 | `--no-broker` 로 우회 (admin UI 만) |
| 인증 | JWT + 세션 | `X-WTG-User` 헤더만 |
| 라우팅 룰 | etcd | `-dev-routes-file`(JSON) → in-memory 시드 |
| 정책 동기화 | etcd watch | `-dev-policy-url=http://127.0.0.1:9090/v1/admin/policy` poll |
| Redis | 운영 store | `pkg/auth/memstore.go` |
| TLS | 모두 mTLS | 모두 plain TCP |

```bash
# 5분 안에 dev stack 띄우기
./build/bin/mci-admin --dev --no-broker --listen :9090 \
  -dev-routes-file ~/mymq/etc/wtg-routes.json
./build/bin/mci-api --dev --listen :8080 \
  -dev-routes-file ~/mymq/etc/wtg-routes.json \
  -dev-policy-url http://127.0.0.1:9090/v1/admin/policy
```

> **`-dev-routes-file` 은 mci-admin / mci-api 양쪽이 같은 파일을 보게 해라.**
> 안 그러면 dev stack 의 alias 가 두 프로세스에서 갈라진다. `-dev-routes-policy`
> 도 양쪽을 일치 (`additive` 또는 `sync`).

---

## 6. 자주 빠뜨리는 세팅 (사고 방지)

- ❌ `mci-push -queue` 에 값 넣음 → broker 가 `_SERVER_` 로 등록해서 unsolicited 못 받음 → **빈 값 유지**.
- ❌ mci-api 의 `-trust-edge` 를 외부 노출 listener 에서 활성 → `X-WTG-SID` 위조 가능. **mci-edge-api 뒤 Internal 망에서만**.
- ❌ mci-admin `-allow-cidrs` 비움 → 외부에서 admin 접근 가능. **반드시 사내 CIDR 명시**.
- ❌ mci-admin `-upstream-api` 운영 환경에서 활성 → `/v1/admin/tx-test` 가 운영 매매 트래픽 발사. **dev 전용**.
- ❌ DevMode 플래그(`-dev`) 가 운영 systemd unit 에 잔존 → JWT 우회 채로 노출.
- ❌ `make ckey-echo` 미실행 후 가동 → broker 가 ckey echo back 안 하면 동시 RPC 가 모두 섞인다 (Phase 1 GO/NO-GO).
- ❌ mci-edge-* 의 `-allow-cidrs` 비움 → 인터넷 전체에 노출. **회사·파트너 출구 IP 만 열기**.
- ❌ 다중 mci-api 인스턴스가 같은 `-instance` → broker 가 ApplName 충돌로 거부. **인스턴스마다 0,1,2,... 다르게**.

---

## 부록 A. Control Panel 관리 항목 전체 카탈로그

§2 가 endpoint 사용법 위주라면, 본 부록은 **관리 단위 / 데이터 모델 / 필드별
의미 / 유효값** 을 한 곳에 모은 레퍼런스. 운영자 / UI 개발자가 한 곳만 보면
admin 이 다루는 모든 항목을 파악할 수 있게 정리.

### A.0. 한눈에 — 관리 대상 카테고리

| 카테고리 | 변경 가능? | 저장소 | watch 전파 | 영향 범위 |
|---|---|---|---|---|
| **라우팅 룰** (alias→exchange/rkey) | ✅ CRUD | etcd `wtg/routes/*` | mci-api 인스턴스 모두 | 클라이언트 alias 동작 |
| **운영 정책** (kill/maintenance/blocked) | ✅ 토글·교체 | etcd `wtg/policy` | mci-api 인스턴스 모두 | 모든 transaction 통과 여부 |
| **broker 명령** (status/clients/whois) | ❌ 조회만 | broker (mymqd) | — | 진단/모니터링 |
| **svc-io 카탈로그** | ⚠ 부팅 시 일괄 로드 + 소스 편집 | 디스크 (`-svc-inc-dir`) | — | 매매 svc 헤더/body 직렬화 |
| **Audit ring** | ❌ 자동 기록 | in-memory (200건) | ws stream | 운영 추적 |
| **WebSocket Hub** | ❌ 자동 push | in-memory | ws subscriber | 다른 admin 화면 동기화 |
| **Tx/Push 테스터** | ✅ 트리거 (dev) | — | — | round-trip 검증 |

---

### A.1. 라우팅 룰 (Routes) — `wtg/routes/*`

**관리 단위**: alias 1개 = `Rule` 1개

#### A.1.1. `Rule` 필드 (`pkg/routing/registry.go:Rule`)

| 필드 | 타입 | 제약 | 의미 |
|---|---|---|---|
| `alias` | string | ≤ 64B, 공백·`/` 금지 | 클라이언트가 보는 짧은 이름 (예: `WECHO_PING`, `ORDER_NEW`) |
| `exchange` | string | ≤ `mymq.LXchg` (=8B), 옵셔널 | broker 의 exchange 이름 (DIRECT 시 필수, FANOUT 시 무시) |
| `routing_key` | string | ≤ `mymq.LRkey` (=8B), **필수** | broker 의 routing key — 매매 transaction 코드 |
| `active` | bool | — | false 면 즉시 비활성 (룰은 보존). 클라이언트는 404 `unknown_alias` |
| `comment` | string | 옵셔널 | 운영자 메모 ("신규주문", "테스트용 echo") |
| `updated_at` | time | 자동 | 마지막 변경 시각 (서버가 채움) |
| `updated_by` | string | 자동 | 변경한 admin 의 `Principal.Usid` (감사용) |

#### A.1.2. 입력 예시

```json
PUT /v1/admin/routes/ORDER_NEW
{
  "exchange":   "ORDER",
  "routing_key":"NEW",
  "active":     true,
  "comment":    "신규 주문 transaction"
}
```

> 운영 카탈로그는 `docs/conventions.md` 의 ApplName / Channel / Exchange / RKey
> 표를 참조해서 alias 를 정한다.

---

### A.2. 운영 정책 (Policy) — `wtg/policy`

**관리 단위**: 시스템 전역 단일 `State` (단일 etcd key)

#### A.2.1. `State` 필드 (`pkg/policy/policy.go:State`)

| 필드 | 타입 | 의미 | 거부 응답 |
|---|---|---|---|
| `kill_switch` | bool | true 면 매매 transaction 차단 | HTTP 503 `kill_switch` |
| `kill_switch_channels` | `[]string` | 비면 전체 차단 / 채워지면 그 채널만. 예: `["WEB","MOB","HTS"]` 로 고객만 차단, 직원(`ADM`/`EMP`) 거래는 유지 | HTTP 503 |
| `maintenance.start` | time | 정비창 시작 (UTC). zero 면 비활성 | HTTP 503 `maintenance` |
| `maintenance.end` | time | 정비창 끝 (UTC). zero 면 비활성. `start < end` 검증 | HTTP 503 |
| `maintenance.message` | string | 사용자 안내 ("심야 정비 중") | 응답 본문에 포함 |
| `blocked_symbols` | `[]string` | 거래 차단 심볼 (대문자 비교). envelope.data 의 `symbol` 추출 | HTTP 403 `blocked_symbol` |
| `blocked_routing_keys` | `[]string` | 차단 transaction code (raw envelope 우회 방어) | HTTP 403 `blocked_routing_key` |
| `updated_at` / `updated_by` | time / string | 자동 audit |

#### A.2.2. 입력 예시

```json
// 고객만 비상 차단, 직원 거래는 유지
POST /v1/admin/policy/kill-switch
{ "active": true, "channels": ["WEB","MOB","HTS"] }

// 새벽 2~4시 정비창 (UTC)
POST /v1/admin/policy/maintenance
{
  "start":   "2026-05-10T17:00:00Z",
  "end":     "2026-05-10T19:00:00Z",
  "message": "정기 시스템 점검"
}

// 차단 심볼 (전체 교체 — 단건 add/remove 아님)
POST /v1/admin/policy/blocked-symbols
{ "items": ["USDKRW", "EURJPY"] }

// raw envelope 우회 방어용 routing-key 차단
POST /v1/admin/policy/blocked-routing-keys
{ "items": ["NEW", "AMEND"] }
```

#### A.2.3. 검사 순서 (`internal/api/handlers/transaction.go`)

```
[1] kill_switch        → 503
[2] maintenance        → 503
[3] blocked_routing_keys (alias→rkey 또는 raw rkey) → 403
[4] blocked_symbols    (envelope.data 에서 추출)    → 403
[5] alias resolve (Routing)
[6] broker call
```

---

### A.3. broker 진단 (조회 전용)

`mci-admin` → broker (mymqd) 의 `FC_ADMIN/SubGet*` 명령 passthrough.

| Method | Path | broker 명령 | 반환 |
|---|---|---|---|
| POST | `/v1/admin/cmd` | generic `FC_ADMIN/SubGet*` | `AdminCmdResponse{errn, errm, data}` |
| GET | `/v1/admin/status` | broker status | broker 운영 메트릭 |
| GET | `/v1/admin/clients` | 접속 client | ApplName/Instance/Channel 별 접속 |
| GET | `/v1/admin/exchanges` | exchange 카탈로그 | broker 가 인지하는 exchange 목록 |
| GET | `/v1/admin/users` | 로그인 사용자 | broker 가 추적하는 LogonID |
| GET | `/v1/admin/whois?argv0=ECHOSVC&argv1=PING&argv2=ORDER_Q` | transaction 라우팅 추적 | 특정 exchange/rkey/queue 조합이 어디(어느 ApplName/IP)로 등록되어 있나 |

> **이건 변경 불가** — 변경하려면 broker 측 cfg (`mymqd.cfg`) 를 직접 수정해야 한다.

---

### A.4. svc-io 카탈로그 (매매 svc 헤더 파싱)

**관리 단위**: 부팅 시 `-svc-inc-dir` 로 일괄 파싱. UI 에서 단건 조회 + (개발 시) 소스 편집.

#### A.4.1. 데이터 (`pkg/svcio/registry.go`)

| 객체 | 필드 |
|---|---|
| `SvcSummary` | code (e.g. `WECHO_PING`), filename, brief description, header type, body fields |
| `Field` | name, c-type, length, offset, comment |
| `HeaderEntry` | comhdr (256B) / broadcast_h / 등 named typedef |

#### A.4.2. 운영 endpoint

| Method | Path | 목적 |
|---|---|---|
| GET | `/v1/admin/svc-io` | 전체 svc 목록 + summary |
| GET | `/v1/admin/svc-io/headers` | 공통 헤더 정의 카탈로그 (comhdr.h 파싱 결과) |
| GET | `/v1/admin/svc-io/{code}` | 단건 — IO 필드 spec |
| POST | `/v1/admin/svc-io/{code}/test-wire` | wire frame 직렬화/파싱 검증 |
| GET | `/v1/admin/svc-io/{code}/source` | svc 소스 코드 열람 |
| PUT | `/v1/admin/svc-io/{code}/source` | svc 소스 코드 저장 (개발 편의) |

#### A.4.3. 부팅 시 입력 (운영자가 정하는 값)

| flag | 의미 |
|---|---|
| `-svc-inc-dir` | 매매 svc 헤더 디렉터리 (콤마 구분 다중 path). 예: `~/mywork/win/src/inc/trn` |
| `-svc-common-header` | 공통 transaction 헤더 (예: `~/mywork/win/src/inc/com/comhdr.h`) — 운영 svc 의 wire 가 `[COMHDR(256B)][TX_BODY]` 구조이므로 필수 |

> `-svc-inc-dir` 비우면 svc-io 비활성 — 모든 svc 가 raw body 로만 동작 (직렬화/파싱 없음).

---

### A.5. Audit Ring — 운영 추적 (자동 기록)

**관리 단위**: in-memory ring buffer 200건. 운영자가 직접 쓰지 않음 (자동).

#### A.5.1. `AuditEntry` (`internal/admin/audit_ring.go`)

| 필드 | 의미 |
|---|---|
| `action` | `PUT_ROUTE` / `DELETE_ROUTE` / `SET_ROUTE_ACTIVE` / `POLICY_KILL_SWITCH` / `POLICY_MAINTENANCE` / `POLICY_BLOCKED_SYMBOLS` / `POLICY_BLOCKED_RKEYS` |
| `usid` | 변경한 admin (Principal.Usid) |
| `rid` | request id (트레이싱) |
| `at` | 시각 (UTC) |
| `attrs` | 액션별 상세 — `alias`, `active`, `count`, `start`/`end` 등 |

#### A.5.2. endpoint

| Method | Path | 목적 |
|---|---|---|
| GET | `/v1/admin/audit?limit=N` | 최근 N건 조회 (default 200) |
| GET | `/v1/admin/stream` | WebSocket 실시간 변경 푸시 (route/policy 토글 모두) |

> **운영에서는 immutable 외부 sink** (7년 보관) **가 single source of truth**.
> ring 은 UI 즉시 표시 + 로컬 디버깅용 sliding window. 자세한 카테고리는 `auth.md §10`.

---

### A.6. Tx / Push 테스터 (검증용)

**관리 단위**: dev/검증 트리거. 운영 환경에서는 비활성 권장.

| Method | Path | 본문 | 목적 |
|---|---|---|---|
| POST | `/v1/admin/tx-test` | mci-api `/v1/tx` envelope 그대로 | reverse proxy → mci-api → broker round-trip 검증 |
| POST | `/v1/admin/push-test` | `{logon_id?, exchange, routing_key, payload}` | broker 에 unsolicited publish 트리거 → mci-push 가 받아 fan-out |

**전제**:

- `tx-test`: `-upstream-api` 가 채워져 있어야 함 (`http://127.0.0.1:8080`). 비면 503.
- `push-test`: broker 연결 필요. `--no-broker` 시 503.

---

### A.7. 부팅 시 한 번 결정되는 값 (재시작 필요)

운영자가 control panel 에서 바꿀 수 없는, **부팅 flag/env 로만 결정되는** 항목.

| 항목 | flag | 변경 시 |
|---|---|---|
| listen 주소 | `-listen` | mci-admin 재시작 |
| broker 좌표 | `-broker-host`, `-broker-port` | 재시작 |
| etcd 좌표 | `-etcd`, `-etcd-prefix`, `-etcd-policy-key` | 재시작 |
| 사내 CIDR 화이트리스트 | `-allow-cidrs` | 재시작 |
| 운영자 mTLS CA | `-tls-client-ca` | 재시작 |
| svc-io 디렉터리 | `-svc-inc-dir`, `-svc-common-header` | 재시작 (또는 source 편집 후 자동 reparse) |
| DevMode 토글 | `-dev`, `-no-broker` | 재시작 |

→ 이 값들은 systemd unit / EnvironmentFile 로 고정. control panel 의 동적 항목과 분리.

---

### A.8. 저장 위치 / 영속성 매트릭스

| 데이터 | 저장소 | 재시작 후 보존 | 다중 인스턴스 공유 |
|---|---|---|---|
| 라우팅 룰 (운영) | etcd `wtg/routes/*` | ✅ | ✅ watch |
| 라우팅 룰 (dev) | in-memory + `-dev-routes-file` JSON | 파일 있으면 ✅ | ❌ |
| 정책 (운영) | etcd `wtg/policy` | ✅ | ✅ watch |
| 정책 (dev) | in-memory + (옵션) `-dev-policy-url` poll | ❌ | poll 로 일관 |
| Audit ring | in-memory 200건 | ❌ | 인스턴스별 분리 (외부 sink 가 진실) |
| svc-io 카탈로그 | 부팅 시 디스크 → in-memory | 디스크 ✅ / 메모리 ❌ | 인스턴스별 동일 디스크 mount |
| ws Hub 구독자 | in-memory | ❌ | 인스턴스별 |

---

### A.9. 일상 운영 vs 비상 대응 매트릭스

| 상황 | 작업 | endpoint |
|---|---|---|
| 신규 transaction 라이브 | alias 등록 | `PUT /v1/admin/routes/{alias}` |
| 특정 alias 잠시 차단 | active=false 토글 | `POST /v1/admin/routes/{alias}/active` |
| broker exchange 이전 | exchange/rkey 만 수정 | `PUT /v1/admin/routes/{alias}` (alias 그대로) |
| **고객만 비상 차단** | scoped kill-switch | `POST /v1/admin/policy/kill-switch` `{active:true, channels:[...]}` |
| 전 시스템 비상 정지 | 전체 kill-switch | `POST /v1/admin/policy/kill-switch` `{active:true}` |
| 정기 정비창 예약 | maintenance 설정 | `POST /v1/admin/policy/maintenance` |
| 특정 종목 차단 | symbols 교체 | `POST /v1/admin/policy/blocked-symbols` |
| raw 우회 방어 | rkey 차단 추가 | `POST /v1/admin/policy/blocked-routing-keys` |
| 누가 무엇을 바꿨나 | audit 조회 | `GET /v1/admin/audit?limit=200` |
| 다른 admin 화면 동기화 | ws 구독 | `GET /v1/admin/stream` |
| broker 정상성 확인 | broker status | `GET /v1/admin/status` |
| 로그인 사용자 검색 | usid glob | `GET /v1/admin/users?argv0=trader*` |
| transaction 라우팅 추적 | whois (xchg/rkey/qnam) | `GET /v1/admin/whois?argv0=ECHOSVC&argv1=PING` |

---

### A.10. 권한 / 보안 (auth.md 와 일치)

- 모든 `/v1/admin/*` 는 JWT + `ChannelAdmin` 통과 필수.
- 사내망 `-allow-cidrs` 화이트리스트 통과 필수.
- 운영자 mTLS (`-tls-client-ca`) 통과 필수.
- `updated_by` 필드는 항상 `Principal.Usid` 에서 자동 채워짐 — 익명 변경 불가.
- 비즈니스 권한 (거래 한도/통화쌍/거래시간) 은 **여기서 안 다룬다** — 매매 엔진 책임.

---

### A.11. 빠른 운영 체크리스트

```
□ alias 카탈로그 정합성     — docs/conventions.md ↔ /v1/admin/routes 비교
□ 정책 snapshot 백업        — GET /v1/admin/policy 정기 dump
□ Audit 외부 sink 가동      — 7년 보관 정책 충족
□ ws stream 사용 중인지     — admin 다중 운영 시 race 방지
□ -upstream-api 가 운영 비활성 — 운영 매매 트래픽 우회 차단
□ -dev 플래그 운영 unit 잔존 X — JWT 우회 방지
□ etcd 좌표 일치 (admin ↔ 모든 api) — 본 문서 §4
```

---

## 7. 관련 문서

- `docs/mci-architecture.md` — 컴포넌트 흐름도 + 내부 도구 라우팅 권고
- `docs/routing.md` — alias→exchange/rkey 변환, Registry, SeedPolicy 상세
- `docs/auth.md` — JWT/세션, 매매 엔진 권한 위임, ADMIN_ACTION 카테고리
- `docs/conventions.md` — ApplName / Channel / Exchange / RoutingKey / Queue 카탈로그
- `docs/broker-tls.md` — broker TLS 합의안
- `docs/mci-test-runbook.md` — `mci-test` CLI GO/NO-GO 검증 절차
- `docs/testing.md` — 단위 → 통합 → e2e 단계별 시나리오
