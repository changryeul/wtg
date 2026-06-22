# 배포 시나리오 — 다중 사이트 (Active-Active + GSLB)

> 지리적으로 분리된 두 데이터센터 (Seoul / Busan) 에 WTG 를 배포하는 구성 예제.
> 본 문서는 단일 사이트 구성 (`deployment-scenario-ha-channel.md`) 을 전제로 — **사이트 1개 안의 HA / 채널 / 라우팅 / 마진 / 검증** 은 그 문서 그대로 적용된다.
> 본 문서가 다루는 것은 **사이트가 둘 이상 됐을 때 새로 결정해야 할 4가지** 와 그 결과 :
> 1. **사이트 간 데이터 동기화** — etcd / Redis / TimescaleDB
> 2. **사용자 트래픽 라우팅** — GSLB / sticky 정책
> 3. **broker 클러스터의 범위** — 사이트별 독립 vs 양 사이트 걸침
> 4. **사이트 fail 시 동작** — RPO / RTO / split-brain

---

## 1. 요구 사항

본 시나리오의 출발점:

1. **두 사이트** — Seoul (주 데이터센터) + Busan (보조 / DR)
2. **Active-Active** — 둘 다 동시에 사용자 트래픽 받음. 한쪽이 idle DR 가 아님.
3. **사용자는 가까운 사이트로** — Seoul 사용자는 Seoul, Busan 사용자는 Busan. GSLB (Global Server Load Balancer) 가 결정.
4. **한 사이트 전체 다운** 시에도 서비스 지속 — 30초 이내 GSLB failover.
5. **사이트 간 데이터 일관성** — 마진 정책 / 라우팅 룰 / 세션 / 매매 감사 모두 양 사이트가 같은 view.

**RPO / RTO 목표** (운영 합의값) :
- **RPO** ≤ 5초 (매매 손실은 마지막 5초 안의 것만)
- **RTO** ≤ 30초 (사용자가 30초 안에 다른 사이트로 자동 이전)

> 더 엄격한 RPO/RTO 가 필요하면 cross-site synchronous replication 도입 — 본 시나리오에선 asynchronous + 사이트 내 synchronous 의 절충.

---

## 2. 토폴로지

### 2.1 사이트 별 컴포넌트

각 사이트는 단일 사이트 구성 (`deployment-scenario-ha-channel.md` §2) 의 풀스택을 그대로 가진다. 새로 추가되는 건 사이트 간 동기화 + GSLB.

```
                       사용자 (전세계)
                            │
                            ▼
                     ┌──────────────┐
                     │    GSLB      │  DNS-based, latency/health-based
                     │ (Route53/    │  routing
                     │  Cloudflare) │
                     └──────┬───────┘
                            │
              ┌─────────────┴─────────────┐
              ▼                           ▼
   ┌────────────────────────┐   ┌────────────────────────┐
   │   Seoul Site (주)       │   │   Busan Site (보조)     │
   │                        │   │                        │
   │   DMZ : dmz-sl-1, 2    │   │   DMZ : dmz-bs-1, 2    │
   │   Internal : int-sl-*  │   │   Internal : int-bs-*  │
   │   AP : ap-sl-1, ap-sl-2│   │   AP : ap-bs-1, ap-bs-2│
   │   fwd-sl-1             │   │   fwd-bs-1             │
   │   etcd-sl-{1,2,3}      │   │   etcd-bs-{1,2}        │ ← 양 사이트가
   │   redis-sl-{1,2,3}     │   │   redis-bs-{1,2}       │   하나의 cluster
   │   db-sl-1 (primary)    │   │   db-bs-1 (replica)    │
   │   obs-sl-1             │   │   obs-bs-1             │
   └────────────┬───────────┘   └────────────┬───────────┘
                │                              │
                └──────────────┬───────────────┘
                               │
                  사이트 간 전용선 (≤5ms RTT)
                    - etcd Raft replication
                    - Redis Sentinel cross-site
                    - TimescaleDB streaming replication
                    - Prometheus federation
```

### 2.2 노드 별 역할 (양 사이트 합계)

