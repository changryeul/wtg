# 로이터 swap 피드 어댑터 스펙 (초안)

> **상태**: 초안. 실제 NH 로이터 전송 방식(포맷·프로토콜)이 확정돼야 구현. 본 문서는
> mds `WD9500/onrecv_swap` 의 실 wire 를 참고 스펙으로 남긴 것 — 확정 시 빠르게
> 어댑터를 붙이기 위함. **어댑터 뒤의 주입 seam(`POST /v1/pricing/swap-received`)은
> 이미 완성**되어 있어, 어댑터는 wire→endpoint 변환만 담당한다.

## 1. 위치 (이미 만든 것 vs 어댑터)

```
[로이터] ──wire(미정)──▶ [swap 어댑터]  ──POST /v1/pricing/swap-received──▶ ReceivedSwapStore
                          (본 스펙, 보류)      (안정 계약, 완성)                    │
                                                                          effective=received+delta
                                                                                   ▼
                                                                   forward_snapshot / AlgoStream(tenor)
```

어댑터는 **mci-price 프로세스 안(수신 goroutine)** 또는 **quote-forwarder** 어느 쪽에
둬도 된다. quote-forwarder 가 이미 멀티 UDP 피드 리스너라 자연스럽다(spot 과 별도 포트).

## 2. mds 참고 wire — `swaprate_t` (UDP)

전송: **UDP**, `xchg.apsnd` 포트 (spot 수신과 별개). `mds/WD9500/reuter.h`.
고정폭 ASCII, 총 **106 byte**, 선두 2byte 타입 마커 `"FB"`.

| offset | 필드 | 크기 | 의미 |
|---|---|---|---|
| 0 | `type` | 2 | `"FB"` 고정 (스왑 rate 구분 — 아니면 무시) |
| 2 | `rdcode` | 20 | realtime symbol = 원천(`'R'`) + symb |
| 22 | `tymd` | 12 | 영업일 |
| 34 | `symb` | 12 | 통화쌍 (예: `USD/KRW`) |
| 46 | `tenor` | 12 | 테너코드 (§4) |
| 58 | `kymd` | 12 | 날짜 YYYYMMDD |
| 70 | `khms` | 12 | 시간 HHMMSSSSS (msec) |
| 82 | `bid` | 12 | 매수 swap point (정수 문자열, §3 스케일) |
| 94 | `ask` | 12 | 매도 swap point (정수 문자열) |

## 3. 파싱 규칙 (mds `onrecv_swap` 기준)

```c
if (memcmp(data, "FB", 2)) return;           // 타입 마커 검증
symb  = trim(swap->symb);  tenor = trim(swap->tenor);
tenor_idx = tenor2index(tenor);  if (<0) return;   // 테너 유효성
// 스케일: wire 는 정수 pip, zdiv(소수자리)로 나눠 실수화 + 반올림
swap_bid = round(atof(swap->bid) / 10^zdiv, zdiv);
swap_ask = round(atof(swap->ask) / 10^zdiv, zdiv);
```
- **스케일링 핵심**: `bid`/`ask` 는 정수 문자열(pip). 실제값 = `정수 / 10^zdiv`.
  `zdiv` 는 **통화쌍별 소수자리**(운영 카탈로그). 예 USD/KRW zdiv=2 → wire `"255"` = 2.55.
- **작업시간 외 skip**, **수신시각/건수 카운터** 갱신 (운영 관측).

## 4. 테너 코드 매핑 (mds `TENOR_*` / tenor_nm)

| mds idx | 코드 | WTG tenor 라벨 |
|---|---|---|
| 0 | `SPT` | (spot — swap 없음, 어댑터 skip) |
| 1 | `TOD` | TOD |
| 2 | `TOM` | TOM |
| 3 | `W01` | W01 |
| 4 | `M01` | M01 |
| 5 | `M02` | M02 |
| 6 | `M03` | M03 |
| 7 | `M06` | M06 |
| 8 | `Y01` | Y01 |

> WTG `AlgoQuote.tenor` / `SwapKey.Tenor` 는 위 라벨을 그대로 사용(SPT 는 spot).
> forward tenor(TOD~Y01)만 swap 대상.

## 5. WTG 주입 매핑

`swaprate_t` 1건 → `POST /v1/pricing/swap-received`:
```json
{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":2.55,"ask":2.70}]}
```
- `pair` = `symb`(trim), `tenor` = `tenor`(trim, SPT 제외), `bid`/`ask` = **스케일 적용한 실수**.
- 배치로 여러 tenor 를 한 요청에 묶어도 됨(`updates[]`).

## 6. 운영자 조정(override)과의 관계 — 중요

mds `onrecv_swap` 은 **수동 override(`swap_bid_man`)가 설정돼 있으면 로이터 수신을
무시**한다(`if (!isnan(swap_bid_man)) return;`). 즉 mds 도 human override 개념이 있었다.

WTG 매핑:
- 로이터 수신 → `ReceivedSwapStore` (본 어댑터).
- 운영자 조정 → `PricingTable.SwapPoint`(delta) — 기존 경로.
- **effective = received + delta** (delta/skew 모델). mds 의 "override 시 수신 무시"는
  WTG 에선 "delta 로 조정"으로 대체(자동 무시 대신 조정값 합산). 완전 override 가
  필요하면 delta 대신 절대 override 필드를 admin 에서 선택(추후).

## 7. 확정 필요 (TBD — 구현 전 결정)

- **실 NH 로이터 전송 방식** = 위 UDP `swaprate_t` 그대로인가? (FIX/파일/API 가능성)
- **zdiv 소스** — 통화쌍별 소수자리 카탈로그(WTG `SymbolEntry`/`Pair.QuoteDecimals` 재사용).
- **어댑터 위치** — quote-forwarder(권장) vs mci-price 수신 goroutine.
- **override 정책** — delta 합산만 vs 절대 override 선택 (§6).

## 8. 테스트 (어댑터 없이 지금 가능)

주입 endpoint 로 직접 넣어 swap→algo 경로 검증:
```bash
curl -XPOST http://mci-price:8082/v1/pricing/swap-received \
  -d '{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":2.55,"ask":2.70}]}'
# SubscribeAlgo(symbols=USDKRW, tenors=M01) → spot + 2.55/2.70 확인
```

## 관련
- `docs/algo-quote-reconciliation.md` — algo swap 반영(§AlgoStream tenor)
- `internal/price/swap_received.go` / `swap_provider.go` — 주입 store + effective
- `internal/price/swap_received_http.go` — `POST/GET /v1/pricing/swap-received`
- mds `WD9500/{onrecv_swap,reuter.h}`, `include/mds.h`(TENOR_*) — 참고 원본
