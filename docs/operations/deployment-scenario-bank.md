# 배포 시나리오 — 은행(금융권) 제안용 서버 구성

> 금융권(은행)에 WTG 아키텍처를 제안할 때의 **서버 구성 표준안**.
> 전제: **AP(매매엔진) 서버는 Active-Standby**, 그 앞단 WTG 게이트웨이 계층은
> Active-Active.
>
> **본 문서는 "최소 HA(물리 4대)"를 출발점으로 제안**하고, 정식 오픈/대형 규모의
> 풀 이중화는 [부록 A](#부록-a--풀-이중화-구성-정식-오픈대형)로 둔다. 단일 사이트
> 상세 설정은 [deployment-scenario-ha-channel.md](deployment-scenario-ha-channel.md),
> 다중 사이트/DR 은 [deployment-scenario-multi-site.md](deployment-scenario-multi-site.md) 참조.

---

## 1. 큰 원칙 한 줄

> **AP(매매엔진)만 Active-Standby. 그 앞단 WTG 게이트웨이 계층은 Active-Active.
> 상태 저장소(etcd/Redis/DB)는 각자의 정족수·복제 방식으로 이중화.**
> 은행이면 여기에 **망분리 3구간**을 얹고, 규모에 따라 이중화 수준을 단계적으로 올린다.

**왜 게이트웨이는 Active-Active 인가** — WTG 컴포넌트는 대부분 stateless 다
(TLS 종단·JWT 검증·IP 화이트리스트·passthrough·시세 fan-out). 상태를 들고 있지
않으므로 수평 확장이 자연스럽고, 접속/세션/인증 계층에 단일 장애점(SPOF)을 두지
않는다. AP 만 상태 기반 매칭 로직이라 Active-Standby 다.

---

## 2. 망분리 3구간 (금융권 필수)

금융권은 망분리가 규정이다. WTG 는 이미 이 구조로 설계돼 있어 **자연스럽게 망분리에
부합**한다 — 제안 시 강한 논거. 이 구간 개념은 최소 구성이든 풀 이중화든 동일하게 적용된다.

```
 ┌───────────────┐   방화벽   ┌────────────────────┐   방화벽   ┌──────────────┐
 │  외부/DMZ 구간  │◄────────►│   내부 업무 구간      │◄────────►│   DB 구간      │
 │               │          │                    │          │              │
 │  mci-edge-*   │          │  mci-api           │          │  etcd         │
 │  (TLS 종단     │          │  mci-push          │          │  Redis        │
 │   /인증/IP     │          │  mci-price         │          │  TimescaleDB  │
 │   화이트리스트  │          │  mci-chart         │          │              │
 │   /rate-limit) │          │  mci-admin         │          │              │
 └───────────────┘          │  ─── mymq wire ──► │          └──────────────┘
                            │        AP 서버      │
                            │  (mymqd+매매AP+매칭) │
                            └────────────────────┘
```

- **외부/DMZ** : 인터넷·외부 카운터파티가 닿는 유일한 구간. `mci-edge-*` 만 배치.
- **내부 업무** : WTG 내부 게이트웨이 + AP. 외부에서 직접 접근 불가.
- **DB 구간** : etcd/Redis/TimescaleDB. 내부 업무 구간에서만 접근.

> 최소 구성에선 구간을 **VLAN + 방화벽**으로 논리 분리한다. 물리 분리 요구가 있으면
> [부록 A](#부록-a--풀-이중화-구성-정식-오픈대형) 참조. 포트 카탈로그는
> [../conventions.md](../conventions.md), 서비스별 flag/env 는 [../operations.md](../operations.md).

---

## 3. 최소 HA 구성 (권장 출발점) · **물리 4대**

AP active-standby 만 요구대로 두고, 게이트웨이+저장소를 각 노드에 **통합**한다.
AP 1대·게이트웨이 1대가 죽어도 매매·접속이 지속되는 최소 무중단 구성.

### 3.1 논리 노드 → 물리 매핑

```
랙 A (전원계통 A)                     랙 B (전원계통 B)
──────────────────                   ──────────────────
[물리1] ap1 (ACTIVE)                  [물리2] ap2 (STANDBY)
        mymqd+매매AP+매칭                    mymqd+매매AP+매칭 (대기)
        + etcd                              + etcd
[물리3] gw1                           [물리4] gw2
        edge* + api/price/push/chart        edge* + api/price/push/chart
        + admin(active)                     + forwarder
        + etcd + redis(master) + DB         + redis(replica)
   [소프트 LB (HAProxy) 또는 DNS round-robin — gw1/gw2 에 얹음]
```

### 3.2 계층별 HA 방식 (최소)

| 컴포넌트 | 최소 HA 방식 | 근거 |
|--------|------------|-----|
| AP | **active-standby 2대** | cluster:11218 heartbeat, VIP 로 active 노출, WTG 자동 재연결 ([../broker-reconnect.md](../broker-reconnect.md)) |
| WTG 게이트웨이 | gw1/gw2 **active-active 2대** (edge+internal 통합) | stateless. mci-price best/봉은 broker feed 로 각자 재구성 ([../mci-price-ha.md](../mci-price-ha.md)) |
| etcd | **3 member 를 ap1/gw1/gw2 에 분산** | 한 대 죽어도 정족수 유지 (etcd 경량, AP 노드에 얹어도 무방) |
| Redis | gw1 master + gw2 replica + sentinel(3 노드에 얹기) | 세션 + `cookie_t` failover 복구 |
| TimescaleDB | **gw1 단일** | 챠트 영속만 담당 — 매매 경로 무관 (트레이드오프 수용) |
| forwarder | gw2 에 통합 | 전용 노드 없음. UDP feed 전용 NIC 만 분리 |
| LB | **소프트 LB(HAProxy) 또는 DNS round-robin** | 죽어도 DNS 로 우회 가능 |
| 관측 | gw1 에 Prometheus 1개 (또는 생략) | 사고 진단 |

### 3.3 최소 구성에서 지키는 것 / 포기하는 것

- **지킴**: AP active-standby, 게이트웨이 무중단(2대), 세션/설정 정족수(etcd·redis), 매매 경로 무중단.
- **포기(통합)**: DB 이중화, LB 이중화, 전용 노드(저장소/관측/forwarder), 게이트웨이 N대 확장, 물리 망분리.

> **유일한 SPOF 는 DB·LB 단일** — 그러나 둘 다 매매 경로가 아니다 (DB=챠트 영속,
> LB=접속 분산이라 죽어도 DNS 우회 가능). 정식 오픈 시 이 둘만 이중화하면 그대로
> 풀 구성으로 승격된다 (§5).

> **더 작게 (HA 불필요 — 데모/PoC/내부용)**: 1대 all-in-one 도 가능하다
> (AP + WTG 전체 + etcd/redis 단일 + DB + forwarder). WTG 는 `--no-broker` standalone,
> etcd/redis 단일 또는 MemoryStore 로 부팅. **SPOF 있음** — 정식 서비스엔 부적합.

---

## 4. 은행이 반드시 물어볼 질문 — 준비된 답변

> **Q. AP 가 Active-Standby 면 매매 처리량은 결국 active 1대에 묶인다.
> 게이트웨이를 Active-Active 로 늘려봐야 소용없지 않나?**

**A. 게이트웨이 A-A 의 목적은 매매 처리량 확장이 아니다.** 매매 RPC 처리량 상한은
AP active 1대 성능이 결정한다. 게이트웨이 A-A 는 다음을 위한 것:

1. **접속·세션·인증 계층 무중단** — edge/GW 한 대가 죽어도 서비스 지속.
2. **시세 fan-out 부하를 AP 에서 분리** — 시세/push 는 게이트웨이 계층에서 확산되므로
   AP 는 매매 RPC 에만 집중.
3. **DMZ 보안 계층 이중화** — 외부 접점의 단일 장애점 제거.

특히 WTG 는 시세·push 를 **broker 우회 경로**(mci-push HTTP push 트랙 B,
mci-price gRPC PublishTick)로 설계해 Active-Standby AP 의 부하와 SIGABRT 리스크를
덜어낸다 ([../broker-sigabrt-analysis.md](../broker-sigabrt-analysis.md)). 즉
**"단일 active AP 를 보호하는 아키텍처"** 라는 것이 핵심 논거다.

---

## 5. 단계적 확장 경로 (최소 → 풀 이중화)

규모·규정 요구가 커지면 아래 순서로 올린다. 각 단계는 재배포 없이 노드 추가로 가능.

| 단계 | 추가 | 얻는 것 |
|-----|-----|--------|
| ① DB 이중화 | TimescaleDB standby 1대 (Primary-Standby 복제) | 챠트 영속 SPOF 제거 |
| ② LB 이중화 | L4 어플라이언스 2대 (VRRP) | 접속 분산 SPOF 제거 |
| ③ 전용 노드 분리 | forwarder / 관측 / 저장소 하이퍼바이저 분리 | 부하 격리, 물리 망분리 여지 |
| ④ 게이트웨이 N대 | edge/int VM 증설 | 외부 트래픽/카운터파티 규모 대응 (mci-edge-price N6 fan-out) |
| ⑤ DR 센터 | 주센터 세트 복제 + GSLB | 재해복구 (금감원 규정) — [§6](#6-dr재해복구센터--대형정식) |

④·⑤ 를 다 반영한 형태가 [부록 A 풀 이중화 구성](#부록-a--풀-이중화-구성-정식-오픈대형)이다.

---

## 6. DR(재해복구센터) · 대형/정식

주센터 세트를 재해복구센터에 대칭 복제(또는 축소본) + GSLB. RTO/RPO 목표에 따라:

| 방식 | 설명 | 적합 |
|-----|------|-----|
| **Active-Standby-DR** | 주센터만 서비스, DR 은 대기(데이터만 복제) | RTO 분~시간 허용, 비용 절감 |
| **Active-Active + GSLB** | 양 센터 동시 서비스, 사용자 sticky | RTO 초, 무중단 요구 |

- **broker cluster 는 사이트별 독립** 권장 (split-brain 회피 —
  [deployment-scenario-multi-site.md §5](deployment-scenario-multi-site.md)).
- **etcd** : 단일 5-node Raft 를 양 사이트에 3+2 배치하거나, 사이트별 독립 후 카탈로그 동기.
- **Redis** : Sentinel cross-site. **TimescaleDB** : streaming replication.
- **quote_id / swap_id Registry** 도 사이트별 분리 (매칭엔진 검증 일관성).

---

## 7. 물리 구성 원칙 (최소·풀 공통)

- **가상화**: **AP·DB 는 물리 전용(bare-metal)** — 매칭엔진 저지연·DB I/O. 게이트웨이·etcd·redis 는
  가상화 허용. 폐쇄망 + native binary/systemd 이므로 하이퍼바이저는 VMware/KVM 기준,
  컨테이너 오케스트레이션은 미도입.
- **anti-affinity**: active/standby 짝(AP, DB, LB)은 **다른 랙 + 다른 전원계통 + 다른 스위치**.
  etcd/redis 정족수 노드는 서로 다른 물리 호스트에 분산.
- **전원**: 이중 PDU(A/B feed), 각 물리서버 이중 파워서플라이, 랙 단위 전원계통 분리.
- **네트워크**: 이중 ToR 스위치, 각 서버 **NIC bonding(LACP)**. 스위치 1대 죽어도 무중단.
- **시세 feed**: forwarder 에 **UDP feed 전용 물리 NIC/회선** 분리 (업무 트래픽과 격리, 커널 drop 회피).
- **스토리지**: AP·DB 로컬 NVMe, etcd 로컬 SSD(fsync). SAN 사용 시 이중 HBA/이중 패브릭.

---

## 8. 금융권 제안서 체크리스트

- [ ] **망분리 3구간** + 구간 간 방화벽 포트 화이트리스트
- [ ] **AP Active-Standby** + failover 자동 재연결 근거
- [ ] **게이트웨이·접속 계층 무중단** (최소 2대)
- [ ] **상태 저장소 정족수/복제** (etcd 3, Redis Sentinel)
- [ ] **인증/권한 분담** — WTG=인증(JWT/MFA/rate-limit/IP), 매매엔진=권한 ([../auth.md](../auth.md))
- [ ] **TLS/mTLS** — 모든 구간 암호화, 인증서 회전 ([../push-secret-rotation.md](../push-secret-rotation.md), [../broker-tls.md](../broker-tls.md))
- [ ] **관측/감사 로그** 보관 + 백프레셔 alert ([../observability.md](../observability.md))
- [ ] **NTP 시각 동기** — 시세 ts/quote_id 만료 판정 정확성
- [ ] **배포 방식** — native binary + systemd, 폐쇄망 반입 절차 ([../../deploy/README.md](../../deploy/README.md))
- [ ] (대형/정식) **DB·LB 이중화, DR 센터, 물리 망분리** — §5·§6

---

## 부록 A — 풀 이중화 구성 (정식 오픈/대형)

§5 확장을 모두 반영한 최대 무중단 구성. 은행 정식 오픈·대형 규모·엄격한 규정 대응용.

### A.1 계층별 HA 모델

| 구간 | 컴포넌트 | HA 모델 |
|-----|---------|--------|
| DMZ | mci-edge-* | Active-Active N대 (L4 뒤) |
| 내부 GW | mci-api, mci-push, mci-chart, mci-price | Active-Active N대 |
| 내부 GW | mci-admin | Active-Standby (1 active — etcd write 권한자) |
| 시세수신 | quote-forwarder | Active-Standby (VRRP) |
| AP | mymqd + 매매AP + 매칭엔진 | Active-Standby |
| 설정 | etcd | 3-node (대규모 5) |
| 세션 | Redis | Sentinel 3 (초대형 Cluster) |
| 시계열 | TimescaleDB | Primary-Standby 스트리밍 복제 |
| LB | L4/HAProxy | Active-Standby (VRRP) |
| 관측 | Prometheus/Grafana/Loki | 이중 |

### A.2 물리 풋프린트 (~10대) + 랙 배치

```
랙 A (전원계통 A)                         랙 B (전원계통 B)
──────────────────                       ──────────────────
[물리1] AP active (bare-metal)           [물리2] AP standby (bare-metal)
[물리3] HV-GW-1                          [물리4] HV-GW-2
        └ VM: edge1, int1                        └ VM: edge2, int2, admin
[물리5] HV-STORE-1                       [물리6] HV-STORE-2
        └ VM: etcd1, redis1                      └ VM: etcd2, redis2, obs
[물리7] DB primary (bare-metal)          [물리8] DB standby (bare-metal)
[물리9] HV-STORE-3                       [물리10] fwd (소형)
        └ VM: etcd3, redis3
[LB-A] L4 (VRRP master)                  [LB-B] L4 (VRRP backup)
[SW-A] ToR 스위치 pair                    [SW-B] ToR 스위치 pair
```

- 논리 15 노드가 물리 ~10대로 수렴. 정족수·복제 노드만 서로 다른 물리 호스트에 강제 분산.
- **물리 망분리 요구 시**: DMZ 용 스위치/방화벽을 물리 분리(별도 상면).
- 노드 상세 설정·호스트명 예시는 [deployment-scenario-ha-channel.md §2](deployment-scenario-ha-channel.md).

---

*본 문서는 은행 제안 관점의 구성 원칙 요약이다. 실제 설정 값(flag/env/카탈로그)은*
*[deployment-scenario-ha-channel.md](deployment-scenario-ha-channel.md) 를, 다중 사이트/DR 상세는*
*[deployment-scenario-multi-site.md](deployment-scenario-multi-site.md) 를 단일 진실로 참조한다.*