| 노드 | 개수 / 사이트 | 양 사이트 합 |
|---|---|---|
| AP active+standby | 2 | 4 |
| Internal 서비스 | 2 | 4 |
| DMZ 서비스 | 2 | 4 |
| quote-forwarder | 1 | 2 |
| etcd | Seoul 3 + Busan 2 | **5 node 단일 cluster** |
| Redis Sentinel | Seoul 3 + Busan 2 | **5 sentinel 단일 cluster** |
| TimescaleDB | Seoul primary + Busan replica | 2 (master + standby) |
| 관측 stack | 1 | 2 (서로 federate) |
| GSLB | (외부 SaaS) | 1 |

> **etcd 5 node** 가 양 사이트에 3+2 로 분산되면 quorum=3 — Seoul 단독으로도 quorum 유지, Busan 단독은 안 됨 (intentional). Busan 이 주 quorum 사이트가 되려면 3+2 → 2+3 으로 재배치.

### 2.3 사이트 내 구조는 단일 사이트 문서 그대로

각 사이트 안의 :
- 채널 분리 (WEB / CS) — §3 (단일 사이트 문서)
- 라우팅 (alias → exchange/rkey) — §4 (단일)
- 마진 정책 — §5 (단일)
- 인증 / cookie_t — §8 (단일)
- failover (broker reconnect supervisor) — §10 (단일)

→ **운영자가 한 사이트의 운영을 알면, 양 사이트의 운영은 1.x 배 정도의 부담** (사이트 간 동기화만 추가).

---

## 3. 사이트 간 데이터 동기화

본 시나리오의 4가지 store 각각이 어떻게 양 사이트를 가로지르는가.

### 3.1 etcd — 단일 5-node Raft 클러스터

가장 단순한 모델 — **양 사이트가 etcd 하나를 공유**.

```
etcd-sl-1 ◄────────► etcd-sl-2 ◄────────► etcd-sl-3
    ▲                    ▲                    ▲
    │                    │                    │
    └────── Raft peer connection (사이트 내) ──┘
                         │
                    ┌────┴────┐
                    │ 전용선  │
                    └────┬────┘
                         │
              ┌──────────┴──────────┐
              ▼                     ▼
          etcd-bs-1 ◄─────────► etcd-bs-2
```

**특징** :
- 양 사이트 모두 `mci-admin` / `mci-api` / `mci-price` 가 자기 사이트 etcd 노드에 연결 (`--etcd https://etcd-sl-1:2379,etcd-sl-2:2379,...`).
- 모든 write 가 Raft leader 를 거침 — 양 사이트 모든 client 가 같은 view.
- 사이트 간 RTT 5ms → write latency 도 5ms 정도 추가 (acceptable for 정책 변경).

**장점** : 정책 (라우팅 룰 / PricingTable / Profile / 카탈로그) 가 **양 사이트 자동 일치**. 한쪽에서 admin UI 로 PricingTable 갱신하면 양 사이트 mci-price 가 모두 watch 로 즉시 hot reload.

**리스크** : Seoul 사이트 전체 다운 → etcd Seoul 3 node 죽음 → quorum loss (Busan 2 node 만 살음) → etcd cluster 가 read-only 로 떨어짐. **Busan 만으로는 정책 변경 불가**. 대응 : §6.4 의 split-brain 대응 절차.

### 3.2 Redis — Sentinel cross-site

세션 + cookie_t + quoteid Registry 의 store.

```
            ┌─────────────┐
            │   master    │  redis-sl-1 (Seoul, primary)
            └──────┬──────┘
                   │ async replication
        ┌──────────┼──────────┐
        ▼          ▼          ▼
    redis-sl-2  redis-sl-3  redis-bs-1
    redis-bs-2
       (read replicas)

Sentinel:
    sentinel-sl-1, sl-2, sl-3, bs-1, bs-2  (5 sentinel, quorum=3)
```

**특징** :
- master 가 Seoul. Busan replica 가 async 로 따라감 (≤ 100ms 지연).
- 모든 write 는 Seoul master → Busan 도 같은 사이트 redis 에 read 만 (위치상 가까움) — `redis.UniversalClient` 의 `ReadOnly=true` slave preferred 옵션.
- 양 사이트 어디서든 같은 cookie_t / 세션 lookup 가능.

