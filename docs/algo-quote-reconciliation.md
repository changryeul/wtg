# algo 시세 경로 대사표 (mds → WTG SubscribeAlgo)

내부 algo(autotrd `automkm`)가 소비하던 mds 시세와, WTG 대응(`mci-price
SubscribeAlgo` / `AlgoQuote`)의 **필드 단위 대조 + 이관 구현 결과**. 외부고객
WS(`docs/cs-ws-migration.md`)보다 **필드 충실도가 핵심인 경로** — algo 는 값으로
매매 판단을 하기 때문.

> 소스 근거: `nh/autotrd/automkm/{automkm.c,state.c}`, `nh/mds/{include/mds.h,
> WD9500/{fix.c,mds.c}}`, `wtg/internal/price/{algo.go,best.go}`,
> `wtg/cmd/quote-forwarder/main.go`, `wtg/pkg/{quote,pricing}`.

## 상태 요약 (2026-07-16)

| refprctype | algo 기준가 | 이관 | 커밋 |
|---|---|---|---|
| 3 (bid/offer) | 호가 | 🟢 | (기존) |
| 2 (mid) | 중간가 | 🟢 `mid`=(bid+ask)/2 | `11d4db3` |
| 4 (cross-mid) | CNH/KRW 재정 | 🟢 ComputeCross + SourceCross | `9e4c173` |
| 1 (fill) | 시장 체결가 | 🟢 `last` (co-present) | `83787d1` |
| — 원천별(excode) | per-source MM | 🟢 per-source 스트림 + backfill | `336159f`,`307d954` |

**보류 1건**: "호가 없는 체결" 단독 tick — 실 피드 발생 여부 확인 선결 (§5).
**본질적 잔차**: WTG 는 원천별/BEST 이원 대신 단일 값 모델 (§3 주).

## 1. 레거시 algo 수신 구조

algo 는 UDP `APSISE` 를 **트리거(notify)** 로만 받고, **실제 가격은 공유메모리
`MDFOLD` 에서 읽는다** (`automkm.c`: `market_getfold(compid, code)`).

```
mds(WD9500) ──UDP APSISE(trigger)──▶ automkm ──SHM MDFOLD(실값)──▶ quote_t ──▶ state
                                       └ APSISE 에서 쓰는 건 라우팅뿐: type("FA")/excode/symb
```

`state.c` 실사용 quote 필드: **bid, bid_best, offer, offer_best, mid, fillprc**.
refprctype 분기: `1`=체결가 / `2`=중간가 / `3`=bid·offer / `4`=cross-mid(CNH/KRW).
**중요**: state 가 `(compid=원천, symbol)` 키잉 → 원천별(SMB/KMB) 독립 마켓메이킹.

## 2. WTG `AlgoQuote` 필드 (SubscribeAlgo)

| 필드 | 의미 |
|---|---|
| `sym` | 심볼 (external name) |
| `bid` / `ask` | BEST(또는 per-source) bid/ask (마진 미적용) |
| `mid` | (bid+ask)/2 — refprctype=2 (`11d4db3`) |
| `last` / `last_qty` | 최근 시장 체결가/수량 — refprctype=1 (`83787d1`) |
| `source` | 원천 (BEST/CROSS/SMB/KMB…) — excode 대응 (`336159f`) |
| `seq` | stream(심볼 또는 source\|symbol)별 monotonic — gap 감지/backfill |
| `ts_source_unix_ns` / `ts_wtg_unix_ns` | feed 발행 / mci-price 처리 시각 (latency 분해) |
| `is_backfill` | replay 여부 |

`AlgoSubscribeRequest`: `symbols`, `sources`(비면 BEST 모드), `from_seq`(backfill).

## 3. 필드 대사표

🟢 대응됨 · 🟡 대응되나 의미/형식 주의 · 🔴 미대응

| algo 소비 (quote_t) | 출처 | 의미 | WTG `AlgoQuote` | 상태 |
|---|---|---|---|---|
| `bid` / `offer` | MDFOLD | 매수/매도호가 | `bid` / `ask` | 🟢 (BEST 또는 per-source `source`) |
| `bid_best`/`offer_best` | MDFOLD | best 호가 | `bid` / `ask` | 🟡 WTG bid/ask 가 곧 best |
| `mid` | MDFOLD (`mdquot_calc_mid`) | 중간가 (rpt=2) | `mid` = (bid+ask)/2 | 🟢 §4.2 |
| `fillprc` | MDFOLD (FIX 269=2) | 시장 체결가 (rpt=1) | `last` | 🟢 §4.1 (호가 동반分) |
| `excode` | APSISE | 원천(SMB/KMB…) | `source` (per-source 모드) | 🟢 §4.4 |
| `symb` | APSISE | 통화쌍 | `sym` | 🟡 표기 정규화(USD/KRW↔USDKRW) |
| `type`("FA") | APSISE | swap rate 구분 | 없음 | 🔴 swap 은 별도 (범위 밖) |
| `date`+`time` | APSISE | 발행시각 | `ts_source_unix_ns`(+`ts_wtg`) | 🟢 오히려 개선(2단 latency) |
| — | — | 순번 | `seq` | 🟢 gap 감지 신규 |

> **본질적 잔차**: mds 는 원천별 `bid`/`bid_best` 를 **이원 추적**하고 mid/cross 도
> fold 의 last 기반. WTG BEST 모드는 단일 합성값. per-source 모드로 원천별 값은
> 보존되나, "동일 심볼의 원천 raw + best 동시 추적" 구조는 단순화된다. 산식 차이가
> 아니라 모델 차이.

## 4. 이관 구현 결과 (gap 해소)

