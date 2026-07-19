# 시세 파이프라인 HA (gRPC-only) — 다중 인스턴스 설계

> 신규 구축(시세 gRPC-only, `docs/../` broker 미사용) 기준의 mci-price 다중 인스턴스 HA.
> broker FANOUT 이 공짜로 주던 fan-in 을 **forwarder subscribe 허브**로 대체한다.
> 결정성/archiver/quoteid 등 **코어 HA 는 `docs/mci-price-ha.md` 가 그대로 유효** —
> 본 문서는 gRPC-only 로 바뀐 **fan-in + 수집 + warm-up** 만 재설계한다. (2026-07-19)

## 0. 핵심 — HA 의 90%는 이미 풀려 있음

mci-price 코어는 **Active-Active** 가 자연스럽다. 모든 consumer 가 결정적(같은 입력→같은
출력)이라 N 대가 각자 같은 BEST/quote 를 낸다:

| 이미 해결 (mci-price-ha.md) | 근거 |
|---|---|
| BEST/Aggregator/Pricing/Cross 충돌 없음 | 메모리 state, 결정적 산출 (§1.2) |
| etcd 카탈로그 일관 | 모든 인스턴스 같은 watch (§1.3) |
| Archiver → TimescaleDB 중복 | `ON CONFLICT DO NOTHING` first-wins (§1.4) |
| edge → mci-price failover | gRPC `round_robin` + healthcheck (§2.1) |
| QuoteID 인스턴스 무관 | Redis SETNX atomic (§2.2) |

**유일한 gap**: broker FANOUT 이 forwarder 1-publish → 전 인스턴스 구독을 공짜로 줬는데,
gRPC PublishTick 은 점대점이라 N 인스턴스에 스트림을 못 뿌린다. 이걸 재설계한다.

## 1. fan-in — forwarder subscribe 허브 (방향 뒤집기)

```
기존: forwarder(client) ─PublishTick→ mci-price(server)      점대점, N 못 뿌림
설계: forwarder(server) ◀─SubscribeTicks── mci-price(client)  구독 모델
      forwarder 가 구독자(dial-in 한 mci-price) 전체에 tick fan-out
```

- **forwarder**: `TickService.SubscribeTicks(stream Tick)` gRPC **서버**. 구독자 Registry
  보유. worker 가 parse+batch 후 `Broadcast(envelope)` 로 구독자 전체 push.
  - per-subscriber send queue + **slow-consumer 격리** (느린 mci-price 가 남을 안 막음).
    → mci-edge-price 의 Registry/Broadcast/eviction 과 동일 패턴 재사용.
- **mci-price**: `TickSubscriber` 클라이언트가 forwarder(들)에 dial → 스트림 수신 →
  기존 BestConsumer/PricingConsumer 경로 주입 (broker subscribe 자리 대체).
  - **reconnect supervisor**: forwarder 재시작 시 self-heal (재 dial).

> broker subscribe 모델을 gRPC 로 옮긴 것 — 검증된 패턴, etcd discovery 불요
> (구독자가 dial 로 자기를 등록, 끊기면 자동 이탈).

## 2. 수집 (forwarder HA) — 결정: dual-active + dedup

forwarder 자체가 SPOF(UDP 못 받으면 시세 끊김) → **≥2 forwarder**.

**채택: dual-active + (source, seq) dedup** (zero-gap):
- 각 mci-price 가 **양쪽 forwarder 에 구독** → 중복 tick 수신.
- mci-price 가 ingest 에서 **(source, seq) 로 dedup** — 이미 본 (원천, 시퀀스)는 drop.
  BEST(max/min)는 멱등이라 중복이 무해하지만, **체결(last)·bar 는 중복 집계 방지**로 dedup 필수.
- 한 forwarder 죽어도 다른 forwarder 스트림으로 무손실 지속.

대안(미채택): VIP active-standby (keepalived) — dedup 불요·단순하나 **전환 갭** 발생.

## 3. 코어 — warm-up gate (신규)

Active-Active/etcd/archiver/quoteid 는 기존 그대로 (§0). **추가되는 것 하나**:

**warm-up gate** — 갓 뜬(또는 재연결한) 인스턴스는 아직 모든 source tick 을 못 봐 BEST 가
불완전하다. 그 사이 서빙하면 반쪽 BEST 를 낸다.
- 부팅 후 **healthcheck 를 not-ready** 로 유지 → edge round_robin 이 skip.
- ready 조건: **활성 source 별 ≥1 tick 수신** 또는 **grace 시간(예 2s) 경과** 중 먼저.
- 충족 시 ready → edge 가 라우팅 시작. → **조용히 합류 후 warm-up → 서빙**.
- `/v1/ready` (또는 gRPC health) 를 edge/LB healthcheck 가 소비.