**Active-Active 세션 관리** :
- 사용자 A 가 Seoul 에서 로그인 → Seoul master 에 세션 write.
- 같은 사용자 A 가 Busan 으로 trip → Busan replica 에서 세션 lookup → 100ms 안에 보일 가능성 높음. 만 못 보면 401 → 다시 로그인.
- 이걸 막으려면 **사용자 sticky** (§4.2) 또는 **synchronous replication** (비용 ↑).

### 3.3 TimescaleDB — streaming replication

차트 봉 영속 store.

```
                                  WAL streaming
   db-sl-1 (primary) ──────────────────────────────────► db-bs-1 (standby)
       │                                                       │
       │ INSERT (mci-price.Archiver)                           │
       ▼                                                       ▼
   quote_bars                                            quote_bars (read-only)
                                                               │
                                                               │ SELECT (mci-chart-bs)
                                                               ▼
                                                          Busan 사용자 차트
```

**특징** :
- Seoul primary 에 모든 write. Busan standby 는 hot standby (read-only).
- Busan mci-chart 는 standby 에 SELECT — read-after-write 일관성 약함 (~100ms 지연 OK, 차트는 거의 read-mostly).
- Seoul primary 죽으면 Busan standby 가 promote — Patroni 또는 pg_auto_failover 권장.

### 3.4 관측 stack — Prometheus federation

```
   Prometheus-sl (Seoul scrape)         Prometheus-bs (Busan scrape)
       │                                      │
       └──── federate (서로 핵심 메트릭 복제) ──┘
       │                                      │
       ▼                                      ▼
   Grafana-sl  ◄───────────────────────► Grafana-bs
                 (datasource 양쪽)
```

**특징** :
- 사이트 별 Prometheus 가 자기 사이트만 scrape — 사이트 간 5ms RTT 라도 scrape 부담 회피.
- 핵심 메트릭 (예: `wtg_http_requests_total`) 만 federate 로 cross-site — Grafana 가 글로벌 view 만들 때 사용.
- Alert 은 각 사이트 Prometheus 가 자기 사이트 alert. Cross-site alert (예: "양 사이트 동시 다운") 은 federation 위에서 추가 룰.

### 3.5 동기화 종합표

| store | 일관성 | 양 사이트 view | RPO |
|---|---|---|---|
| etcd | **strong (Raft)** | 동일 | 0 (synchronous) |
| Redis | eventual (async) | ~100ms 차이 가능 | ~100ms |
| TimescaleDB | eventual (streaming) | ~100ms 차이 가능 | ~100ms |
| Prometheus | per-site + federate | per-site view + 글로벌 panel | 메트릭 별 |

→ **etcd 만 strong consistency, 나머지는 eventual**. 본 시나리오 설계의 핵심 트레이드오프.

---

## 4. 사용자 트래픽 라우팅 — GSLB + sticky

### 4.1 GSLB 정책

**1차 결정** : 사용자가 어느 사이트로 갈지.

| 방식 | 본 시나리오 |
|---|---|
| **geographic** | Seoul → Seoul site, Busan → Busan site (IP geolocation) |
| **latency-based** | 낮은 latency 사이트 자동 선택 (Route53 latency routing) |
| **weighted** | Seoul 70% / Busan 30% (운영 정책상 분배) |
| **failover** | health check fail 시 다른 사이트로 자동 전환 (RTO ≤ 30s) |

본 시나리오 정책 : **latency-based primary + failover** — 평상시는 사용자 별로 가까운 사이트, 한 쪽 다운 시 자동 전환.

### 4.2 사용자 sticky — 같은 사용자는 같은 사이트에 묶기

**왜 필요** : Redis 의 eventual consistency 로 세션이 양 사이트 간 100ms 지연. 매 요청마다 다른 사이트 가면 401 위험.

**구현** :
- GSLB 의 client subnet sticky (Route53 의 client-IP based routing) — 같은 사용자는 같은 사이트로 N분 유지.
- 또는 첫 로그인 응답에 `Set-Cookie: site=seoul; Domain=*.wtg.example.com` — 이후 클라이언트가 자동으로 `seoul.wtg.example.com` 으로 직접.

**예외** :
- Seoul 사이트 다운 → 모든 sticky 무효 → Busan 으로 전환 → 사용자 재로그인 필요할 수도 (Busan Redis replica 가 stale 한 짧은 시간).

### 4.3 DMZ 진입 후 routing