### 4.1 fillprc (시장 체결가, refprctype=1) — `83787d1`
출처 = **FIX 시세 `MDEntryType='2'`(Trade)** (`270`→fillprc, `271`→fillqty). mds
`fix.c` → SHM MDFOLD → algo. **고객 주문 체결이 아니라 시장 체결가**라 별도 스트림이
아닌 시세 경로로 복원:
- `quote-forwarder.fastExtractV1` 가 `269=2` 파싱 (기존 silent skip) → `JSONEnvelope.Last/LastQty`
- `BestConsumer` per-symbol persist (mds MDFOLD 모델) → best envelope 에 실음
- `AlgoQuote.last/last_qty`
- **범위**: bid/ask 와 같은 메시지에 온 체결가(35=W 스냅샷)만. 단독 체결은 §5 보류.

### 4.2 mid (중간가, refprctype=2) — `11d4db3`
`quote.mid` = `mdquot_calc_mid` (mds.c:1588):
```
count = (bid.last>0)+(ask.last>0);  mid = (bid.last+ask.last)/count   // 반올림 없음
```
(`calc_mid`(mds.c:875)의 USD/KRW 소수1자리 반올림은 고객/display 용, algo 아님.)
→ `AlgoQuote.mid` 를 서버 `(bid+ask)/2` 로 제공. AlgoQuote 는 bid·ask 둘 다 있을 때만
emit → one-side edge 없음 → 산식 일치.

### 4.3 cross-mid (refprctype=4, CNH/KRW) — `9e4c173`
CNH/KRW 재정환율 합성. mds(`automkm.c`)와 WTG(`pkg/pricing.ComputeCross`) **산식 동일**:
```
bid = USDKRW.bid / USDCNH.ask   ask = USDKRW.ask / USDCNH.bid   (div=worse-side)
```
`CrossRateConsumer` 가 `SourceCross` 로 emit. `AlgoStream.OnTick` 이 `SourceBest` 만
받던 배선 gap 해소 → `SourceCross` 도 수신. `SubscribeAlgo(["CNHKRW"])` 로 수신.

### 4.4 excode (원천 구분) — per-source 스트림 `336159f` + backfill `307d954`
automkm 은 원천별 MM (state 가 (원천,symbol) 키잉). BEST 로 뭉치면 깨짐 → per-source:
- `AlgoQuote.source` + `AlgoSubscribeRequest.sources`(필터)
- `Server.AddRawConsumer` — AlgoStream 이 BestConsumer *이전* raw tick(SMB/KMB) tap.
  main: `AddConsumer`(BEST/CROSS) + `AddRawConsumer`(raw) 병행.
- `OnTick`: `streamKeyFor` 로 합성=symbol / raw=source|symbol seq·ring 독립. per-source
  구독자는 지정 원천만, BEST 모드는 합성만. per-source 구독 0 이면 raw skip(perf).
- **backfill(Phase B)**: `replayKeys(sub)` 가 모드별 ring 키 산출 → `from_seq>0` 재구독
  시 원천별 ring 에서 정확히 replay + live dedup (streamKey 기준).
- **호환**: `sources` 미지정 = 기존 BEST 동작 그대로.

사용: `SubscribeAlgo(symbols=["USD/KRW","CNH/KRW"], sources=["SMB","KMB"])`.

## 5. 보류 / 남은 항목

- **"호가 없는 체결" 단독 tick** — bid/ask 없이 `269=2` 만 담긴 35=X 는 `fastExtractV1`
  이 drop. **보류**(실 피드 미발생 판단). 선결: 라이브 tcpdump/mds UDP 캡처로 `269=2`
  가 `269=0/1` 없이 단독으로 오는지 확인 (repo `.trc` 는 앱 로그라 wire 캡처 없음,
  2026-07-16 스캔). 발생 시 설계: best/aggregator 우회 side-channel 로 `AlgoStream` 에만
  전달(봉/마진 오염 방지).
- **`type`="FA" (swap rate)** — swap 시세는 별도 경로. 현 범위 밖.
- **검증(컷오버 전)**: mds UDP(APSISE) 캡처 + SHM MDFOLD 덤프를 기준값으로 `algo-tester`
  수신값과 심볼·시각 정렬 후 bid/ask/mid/last 오차 대사.

## 7. 결정적 e2e 검증 도구

- `cmd/mock-lp` — LP별 결정적 호가/체결(FIX 269=2) 시나리오 송신 (`--scenario`/`--once`).
- `scripts/mock-lp-verify.sh` — broker/etcd 없이 최소 스택 부팅 → mock-lp 송신 →
  `algo-tester --json` 수신값 대사. **BEST**(bid=max/ask=min/mid/last) +
  **per-source(SMB)** 원천값 자동 assert.
- `internal/price/mocklp_cross_integration_test.go` (`-tags integration`) — embedded
  etcd + 실 `EtcdPairWatcher`→`CrossRateConsumer` 배선으로 **cross(CNH/KRW)** 값을
  mds worse-side div 산식과 일치까지 검증.
- `cmd/algo-tester` — `--sources`(per-source) / `--json` / `--from-seq`(backfill).

## 관련
- `docs/mds-replacement-plan.md` — mds 폐기 단계 + RTA(주문/체결 push) 대체
- `docs/cs-ws-migration.md` — 외부고객 WS 경로 (§11 대사표)
- `internal/price/algo.go` — `SubscribeAlgo`/`AlgoQuote`/per-source/backfill
- `internal/price/best.go` — BestConsumer (last persist)
- `cmd/quote-forwarder/main.go` — FIX 269=2 파싱
- `cmd/algo-tester` — SubscribeAlgo smoke 수신 dump
