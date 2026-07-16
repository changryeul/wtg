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
| `mid` | MDFOLD (`mdquot_calc_mid`) | 중간가 (refprctype=2) | `mid` = (bid+ask)/2 | 🟢 제공 (§5-B) |
| `fillprc` | MDFOLD (FIX 269=2 Trade) | 시장 체결가 (refprctype=1) | **없음** | 🔴 gap — forwarder 가 Trade drop 중 §4 |
| `symb` | APSISE | 통화쌍 | `sym` | 🟡 표기 정규화(USD/KRW↔USDKRW) |
| `excode` | APSISE | 원천(SMB/KMB/EBS/CMB) | **없음** | 🔴 BEST 합성 → 원천 구분 소실 |
| `type`("FA") | APSISE | swap rate 구분 | **없음** | 🔴 swap 이면 별도 |
| (시각) `date`+`time` | APSISE | 발행시각 | `ts_source_unix_ns`(+`ts_wtg`) | 🟢 오히려 개선(2단 latency) |
| (순번) — | — | — | `seq` | 🟢 gap 감지 신규 제공 |

## 4. 핵심 gap (컷오버 전 반드시 해결)

1. **`fillprc` (체결가 = 시장 last trade price)** — `refprctype=1` algo 가 사용.
   출처는 **FIX 시세 피드의 `MDEntryType='2'`(Trade)** 항목(`270`=MDEntryPx →
   fillprc, `271`→fillqty, MDEntryID→fillid; 35=X + MDUpdateAction=0 에서
   `fill_flag=1`). mds `fix.c` → SHM `MDFOLD` 저장 → algo 소비.
   **주의: 이는 고객 주문 체결이 아니라 시장 체결가(market data)다.** 따라서 별도
   체결 push 스트림이 아니라 **시세 파이프라인 안에서** 해결해야 한다. 그런데
   현재 **WTG quote-forwarder 는 Trade(269=2) 를 의도적으로 버린다**
   (`cmd/quote-forwarder/main.go`: "trade 는 silent skip", `TestExtractV1IgnoresTradeEntries`),
   `JSONEnvelope` 에도 체결가 필드가 없다(`Sym/Bid/Ask/TS/Src/Seq`).
   → **gap = 시세 경로가 Trade tick 을 드롭 중.** 해결: forwarder 가 269=2 파싱 +
   `JSONEnvelope` 에 `last`/`trade_px`(+qty) 추가 + `AlgoQuote` 에 필드 추가.
2. ~~**`mid` (중간가)**~~ — **해소됨(§5-B)**: 산식 = `(bid+ask)/2` (count 기반, 반올림
   없음) 확인 → `AlgoQuote.mid` 서버 계산 제공.
3. **`excode` (원천)** — mds 는 원천별(SMB/KMB…) tick, algo 는 SMB/KMB 만 처리
   (`automkm.c`). WTG 는 **BEST 합성**이라 원천 구분이 없다. algo 가 원천 필터에
   의존하면 로직 재설계 필요(예: BEST 단일 소비로 전환).
4. **원천 raw vs BEST** — algo 가 `bid`(원천 last) 와 `bid_best` 를 **모두** 추적.
   WTG 는 BEST 하나만 → raw/ best 이원 추적 로직이면 축소됨.

## 5. 권고

- **fillprc(시장 체결가)**: 별도 스트림 아님 — **시세 경로 확장**으로 복원.
  ① `quote-forwarder` 가 `269=2`(Trade) 파싱 (현재 skip) → ② `JSONEnvelope` 에
  `last`(+`last_qty`) 필드 추가 → ③ BestConsumer/`AlgoQuote` 로 전달. 호가만 쓰는
  refprctype(2/3/4) 은 즉시 이관, `1`(체결가)은 이 확장 후 이관.
- **mid**: ✅ 완료 — `(bid+ask)/2` 로 확인, `AlgoQuote.mid` 제공 (§5-B).
- **excode**: BEST 단일 소비로 전환 가능한지 algo 정책 확인. 원천 지정이 필수면
  `SubscribeAlgo` 에 per-source tick 옵션 확장 필요 (BestConsumer 이전 raw).
- **검증**: mds UDP(APSISE) 캡처 + SHM MDFOLD 덤프를 기준값으로, `algo-tester`
  (SubscribeAlgo smoke) 수신값과 심볼·시각 정렬 후 bid/ask 오차 대사.

## 5-A. 구현 상태 + 보류 후속 (2026-07-16)