```
사용자 ─── DNS GSLB ───► seoul.wtg.example.com
                            │  (사내 LB 의 VIP)
                            ▼
                       HAProxy (Seoul)
                            │
            ┌───────────────┼────────────────┐
            ▼               ▼                ▼
        dmz-sl-1        dmz-sl-2         (Busan 의 DMZ 로 cross-site
                                          forward 안 함 — sticky 유지)
```

→ Seoul 사이트 안에서는 단일 사이트 구성과 동일 routing. cross-site forward 는 GSLB 단에서만 결정.

### 4.4 라이브 ws (시세) 의 cross-site

사용자가 Seoul → Busan 으로 강제 이전된 경우 :
- mci-edge-price 의 ws 연결 끊김 → 클라이언트가 자동 재연결 → GSLB 가 Busan 으로 라우팅 → Busan mci-edge-price 의 새 ws.
- 호가창 0.5 ~ 2초 빈 화면 후 Busan 시세로 재개.
- 운영 신호 : 양 사이트 admin UI 의 `🔗 연결 (ws)` 페이지 count 가 단계적으로 이전.

---

## 5. broker 클러스터의 범위 — 가장 중요한 결정

다중 사이트의 가장 어려운 결정. **2가지 옵션** 이 있고 각각 trade-off 가 다르다.

### 5.1 옵션 A — 사이트별 독립 broker cluster (**본 시나리오 선택**)

```
   Seoul site                      Busan site
   ─────────                       ─────────
   ap-sl-1 ◄══cluster:11218══► ap-sl-2     (Seoul broker cluster)
                                    
   ap-bs-1 ◄══cluster:11218══► ap-bs-2     (Busan broker cluster)
   
   서로 broker 통신 안 함. 매매 AP / cookie_t 도 사이트 내 완결.
```

**특징** :
- Seoul 매매 AP 가 발급한 cookie_t 는 Seoul 안에서만 유효.
- 사용자가 Seoul → Busan 으로 이전 시 → cookie_t 재발급 (재로그인 비슷한 짧은 절차) 필요.
- 사이트 간 broker 통신 없음 — 네트워크 의존 0 → split-brain 위험 없음.

**장점** :
- 단순. 운영 사고 시 한 사이트만 보면 됨.
- 사이트 간 전용선이 끊겨도 각 사이트가 독립 가동.
- broker 부하가 사이트별로 격리 — 한쪽 사고가 다른쪽 영향 없음.

**단점** :
- cookie_t 가 사이트 간 공유 안 됨 → 사용자 cross-site 이전 시 재인증.
- 매매 AP 의 ledger 가 사이트별로 분리 — 분쟁 시 어느 사이트에서 발생했는지 추적 필요.

### 5.2 옵션 B — 양 사이트 걸친 broker cluster (참고)

```
   ap-sl-1 ◄══════ cluster:11218 ═══════► ap-bs-1
                       │
                  전용선 5ms RTT
                       │
   ap-sl-2 ◄══════ cluster:11218 ═══════► ap-bs-2
```

**특징** :
- broker cluster 가 4 node 로 sync. 매매 AP 가 양 사이트에 분산되어도 같은 cookie_t / ledger view.
- 사용자 cross-site 이전 시 cookie_t 그대로 유효.

**장점** :
- 사용자 입장에서 매끄러움.

**단점** :
- 사이트 간 전용선 끊김 → split-brain 위험 (양 사이트가 각자 active 라고 생각).
- broker cluster 의 cross-site sync latency → 매매 latency ↑.
- mymq broker 가 이 모드 지원하는지 별도 검증 필요 — `../broker-tls.md` 참조.

### 5.3 본 시나리오 선택 — A (사이트별 독립)

이유 :
- mymq broker 의 cross-site cluster 지원이 검증 안 됨.
- split-brain 위험 회피 우선.
- 사용자 cross-site 이전이 드문 케이스 (사이트 다운 시에만) — UX 약간 손해 감수.

**결과 — cookie_t / 매매 ledger 가 사이트별로 분리** :

