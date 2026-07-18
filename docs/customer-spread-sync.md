# 고객별 스프레드 pricing (customer spread → override)

> NH 현행은 고객 **등급(tier)** 이 아니라 고객별 **스프레드**(별도 DB)로 가격을 매긴다.
> WTG 는 이미 이 모델(`CustomerMargin` override 레이어)을 갖고 있어, **엔진·런타임
> 무변경 + 데이터 feed(fx-sync) 추가**만으로 전환한다. (2026-07-19 설계 확정)

## 1. 핵심 — WTG 엔진에 이미 있음

`pkg/pricing/engine.go` 의 customer override:
```
Mode=override:  bid = BEST_bid − (swap + customer.BidDelta)   ← HQ/Site(tier) 무시
                ask = BEST_ask + (swap + customer.AskDelta)
```
= 고객별 절대 스프레드. 그릇은 `CustomerEntryDoc{CustomerID, Pair, BidDelta, AskDelta,
SkewDelta, SpreadDelta, Mode, Priority}`. **tier(HQ margin)는 override 없는 고객만 적용.**

## 2. 전체 경로

```
[고객 스프레드 DB] ──fx-sync SyncCustomerMargins──▶ etcd wtg/pricing/table.customer_margin[] (Mode=override)
   customer,pair,     (스프레드→BidDelta/AskDelta)          │ version++ watch (hot reload)
   bid/ask spread                                            ▼
런타임(이미 있음): 고객 ws → mci-edge-price(customerID=usid, --customer-stream)
              → mci-price SubscribeCustomerQuote
                 ├─ override 룰 O → 고객 스프레드 (bid=BEST−BidDelta)
                 └─ override 룰 X → HQ(tier)+Site  ← tier fallback (자동)
```

## 3. 변경 범위

| 부분 | 상태 |
|---|---|
| 엔진 apply (override→else tier fallback) | ✅ **이미 그렇게 동작 — 무변경** |
| 런타임 fan-out (customerID=usid, `--customer-stream`) | ✅ **이미 있음 — 무변경** |
| tier fallback (user-profile tier sync) | ✅ **존속** (스프레드 미등록 고객용) |
| **데이터 feed** (`SyncCustomerMargins` + `LoadCustomerSpreads`) | ✅ **구현·검증 완료 (File backend)** |

## 4. 구현 (fx-sync)

- `internal/fxsync/customerspread.go` — `CustomerSpread{Usid,Pair,BidDelta,AskDelta,Active}`
  + `customerSpreadsToEntries` (→ `CustomerEntryDoc` **Mode=override**, inactive 제외).
- `Backend.LoadCustomerSpreads` + `FileBackend`(`customer_margin.json`).
- `Syncer.SyncCustomerMargins` — `modifyPricingDoc` 로 `wtg/pricing/table.customer_margin`
  **부분 교체** (HQ/Site/Swap 레이어 보존, version++ → hot reload).
- `cmd/fx-sync --table=customer_margin` (`--table=all` 포함).
- 검증: 단위(매핑/파일) + **통합 e2e** — `sync → pricing doc → BuildPricingTable →
  ApplyForCustomer`: alice(override, tier 무시) + charlie(미등록, tier fallback) 값 대사.

## 5. 사용 (dev)

```bash
fx-sync --source=file --source-dir=./etc/db-mirror --table=customer_margin --etcd=127.0.0.1:2379
# → etcd wtg/pricing/table.customer_margin 에 alice01/bob02 override 반영
# → mci-price hot reload → alice01 ws 는 BEST±자기 스프레드 quote (tier 무시)
```

## 6. 확정 필요 (Oracle backend — 나중에)

1. **스프레드 DB 스키마** — 고객키(usid? 별도 고객번호?), pair, bid/ask spread 컬럼.
   - 고객키 ≠ usid 면 **usid↔고객번호 매핑 seam** 필요 (등급 sync 의 GradeMapper 유사).
2. **스프레드 의미** — BEST 로부터의 절대 spread(→ BidDelta/AskDelta 직결) 확인.
   폭/skew 별도 컬럼이면 `SpreadDelta`/`SkewDelta` 로.
3. **per-pair** — 고객 × pair 단위 (CustomerEntryDoc.Pair). 전 pair 공통이면 `pair=""`.

→ 확정 시 `oracle_backend.go` 에 `LoadCustomerSpreads` SELECT 추가 + (필요시) 고객키 매핑.
Syncer/엔진/런타임은 그대로.

## 관련
- `pkg/pricing/engine.go` — override / tier fallback apply
- `pkg/pricing/codec.go` — `CustomerEntryDoc` + `BuildPricingTable`
- `internal/fxsync/customerspread.go` / `syncer.go` (`SyncCustomerMargins`)
- `docs/auth.md` §6.5 — 고객 등급 tier sync (fallback 레이어)
- `docs/margin-policy.md` — 마진 정책 (레이어 정의)