**구현됨** — 시장 체결가(`last`) 시세경로 복원 (커밋 `83787d1`):
`quote-forwarder`(FIX 269=2 → `JSONEnvelope.Last/LastQty`) → `BestConsumer`
(per-symbol persist, mds MDFOLD 모델) → `AlgoQuote.last/last_qty`. **단, bid/ask 와
같은 메시지에 온 체결가(35=W 스냅샷 케이스)만** 커버.

**보류 — "호가 없는 체결"(quote-less trade)**: bid/ask 없이 `269=2` 만 담긴 35=X
메시지는 `fastExtractV1` 이 `ok=false` 로 drop → 놓침.
- **보류 사유**: 실 피드에서 발생하지 않을 것으로 판단(FX 시세는 호가 위주).
- **선결 조건**: 발생 여부 확인 후 진행. repo 의 `.trc` 는 전부 앱 텍스트 로그라
  FIX wire 캡처 없음(2026-07-16 스캔) → **라이브 tcpdump / mds UDP 캡처로** `269=2`
  가 `269=0/1` 없이 단독으로 오는 프레임이 실재하는지 확인해야 함.
- **발생 시 설계(방향 B, 권장)**: best/aggregator 를 우회하는 side-channel 로 체결을
  `AlgoStream` 에만 전달(봉/마진 오염 방지). 상세는 대화 이력 참조.

## 5-B. mid 산식 확인 + 제공 (2026-07-16)

algo `refprctype=2` 가 읽는 `quote.mid` = `pfold->mdquot->mid.last_valid` =
mds `mdquot_calc_mid` (mds.c:1588) 출력:
```
count = (bid.last>0) + (ask.last>0)
mid   = (bid.last + ask.last) / count      // 둘 다 → (bid+ask)/2, 한쪽 → 그 값
```
**반올림 없음.** (별도 `calc_mid`(mds.c:875) 는 USD/KRW 소수1자리 반올림하지만
고객/display 시세용이지 algo 의 mdquot mid 아님.)

**구현**: `AlgoQuote.mid` 를 서버에서 `(bid+ask)/2` 로 계산해 제공. AlgoQuote 는
bid·ask 둘 다 있을 때만 emit → one-side edge 없음 → 산식 정확히 일치.
잔차: mds mid 는 fold 의 last(원천) 기반, WTG 는 BEST 기반 — 이는 mid 산식이
아니라 기존 BEST vs last 모델 차이(§3 bid/ask 행과 동일 성격).

## 5-C. cross-mid (refprctype=4, CNH/KRW) 확인 + 배선 (2026-07-16)

algo `refprctype=4` 는 CNH/KRW 를 **재정환율로 합성** (`automkm.c`):
```
CNH/KRW bid = round( USDKRW.bid / USDCNH.ask , mrktzdiv )   // worse-side
CNH/KRW ask = round( USDKRW.ask / USDCNH.bid , mrktzdiv )
mid         = (bid+ask)/2  (한쪽 0 이면 다른 쪽)
```
**WTG `pkg/pricing.ComputeCross` 산식이 동일** — `CNHKRW = USDKRW(Mul) × USDCNH(Div)`:
bid=`USDKRW.bid × 1/USDCNH.ask`, ask=`USDKRW.ask × 1/USDCNH.bid` (div=worse-side).
`CrossRateConsumer` 가 `SourceCross` 로 emit.

**배선 gap 해소**: `AlgoStream.OnTick` 이 `SourceBest` 만 받아 cross 를 drop 했음 →
`SourceCross` 도 받도록 수정. 이제 algo 가 `SubscribeAlgo(["CNHKRW"])` 로 cross
pair 를 bid/ask/mid 와 함께 수신. (반올림은 downstream 소수자리 정책에 위임 —
mds mrktzdiv 대응은 SymbolEntry decimal 로 별도 관리.)

## 6. 판정 요약

| refprctype | algo 기준가 | WTG 로 즉시 이관 |
|---|---|---|
| 3 (bid/offer) | 호가 | 🟢 가능 (`bid`/`ask`) |
| 2 (mid) | 중간가 | 🟢 가능 (`mid`=(bid+ask)/2 제공, §5-B) |
| 4 (cross-mid) | CNH/KRW 재정 | 🟢 가능 (ComputeCross 동일 산식 + SourceCross 배선, §5-C) |
| 1 (fill) | 시장 체결가 | 🔴 forwarder 269=2 파싱 + envelope `last` 확장 선결 (별도 스트림 아님) |

## 관련
- `docs/mds-replacement-plan.md` — mds 폐기 단계 + RTA(체결 push) 대체
- `docs/cs-ws-migration.md` §11 — 외부고객 WS 경로 대사
- `internal/price/algo.go` — `SubscribeAlgo`/`AlgoQuote` 구현
- `cmd/algo-tester` — SubscribeAlgo smoke 수신 dump