| 항목 | 사이트 간 공유 | 사이트 별 |
|---|---|---|
| 사용자 인증 (JWT) | etcd 의 user 카탈로그 → 공유 | (JWT 자체는 stateless) |
| 세션 (Redis) | 글로벌 Redis → 공유 | — |
| cookie_t | **사이트 별** ← 매매 AP 가 발급 | ← 본 시나리오 결정 |
| 매매 ledger | — | **사이트 별** ← 매매 AP DB |
| quote_id Registry | (Redis 면 글로벌) 또는 (사이트 별) | 본 시나리오는 사이트 별 (§5.4) |

### 5.4 quote_id Registry 도 사이트 별로

mci-price 가 발급한 quote_id 를 다른 사이트 매매 AP 가 검증할 수 있어야? 본 시나리오는 안 함.

이유 : 사용자가 Seoul mci-price 로 호가 받음 → Seoul 매매 AP 가 검증. cross-site 거래는 정의되지 않음 (사이트 sticky 라 같은 사이트에서 처리 완결).

→ Redis Registry 를 사이트 별로 분리 :
- Seoul mci-price → `redis-sl:6379` (Sentinel master = Seoul)
- Busan mci-price → `redis-bs:6379` (Sentinel master = Busan, 별도 Sentinel)

**주의** : §3.2 의 세션용 Redis 와 quote_id Registry 용 Redis 는 **다른 instance** — 둘 다 Redis 이지만 instance 분리. 세션은 글로벌, quote_id 는 사이트 별. 운영 시 instance 명확히 분리.

---

## 6. 사이트 fail 시나리오

### 6.1 Seoul 사이트 전체 다운 (가장 큰 사고)

```
T=0          Seoul 데이터센터 전원 차단
T=5s         GSLB health check fail (3회 연속 실패 후)
T=10s        DNS TTL 만료 → 사용자가 Busan 으로 라우팅
T=15s        Busan dmz 가 사용자 요청 받기 시작
T=20s        세션 lookup :
                 사용자 A 의 jti 가 Busan Redis replica 에 도착했으면 OK
                 늦었으면 (replication lag) 401 → 재로그인
T=30s        대부분 사용자 정상 (재로그인 포함)

영향 :
  - Seoul mci-price 의 in-memory 5L customer quote 캐시 lost → Busan mci-price 가 새로 계산
  - Seoul quote_id Registry (사이트 별) lost → 진행 중인 매매 inflight 약간 손실
  - Seoul TimescaleDB primary lost → Busan standby 가 promote (Patroni 자동)
  - etcd quorum : Seoul 3 + Busan 2 → Busan 만 2 → quorum loss → read-only
       → 정책 변경 불가 (PricingTable 등)
       → 6.4 의 강제 quorum 복구 절차
```

**RPO** ≈ 5초 (마지막 5초 매매 손실)
**RTO** ≈ 30초 (대부분 사용자)

### 6.2 사이트 간 전용선 다운 (split-brain 가능성)

```
T=0          전용선 fiber cut
T=1s         양 사이트가 서로 통신 못 함
            
   Seoul :  etcd Seoul 3 node 만 보임 → quorum 유지 (정상)
            Redis master 는 Seoul 에 있음 → 정상 read/write
            DMZ / Internal 정상
            
   Busan :  etcd Busan 2 node 만 보임 → quorum loss → read-only
            Redis master 못 보임 → Sentinel 이 Busan replica 를 master 로 promote 시도
                                   하지만 Sentinel quorum (3) 못 채움 → promote 안 됨
            결과 : Busan 도 정상 가동 못 함

GSLB :   Busan health check fail → 모든 트래픽 Seoul 로
T=30s    Busan 사용자도 Seoul 로 우회 (latency ↑)
```

→ **split-brain 회피** : Busan 이 자동으로 자기를 disable. GSLB 가 fail-safe 로 동작.

본 시나리오의 etcd 와 Redis Sentinel quorum 이 3 + 2 = 5 (Seoul 다수) 라 split-brain 발생 시 Seoul 이 자동 우승. 다수 사이트가 Busan 이 되어야 할 운영 정책이라면 quorum 배치를 2 + 3 으로.

### 6.3 한 사이트 안의 ap1 만 다운

→ 단일 사이트 문서 §10.1 그대로 적용. ap2 가 promote → 그 사이트 내 정상 가동. 다른 사이트는 영향 없음.

### 6.4 etcd quorum loss 시 강제 복구

Seoul 사이트 죽었는데 정책 긴급 변경 필요 시 :