**Archiver 결정: dedup-only** (`ON CONFLICT DO NOTHING`). 정합성은 이미 보장.
DB write 부하 최적화가 필요하면 후속으로 leader-elect(etcd lease 로 1대만 write) 추가 —
현재는 불필요.

## 4. fan-out + 클라이언트 (기존 유지)

- **edge → mci-price**: gRPC `round_robin` resolver + healthcheck (mci-price-ha.md §2.1).
  인스턴스 죽으면 자동 failover. edge 는 stateless → N 대 LB 뒤.
- **ws → 고객**: edge/mci-price 죽어도 클라 재연결 + `from_seq` backfill
  (`docs/client-quote-subscribe.md` §5 lifecycle).

## 5. 전체 그림

```
① LP UDP FIX ─▶ forwarder A, B          수집. dual-active + (source,seq) dedup
                   │ SubscribeTicks 허브 (구독자 fan-out + slow evict)
② mci-price #1,#2,#3 ─dial-in 구독(양 forwarder)─┘   각자 full 스트림 → 결정적 BEST
③ 코어: 결정적 + Archiver ON CONFLICT + Redis quoteid + warm-up gate(ready)
④ mci-price ─SubscribeQuote(round_robin)─▶ mci-edge-price (stateless, N + LB)
⑤ edge ─ws─▶ 고객 (재연결 + from_seq backfill)
```

## 6. failover 시나리오

| 사건 | 동작 |
|---|---|
| mci-price 1대 죽음 | edge round_robin 이 healthcheck 로 배제, 나머지가 full 스트림으로 계속 서빙. 클라 재연결·Algo backfill |
| forwarder 1대 죽음(2중) | mci-price 가 그 스트림 끊김, 다른 forwarder 로 계속(이미 dedup 중). **zero-gap** |
| 신규 mci-price 합류 | forwarder dial → warm-up gate → ready → edge 라우팅. 조용한 합류 |
| seq 불연속(failover) | 인스턴스별 seq → 점프 → 클라 `from_seq` backfill, ts 정렬 |

## 7. 구현 범위

| 컴포넌트 | 상태 |
|---|---|
| forwarder 팬아웃 허브 core (Registry + Broadcast + slow evict) | ✅ `internal/forwarder/tickhub`, TDD |
| proto `TickIngestService.SubscribeTicks` | ✅ price.proto + 생성 |
| forwarder gRPC 서버 배선 + `--publish-mode hub` + `--tick-listen` | ✅ `cmd/quote-forwarder/hub_server.go` |
| mci-price `TickSubscriber` client + reconnect + `--tick-source` | ✅ `internal/price/tick_subscriber.go` (dial-in + backoff 재연결) |
| e2e 스모크 (forwarder hub → mci-price dial-in → BEST) | ✅ 값 검증 통과 |
| (source,seq) dedup — 다중 forwarder dual-active HA | ✅ `internal/price/tick_dedup.go` (per-source HWM + reset). 다중 --tick-source 시 자동 활성 |
| warm-up gate + `/v1/ready` | ✅ `internal/price/readiness.go` (warmup/maxWarmup + tick gate). /v1/ready 503→200 |
| e2e (N mci-price Active-Active + failover) | ✅ `scripts/price-ha-verify.sh` (1 hub → 2 mci-price: 결정성+warm-up+failover 자동 검증) |

기동 예:
```bash
# forwarder — 허브 서버
quote-forwarder --publish-mode hub --tick-listen 127.0.0.1:50060 --multi SMB:30044,KMB:30045
# mci-price — dial-in 구독 (N대가 같은 허브 dial → Active-Active)
mci-price --no-broker --tick-source 127.0.0.1:50060 --grpc 127.0.0.1:50051 ...
```

자동 검증: `scripts/price-ha-verify.sh` (1 hub → 2 mci-price Active-Active + warm-up + failover).

## 관련
- `docs/mci-price-ha.md` — 코어 HA (결정성/archiver/quoteid/edge round_robin) — **여전히 유효**
- `docs/client-quote-subscribe.md` §5 — ws 구독 lifecycle / 재연결
- `internal/edge/price/registry.go` — 재사용할 fan-out Registry 패턴
- `cmd/quote-forwarder/` — 허브 서버가 될 곳
