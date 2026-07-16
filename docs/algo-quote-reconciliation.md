# algo 시세 경로 대사표 (mds → WTG SubscribeAlgo)

내부 algo(autotrd `automkm`)가 소비하던 mds 시세와, WTG 대응(`mci-price
SubscribeAlgo` / `AlgoQuote`)의 **필드 단위 대조**. 외부고객 WS(§cs-ws-migration)
보다 **필드 충실도가 핵심인 경로** — algo 는 값으로 매매 판단을 하기 때문.

> 소스 근거: `nh/autotrd/automkm/{automkm.c,state.c}`, `nh/mds/include/mds.h`
> (`APSISE`/`MDFOLD`), `wtg/internal/price/algo.go`, `wtg/api/proto/wtg/v1/price.proto`.

## 1. 레거시 algo 수신 구조 (중요)

algo 는 UDP `APSISE` 를 **트리거(notify)** 로만 받고, **실제 가격은 공유메모리
`MDFOLD` 에서 읽는다** (`automkm.c`: `market_getfold(compid, code)`).

```
mds(WD9500) ──UDP APSISE(trigger)──▶ automkm ──SHM MDFOLD(실값)──▶ quote_t ──▶ state
                                       │
                                       └ APSISE 에서 쓰는 건 라우팅뿐: type("FA")/excode/symb
```

`state.c` 가 실제 쓰는 quote 필드: **bid, bid_best, offer, offer_best, mid, fillprc**.
refprctype 분기: `1`=체결가 / `2`=중간가 / `3`=bid·offer / `4`=cross-mid(CNH/KRW).

## 2. WTG SubscribeAlgo 가 전달하는 필드 (`AlgoQuote`)

| 필드 | 의미 |
|---|---|
| `sym` | 심볼 (external name) |
| `bid` / `ask` | **BEST** bid/ask (마진 미적용, BestConsumer 결과) |
| `seq` | 심볼별 monotonic — gap 감지/backfill |
| `ts_source_unix_ns` | feed cooker 발행 시각 |
| `ts_wtg_unix_ns` | mci-price 처리 시각 (latency 분해) |
| `is_backfill` | replay 여부 |

## 3. 필드 대사표

🟢 대응됨 · 🟡 대응되나 의미/형식 주의 · 🔴 gap (algo 가 쓰는데 미제공)

| algo 소비 (quote_t) | 출처 | 의미 | WTG `AlgoQuote` | 상태 |
|---|---|---|---|---|
| `bid` | MDFOLD | 매수호가(원천 last) | `bid` (BEST) | 🟡 WTG 는 BEST 만 — 원천 raw 아님 |
| `offer` | MDFOLD | 매도호가(원천 last) | `ask` (BEST) | 🟡 상동 |
| `bid_best` | MDFOLD | best 매수 | `bid` | 🟡 WTG `bid` 가 곧 best |
| `offer_best` | MDFOLD | best 매도 | `ask` | 🟡 WTG `ask` 가 곧 best |
| `mid` | MDFOLD | 중간가 (refprctype=2) | **없음** | 🔴 gap — §4 |
| `fillprc` | MDFOLD/fill | 체결가 (refprctype=1) | **없음** | 🔴 gap — §4 |
| `symb` | APSISE | 통화쌍 | `sym` | 🟡 표기 정규화(USD/KRW↔USDKRW) |
| `excode` | APSISE | 원천(SMB/KMB/EBS/CMB) | **없음** | 🔴 BEST 합성 → 원천 구분 소실 |
| `type`("FA") | APSISE | swap rate 구분 | **없음** | 🔴 swap 이면 별도 |
| (시각) `date`+`time` | APSISE | 발행시각 | `ts_source_unix_ns`(+`ts_wtg`) | 🟢 오히려 개선(2단 latency) |
| (순번) — | — | — | `seq` | 🟢 gap 감지 신규 제공 |

## 4. 핵심 gap (컷오버 전 반드시 해결)

1. **`fillprc` (체결가)** — `refprctype=1`(체결가 기준) algo 가 사용. **`SubscribeAlgo`
   는 호가 전용이라 체결가가 없다.** 체결가는 시세가 아니라 체결 이벤트이므로
   별도 스트림 필요 → WTG 의 체결 push(mci-push RTA 대체, `mds-replacement-plan`
   RTA 절) 로 받아 algo 측에서 병합해야 함. **호가 스트림만으로는 대체 불가.**
2. **`mid` (중간가)** — `refprctype=2` 사용. WTG 미제공 → 클라이언트가 `(bid+ask)/2`
   로 산출 가능하나, **mds `midprc` 가 단순 평균이 아닐 수 있음**(별도 필드) →
   mds 산식 확인 후 동일하게 계산하거나 `AlgoQuote` 에 `mid` 추가 검토.
3. **`excode` (원천)** — mds 는 원천별(SMB/KMB…) tick, algo 는 SMB/KMB 만 처리
   (`automkm.c`). WTG 는 **BEST 합성**이라 원천 구분이 없다. algo 가 원천 필터에
   의존하면 로직 재설계 필요(예: BEST 단일 소비로 전환).
4. **원천 raw vs BEST** — algo 가 `bid`(원천 last) 와 `bid_best` 를 **모두** 추적.
   WTG 는 BEST 하나만 → raw/ best 이원 추적 로직이면 축소됨.

## 5. 권고

- **fillprc/체결 병합**: algo 를 `SubscribeAlgo`(호가) + 체결 push(주문/체결) **2 스트림**
  구독으로 재구성. 호가만 쓰는 refprctype(2/3) 은 즉시 이관 가능, `1`(체결가)만 병합 대기.
- **mid**: mds `midprc` 산식 확인 → 단순 평균이면 클라 계산, 아니면 `AlgoQuote.mid` 추가.
- **excode**: BEST 단일 소비로 전환 가능한지 algo 정책 확인. 원천 지정이 필수면
  `SubscribeAlgo` 에 per-source tick 옵션 확장 필요 (BestConsumer 이전 raw).
- **검증**: mds UDP(APSISE) 캡처 + SHM MDFOLD 덤프를 기준값으로, `algo-tester`
  (SubscribeAlgo smoke) 수신값과 심볼·시각 정렬 후 bid/ask 오차 대사.

## 6. 판정 요약

| refprctype | algo 기준가 | WTG 로 즉시 이관 |
|---|---|---|
| 3 (bid/offer) | 호가 | 🟢 가능 (`bid`/`ask`) |
| 2 (mid) | 중간가 | 🟡 mid 산식 확인 후 |
| 4 (cross-mid) | CNH/KRW cross | 🟡 cross 산식 확인 후 |
| 1 (fill) | 체결가 | 🔴 체결 스트림 병합 선결 |

## 관련
- `docs/mds-replacement-plan.md` — mds 폐기 단계 + RTA(체결 push) 대체
- `docs/cs-ws-migration.md` §11 — 외부고객 WS 경로 대사
- `internal/price/algo.go` — `SubscribeAlgo`/`AlgoQuote` 구현
- `cmd/algo-tester` — SubscribeAlgo smoke 수신 dump