```bash
# Busan 의 etcd 2 node 를 강제로 new cluster 로
etcdctl member remove <seoul_member_ids>
# 또는 etcd snapshot restore --initial-cluster ...

# 이후 Seoul 복구 시 새 member 로 join
etcdctl member add etcd-sl-1 --peer-urls=...
```

→ 매우 위험. 운영 SOP 에 "Seoul 복구가 N 시간 안에 가능하면 기다림. 못 하면 강제 복구" 명확.

### 6.5 양 사이트 모두 정상인데 cross-site 거래가 잘못된 경우

발생 가능 : 사용자가 GSLB 의 짧은 라우팅 변경으로 Seoul 로그인 → Busan 매매 시도 → 사이트 별 cookie_t 라 매매 reject.

→ mci-api 가 401 응답하면서 "site mismatch" 에러 → 클라이언트가 재로그인.

운영 신호 : `wtg_http_requests_total{status="401",reason="site_mismatch"}` 카운터.

---

## 7. 부트스트랩 / 운영 SOP

### 7.1 first-time 부트스트랩 순서

```
1. 양 사이트 OS / 네트워크 / 전용선 / PKI 준비
2. etcd 5 node 단일 cluster 구성 (Seoul 3 + Busan 2)
       Seoul 먼저 부팅 → Busan join
3. Redis Sentinel 5 sentinel 구성 (세션용)
       Seoul master + replica + sentinel → Busan replica + sentinel
4. Redis (quote_id Registry 용) 사이트 별 별도 구성 :
       Seoul Sentinel (3 sentinel 사이트 내) + Busan Sentinel (별도)
5. TimescaleDB :
       Seoul primary 부팅 → quote_bars 스키마 → Busan standby streaming replication
6. AP servers 양 사이트 동시 부팅 :
       broker cluster 는 사이트 별 → ap-sl-1, ap-sl-2 cluster (Seoul), ap-bs-* (Busan)
7. Internal services :
       site-aware flag 로 부팅 (예: --site=seoul, --quoteid-redis=redis-sl-...)
8. DMZ services :
       사이트 내 mci-* upstream 만 사용 (cross-site upstream 안 함)
9. 관측 stack :
       site-local Prometheus + Grafana → federation 설정
10. GSLB :
       DNS 등록 + health check + sticky 정책
11. 검증 :
       Seoul 사용자 / Busan 사용자 각자 로그인 → 매매 → 시세 정상
       Seoul → Busan 강제 GSLB 전환 → 재로그인 후 정상
       Seoul site 모의 다운 → 30초 안에 Busan 으로 전환
```

### 7.2 운영자 SOP — 다중 사이트 한정

#### 7.2.1 정책 변경 (PricingTable)

1. **양 사이트에 동시 반영됨** (etcd 글로벌). 한 사이트만 변경할 수 없음.
2. 사이트 별 다른 정책 필요하면 PricingTable 안에 site 필드 추가 + Apply 로직 수정 (코드 변경 필요).

#### 7.2.2 한 사이트 정비창

1. **GSLB 에서 그 사이트 weight=0** → 새 트래픽 차단
2. 활성 사용자가 다른 사이트로 자연 이전될 때까지 대기 (15분 권장)
3. 정비 작업
4. 사이트 복구 후 weight 복원

#### 7.2.3 cross-site 사용자 이전 모니터링

- admin UI `🔗 연결 (ws)` 페이지 : 양 사이트의 connection count 추세
- `📈 매매 통계` : 사이트 별 매매 분포

---

## 8. 시나리오 의존 항목

본 문서가 의존하는 가정. 환경이 다르면 조정 :

| 항목 | 본 시나리오 가정 | 환경 다르면 |
|---|---|---|
| 사이트 수 | 2 (Seoul + Busan) | 3+ 면 etcd / Redis quorum 재계산 |
| Active-Active | 양 사이트 동시 active | Active-DR 면 트래픽 라우팅 단순화 |
| broker cluster | 사이트 별 독립 (옵션 A) | cross-site cluster (옵션 B) 가능 — broker side 검증 필요 |
| cookie_t | 사이트 별 (사용자 cross-site 시 재로그인) | 글로벌 — 별도 cross-site session 동기화 필요 |
| quote_id Registry | 사이트 별 Redis | 글로벌 (cross-site 매매 허용 시) |
| etcd quorum | 3 + 2 (Seoul 다수) | 2 + 3 또는 균등 — 3 사이트 권장 |
| TimescaleDB | Seoul primary + Busan replica | active-active 면 별도 솔루션 (Citus, BDR) |
| GSLB | DNS-based (Route53/Cloudflare) | L7 anycast 또는 자체 솔루션 |
| 사이트 간 RTT | ≤ 5ms (KR 내) | 글로벌 (수십 ms) 이면 cookie_t 사이트 별 강제 |

---

## 9. 단일 사이트 문서 (`deployment-scenario-ha-channel.md`) 대비 무엇이 다른가

| 영역 | 단일 사이트 | 다중 사이트 |
|---|---|---|
| §3 도메인 모델 | 그대로 | + site 필드 가능 (코드 변경 시) |
| §4 라우팅 | 그대로 | GSLB 가 1단 추가 |
| §4.5 시세 파이프라인 | 그대로 | 사이트 별 독립 — feed 가 양 사이트로 동시 들어오거나 cross-site replicate |
| §4.6 mci-push | 그대로 | 사이트 별 mci-push — cross-site 사용자에게는 다른 사이트가 보냄 |
| §4.7 quote_id | 그대로 | Registry 사이트 별 분리 |
| §5 마진 | 그대로 | etcd 글로벌이라 양 사이트 자동 일치 |
| §6 서비스 flag | + `--site=<name>` , `--upstream` 이 site-local | |
| §8 cookie_t | 그대로 | 사이트 별 발급 — cross-site 이전 시 재로그인 |
| §10 failover | 그대로 (사이트 내) | + 사이트 간 GSLB failover |
| §14 매매 AP | 그대로 | 매매 AP 도 사이트 별 — broker cluster 와 동거 |

---

## 10. 검증 체크리스트 (배포 직후)

단일 사이트의 §13 모두 + 다음 추가 :

- [ ] etcd 5 node 단일 cluster `endpoint health` 모두 OK
- [ ] Redis Sentinel cross-site replication 동작 (Seoul write → Busan replica 100ms 안에 read)
- [ ] TimescaleDB Seoul primary → Busan standby streaming replication lag < 1s
- [ ] GSLB health check + DNS TTL 검증 (낮은 TTL 권장 ≤ 60s)
- [ ] Seoul 사이트 모의 down → 30초 안에 Busan 으로 트래픽 이전 (RTO 검증)
- [ ] 같은 사용자가 Seoul 로그인 → Busan 으로 강제 라우팅 → 정상 매매 (재로그인 거쳐서라도)
- [ ] 사이트 간 전용선 시뮬 down → split-brain 회피 (Busan 자동 disable, Seoul 만 active)
- [ ] Prometheus federation 동작 + Grafana 글로벌 view dashboard
- [ ] cross-site cookie_t mismatch 시 401 응답 (`reason="site_mismatch"`)
- [ ] 양 사이트 admin UI 가 정책 변경 시 동시 반영 보이는지 (etcd watch 효과)

---

## 11. 참고 문서

- `deployment-scenario-ha-channel.md` — **본 문서의 전제** (단일 사이트 / HA / 채널 분리)
- `deployment-software.md` — 배포 소프트웨어 명세
- `admin-ui-manual.md` — 운영 매뉴얼 (37 페이지)
- `../operations.md` — 서비스 flag/env + 부트스트랩 순서
- `../broker-reconnect.md` — supervisor goroutine 재연결 정책
- `../broker-tls.md` — broker cluster TLS / cross-site 합의안 (옵션 B 검토 시 필독)
- `../mci-price-ha.md` — mci-price 다중 인스턴스 HA — 사이트 간 분산 고려
- `../quoteid-operations.md` — quoteid allowlist 운영 (사이트 별 Registry 분리 정책)
- `../auth.md` — JWT + cookie_t passthrough
- (외부) AWS Route53 / Cloudflare Load Balancer / Google Cloud DNS 의 latency-based routing 문서
- (외부) etcd 5-node operations guide — `etcd.io/docs/v3.5/op-guide/`
- (외부) Redis Sentinel cross-DC guide
- (외부) PostgreSQL streaming replication + Patroni
