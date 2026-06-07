# WTG 마진 업무 정의서

> 비즈니스/운영 관점에서 WTG 의 마진 정책을 어떻게 운용하는지 정의한 문서.
> 코드/구조 측 명세는 [`margin-policy.md`](./margin-policy.md), 재계산 절차는
> [`margin-recompute.md`](./margin-recompute.md) 참조.

---

## 1. 문서 목적 / 대상 독자

| 항목 | 내용 |
|---|---|
| 목적 | 마진 정책 운용 원칙·절차·권한·감사 기준을 단일 문서로 통합. 운영자가 의사결정 시점에 즉시 참조. |
| 1차 독자 | 딜링 / 자금팀 (마진 등록·변경 책임자) |
| 2차 독자 | 리스크팀 (정책 검증·회귀 분석), 컴플라이언스 (감사) |
| 3차 독자 | 시스템 운영자 (admin UI 조작), 개발팀 (요구사항 확정 시) |
| 갱신 주기 | 정책 변경 시점 / 분기 1회 점검 |

---

## 2. 마진의 비즈니스 정의

### 2.1 왜 마진을 받는가

| 목적 | 의미 |
|---|---|
| **수익** | 시장 호가와 고객 노출 호가의 차이 = 회사 수수료 (이익원) |
| **헷지 비용 회수** | 회사가 시장에서 hedging 하는 spread + 슬리피지 보전 |
| **신용 위험** | 결제일까지의 신용 노출 보상 (특히 선물환·만기 거래) |
| **변동성 대응** | 시장 변동성 / 유동성 부족 시 보수적 호가 |
| **인벤토리 헷지** | 회사의 통화별 long/short 잔량에 따라 한 방향 유도 |

### 2.2 가격 노출의 방향성 (worse-side)

고객 입장에서 항상 시장가보다 불리한 방향:

```
시장 (raw)  :  1300.00 (bid)  ─────  1300.10 (ask)
                  ↓                       ↑
고객 노출    :  1299.93        ─────  1300.17    ← 양쪽 widen
```

- **bid (고객이 매도)** : raw bid 보다 **낮음** → 회사가 더 싸게 매수
- **ask (고객이 매수)** : raw ask 보다 **높음** → 회사가 더 비싸게 매도
- 따라서 모든 마진 amount 는 통상 **양수** (`>= 0`)
- **예외**: Swap point 는 만기·금리차에 따라 음수 가능 — 부호 포함 그대로 합산

---

## 3. 마진 5-Layer 구조 + P6 신규 정책

### 3.1 5 Layer (누계)

| # | Layer | 결정 단위 | 운영 주체 | 변경 빈도 |
|---|---|---|---|---|
| 1 | **Swap point** | (pair, tenor) | 자금팀 | 매일 (만기 금리 변동) |
| 2 | **HQ margin (본점)** | (pair, tier, window?) | 본점 딜링 | 정기 (월 1회) + 시장 급변 시 |
| 3 | **Site margin (영업점·채널)** | (pair, channel, site, window?) | 본점 영업관리 | 정기 + 영업점 신규 시 |
| 4 | **Customer margin** | (customer_id, pair, window?) | 본점 (영업점 위임 X) | 계약 갱신 시 / 분쟁 후 |
| 5 | **TimeWindow** | window 정의 (시간대) | 본점 운영 | 신규 시간대 정책 시 |

### 3.2 P6 신규 — Skew + Spread (이번 분기 추가)

5 Layer 의 모든 카테고리 (Swap/HQ/Site/Customer) 에 **Skew Amount** 와 **Spread Amount** 가 추가됐다:

| 정책 | 정의 | 운영 의미 | 부호 |
|---|---|---|---|
| **Skew** (`skew_amount`) | bid 와 ask 모두 **같은 방향** shift | 딜러 인벤토리 헷지 (long 누적 시 +shift → 매도 유도) | 양수/음수 모두 |
| **Spread** (`spread_amount`) | bid/ask 폭을 추가 widen (절반씩 양쪽 적용) | 변동성·유동성 부족 대응 (보수적 호가) | 양수 (음수는 의미 X) |

### 3.3 통합 산식

```
skewSum   = swap.Skew   + hq.Skew   + site.Skew   + customer.SkewDelta
bidSum    = swap.Bid    + hq.Bid    + site.Bid    + customer.BidDelta
askSum    = swap.Ask    + hq.Ask    + site.Ask    + customer.AskDelta
spreadSum = swap.Spread + hq.Spread + site.Spread + customer.SpreadDelta

고객 노출 bid = raw.bid + skewSum − bidSum − spreadSum / 2
고객 노출 ask = raw.ask + skewSum + askSum + spreadSum / 2
```

| 모든 새 필드가 0 일 때 | 기존 산식 (skewSum=0, spreadSum=0) 과 정확히 동일 → 운영 마이그레이션 영향 X |
|---|---|

---

## 4. 카테고리별 정책 정의

### 4.1 HQ Margin (본점)

| 키 | (pair, tier, window?) |
|---|---|
| 운영 의미 | 등급별 기본 마진. 모든 영업점·채널이 받는 baseline. |
| Tier 종류 | VIP / GOLD / STD / `""` (와일드카드 fallback) |
| 우선순위 | 특정 tier 매칭 → 와일드카드 (`""`) 매칭 |
| window | 시간대 차등 (예: `regular` vs `off_hours`) — 미설정 시 모든 시간 |
| 변경 권한 | 본점 딜링팀 (등급별 정책 승인자) |

**예시:**
| pair | tier | bid_amount | ask_amount | skew_amount | spread_amount | window |
|---|---|---|---|---|---|---|
| USD/KRW | VIP | 0.02 | 0.02 | 0 | 0 | (전체) |
| USD/KRW | GOLD | 0.05 | 0.05 | 0 | 0 | (전체) |
| USD/KRW | STD | 0.10 | 0.10 | 0 | 0.05 | (전체) |
| USD/KRW | "" | 0.15 | 0.15 | 0 | 0 | (전체) |

### 4.2 Site Margin (영업점·채널)

| 키 | (pair, channel, site, window?) |
|---|---|
| 운영 의미 | HQ 위에 영업점·채널이 추가하는 마진 (영업점 자율 X — 본점이 결정). |
| Channel | WEB / MOB / HTS / CS / `""` |
| Site | BRANCH / HQ / DEALER / `""` |
| 우선순위 | 정확 매칭 → channel="" → site="" |
| 변경 권한 | 본점 영업관리팀 |

**예시:**
| pair | channel | site | bid_amount | ask_amount |
|---|---|---|---|---|
| USD/KRW | WEB | BRANCH | 0.05 | 0.05 |
| USD/KRW | WEB | HQ | 0.01 | 0.01 |
| USD/KRW | MOB | BRANCH | 0.07 | 0.07 |

### 4.3 Customer Margin (고객별)

| 키 | (customer_id, pair?, window?) |
|---|---|
| 운영 의미 | 특정 고객 (대형 법인·정책 고객) 별 추가/대체 마진. 본점 직접 등록. |
| Mode | `add` (HQ+Site 위에 누계) / `override` (HQ+Site 무시, Swap+Customer 단독) |
| Priority | 매칭 후보 여러 건 시 우선순위 큰 값 (default 0) |
| Pair | 비면 모든 pair (와일드카드, 운영 권장 X) |
| 변경 권한 | 본점 + 컴플라이언스 (계약 첨부 필수) |

**Mode 선택 가이드:**
- **`add` (default)** : VIP 할인 (`bid_delta = -0.01`), 특정 시간대 우대 — 대부분 운영
- **`override`** : 별도 계약 고객 (예: 정부·공공기관 고정 spread) — 본점 결재 필요

### 4.4 Swap Point (선물환 만기별)

| 키 | (pair, tenor) |
|---|---|
| 운영 의미 | 결제일 차이에 따른 금리·만기 비용. SPOT 외 (1W/1M/3M/6M/1Y...) 거래 시 적용. |
| 부호 | **음수도 허용** — 금리차에 따라 한 방향이 마이너스 swap |
| 변경 빈도 | 매일 (시장 close 후 자금팀이 다음 영업일 swap 갱신) |
| broken-date | tenor 정확 일치 안 하는 임의 결제일 → **선형 보간** (`ApplyForValueDate`) |

**예시:**
| pair | tenor | bid_amount | ask_amount |
|---|---|---|---|
| USD/KRW | SPOT | 0 | 0 |
| USD/KRW | 1W | 0.05 | 0.07 |
| USD/KRW | 1M | 0.15 | 0.25 |
| USD/KRW | 3M | 0.40 | 0.55 |

### 4.5 Time Window (시간대 차등)

| 키 | window 이름 |
|---|---|
| 운영 의미 | 정규장 vs 시간외 등 시간대 차등 — HQ/Site/Customer entry 의 `window` 필드가 참조. |
| 정의 방법 | `start`/`end`/`tz`/`days` (요일) 명시 또는 `complement_of` (다른 window 의 보집합) |

**예시:**
| name | start | end | tz | days | complement_of |
|---|---|---|---|---|---|
| regular | 09:00 | 15:30 | Asia/Seoul | MON-FRI | — |
| off_hours | — | — | — | — | regular |

→ `off_hours` 는 `regular` 의 정확한 보집합. 별도 시간 설정 불필요.

### 4.6 Holidays (휴일 캘린더)

| 키 | 날짜 set |
|---|---|
| 운영 의미 | 영업일 계산 (특히 broken-date swap 보간) 시 휴일 제외. 매년 갱신. |
| 등록 단위 | YYYY-MM-DD 리스트 (KOR 휴일 + 양국 통화 휴일 별도) |
| 변경 권한 | 본점 자금팀 (연 1회) |

---

## 5. 운영 워크플로우

### 5.1 일별 운영 (Daily)

| 시각 | 주체 | 작업 |
|---|---|---|
| T-1 16:00 | 자금팀 | 다음 영업일 Swap point 산출 (시장 close 후 금리·만기 반영) |
| T-1 16:30 | 자금팀 | admin UI → [마진 테이블] → swap_point 갱신 → 저장 |
| T 08:50 | 운영자 | etcd watch 반영 + mci-price 의 hot reload 확인 |
| T 09:00 | 영업관리 | 시장 개장 전 변동 점검 |
| T 일중 | 딜링 | 인벤토리 잔량 점검 → 필요 시 Skew 조정 (HQ Margin) |
| T 15:30 | 운영자 | 일중 매매 통계 검토 ([매매 통계] / [매매 감사]) |

### 5.2 정책 변경 절차 (HQ/Site Margin)

```
1. 정책 변경 요청 (내부 결재) 발생
       │
       ▼
2. [마진 변경 미리보기] 에서 시뮬레이션
   - profiles × pairs 매트릭스 실행
   - 변경 전/후 spread 비교
   - delta 검토 (지나치게 큰 변동 없는지)
       │
       ▼
3. 결재 승인 → [마진 테이블] 에서 raw JSON 편집 또는 그리드 편집
       │
       ▼
4. ▶ 저장 → etcd PUT → 모든 mci-price 가 atomic hot reload
       │
       ▼
5. 효력 발생 시점 = 저장 직후 (운영자 책임 시각)
       │
       ▼
6. [감사 로그] 에 PUT_PRICING 기록 (변경자 usid + 시각 + diff)
```

### 5.3 회수·재계산 (분쟁 발생 시)

| 단계 | 도구 | 결과 |
|---|---|---|
| 1. 분쟁 시점 파악 | [매매 감사] | trace_id + 호가 + 시각 + customer_id |
| 2. 그 시점의 PricingTable Version 확인 | [감사 로그] | PUT_PRICING 의 version |
| 3. raw 시세 + 옛 마진 재적용 | [마진 재계산] | "고객이 받았어야 할 호가" 재구성 |
| 4. 실제 매매 호가와 비교 | UI 결과 | 차이 = 시스템 오류 vs 정책 변경 영향 |
| 5. 결과 처리 | 컴플라이언스 | 환불 / 추가 정산 / 정책 회귀 |

자세한 절차: [`margin-recompute.md`](./margin-recompute.md)

---

## 6. 운영 시나리오 (예시)

### 6.1 VIP 고객 신규 등록

**상황**: 고객 `VIP-001` 과 우대 계약 체결 (USD/KRW 마진 50% 인하).

**작업:**
1. [마진 테이블] → Customer Margin 탭 → `+ 행`
2. 입력:
   - `customer_id = VIP-001`
   - `pair = USD/KRW`
   - `bid_delta = -0.05` (HQ STD 0.10 → 절반 인하 효과)
   - `ask_delta = -0.05`
   - `mode = add`
   - `priority = 100`
3. ▶ 저장 → mci-price 즉시 반영
4. [마진 변경 미리보기] 에서 검증:
   - `customer_id=VIP-001, pair=USD/KRW, tenor=SPOT, sample_bid=1300, sample_ask=1300.10` 입력
   - 결과: bid=1299.95, ask=1300.15 (STD 마진 0.10 - VIP delta 0.05 = 0.05 적용)

### 6.2 USD 인벤토리 long 누적 → Skew 적용

**상황**: 자금팀 모니터링 — USD 매수 잔량 누적 (이번 주 +5M). 시장 추가 매수 부담 → Skew 로 고객에게 분산.

**작업:**
1. [마진 테이블] → HQ Margin 탭 → USD/KRW 의 모든 tier 행
2. `skew_amount = +0.03` 입력 (양쪽 3pip 위로 shift)
3. 효과:
   - bid 1300 → 1300.03 (고객이 매도 시 더 비싸게 받음 — 회사 매수 부담 감소)
   - ask 1300.10 → 1300.13 (고객이 매수 시 더 비싸게 — 회사 매도 유도)
4. 일중 잔량 정상화 시 0 으로 복귀
5. [감사 로그] 에 PUT_PRICING 으로 기록 — 잔량 변화 분석 시 추적 가능

### 6.3 변동성 급증 시 Spread 확대

**상황**: 한국시장 정치 이벤트 → KRW 변동성 +200%.

**작업:**
1. [마진 테이블] → HQ Margin → USD/KRW STD 행
2. `spread_amount = 0.20` 입력 (양쪽 10pip씩 추가 폭)
3. 효과: bid −5pip / ask +5pip 추가 → 고객 노출 spread 가 0.10 → 0.50 으로 widen
4. 정상화 시 0 으로 복귀

### 6.4 시간외 자동 차등 (Time Window)

**상황**: 시간외 (15:30~ 다음 08:50) 거래는 변동성 큼 → 추가 마진.

**작업:**
1. [마진 테이블] → Time Window 탭
   - `regular`: 09:00-15:30 / Asia/Seoul / MON-FRI
   - `off_hours`: `complement_of: regular`
2. HQ Margin 에 같은 (pair, tier) 의 2 entry 추가:
   - USD/KRW STD `window=regular` bid=0.10 ask=0.10
   - USD/KRW STD `window=off_hours` bid=0.15 ask=0.15
3. mci-price 가 현재 시각에 활성 window 만 매칭 → 자동 차등

---

## 7. 권한·감사

### 7.1 권한 매트릭스

| 역할 | 조회 | 변경 | 승인 |
|---|---|---|---|
| 본점 딜링 | ✓ 모든 카테고리 | ✓ HQ Margin, Swap | (자체 승인) |
| 본점 자금 | ✓ 모든 카테고리 | ✓ Swap, Holiday, TimeWindow | (자체 승인) |
| 영업관리 | ✓ Site/HQ | ✓ Site Margin | 본점 딜링 |
| 영업점 | ✓ 자기 Site/Profile | ✗ (조회만) | — |
| 컴플라이언스 | ✓ 모든 카테고리 + 감사 로그 | ✗ | — |
| 시스템 운영자 | ✓ 모든 카테고리 (admin) | ✗ (정책 변경 X, infra 만) | — |

### 7.2 감사 항목

| 이벤트 | 기록 위치 | 보존 |
|---|---|---|
| PricingTable 저장 (PUT) | [감사 로그] action=`PUT_PRICING` | 7년 |
| 그리드 행 추가/삭제 | (위와 동일 PUT 안에 포함) | 7년 |
| Customer Margin 등록 | action=`PUT_PRICING` + customer_id | 7년 |
| 마진 재계산 실행 | action=`MARGIN_RECOMPUTE` | 5년 |
| 매매 호가 자체 (고객별) | TimescaleDB `quote_bars` + Redis tx ring | 5년 (bars), 30일 (ring) |

→ 모든 PricingTable 변경은 `version` 자동 증가 + `updated_at` / `updated_by` 자동 기록.

---

## 8. UI 가이드 (admin → 마진 정책 그룹)

| 페이지 | 용도 | 빈도 |
|---|---|---|
| **마진 테이블** | PricingTable raw 편집 (5 카테고리 그리드 + 원본 JSON) | 매일 (Swap) / 주 1회 (HQ/Site) |
| **마진 정책 명세** | 본 문서 + `margin-policy.md` 의 산식 표시 | 참조 (변경 X) |
| **마진 계산기 (5-Layer)** | 임의 입력으로 5 layer 별 기여도 시각화 | 분기·검증 시 |
| **마진 재계산** | 분쟁 시 옛 PricingTable + 봉 재적용 | 분쟁 발생 시 |
| **마진 변경 미리보기** | 변경 전/후 시뮬레이션 (단일 + 매트릭스) | 정책 변경 결재 시 |

### 8.1 매트릭스 시뮬레이션 (대량 검증)

```
Profiles (한 줄에 하나, Channel.Site.Tier):
WEB.BRANCH.VIP
WEB.BRANCH.STD
WEB.HQ.VIP

Sample Quotes (JSON):
{
  "USD/KRW": {"bid":1380.00, "ask":1380.10},
  "EUR/USD": {"bid":1.0850, "ask":1.0852}
}

Tenor: SPOT

Changes (변경 PricingTableDoc 일부) → ▶ 매트릭스 실행
→ 결과: 6 cell (3 profiles × 2 pairs) 의 before/after/delta 비교
```

---

## 9. FAQ + 운영 주의사항

### Q1. 정책 저장 후 언제부터 적용?

**A.** 저장 즉시 (etcd PUT → mci-price watch 비동기, 일반 < 500ms). 다음 tick 부터 새 마진. 적용 시각은 운영자 책임 — 저장 시각이 곧 효력 시각으로 감사 기록.

### Q2. Skew 와 Spread 의 차이는?

**A.** Skew 는 **방향성 시프트** (bid+ask 같이 위/아래) — 인벤토리 헷지. Spread 는 **폭 확대** (bid 내림 + ask 올림) — 변동성 보수화. 두 정책은 독립적으로 누계.

### Q3. Customer override 와 add 모드 차이는?

**A.** 
- `add`: HQ + Site + Customer 누계 → VIP 할인 같은 미세 조정에 사용
- `override`: HQ + Site **무시** + Swap + Customer 단독 → 정부·공공 고정 계약에만 사용. 결재 필수.

### Q4. Time window 가 겹치면?

**A.** Window 매칭 정책 = **첫 매치 우선**. `(pair, tier)` 같은데 window 가 `regular`/`off_hours` 둘 다 등록되어 있으면 둘 다 lookup 후보가 됨. 매칭 활성 window 가 단일이도록 정의 (`off_hours = complement_of regular` 가 안전).

### Q5. Cross-rate (USDKRW × USDJPY = JPYKRW) 의 마진은?

**A.** Cross 는 base/quote 양쪽 leg 의 best (BEST) tick 으로 합성한 후, **합성 cross 호가** 에 마진 적용. 즉 cross pair 도 별도 (`pair=JPYKRW`) HQ/Site/Customer 마진 등록 가능. 미등록 시 leg 의 와일드카드 (`""`) fallback.

### Q6. Swap point 가 음수면?

**A.** 그대로 합산. 예: USD/KRW 1M swap bid = -0.10 (kor 금리 < 미 금리) → bid 산식에서 `−(−0.10)` = `+0.10` (가산) — 즉 swap 이 음수면 고객에게 유리. 자금팀 의도된 동작.

### Q7. 변경 직후 고객 호가 검증은?

**A.** [마진 변경 미리보기] 에서 raw 호가 입력 → 새 doc 적용 결과 확인. 또는 [시세] 페이지에서 라이브 ws 로 실시간 quote 받아 비교. ws envelope 의 `src=BEST` (raw) vs profile 별 quote (적용 후) 자동 비교 가능.

### Q8. 정책 변경 회귀 (이전 상태 복구) 가능?

**A.** Audit log 에 모든 PricingTable PUT 이 diff 로 기록 → 운영자가 직전 doc 을 그대로 다시 PUT 하면 복구. 자동 rollback 버튼 없음 (의도된 안전장치). 분쟁이라면 마진 재계산 → 환불 처리가 더 안전.

---

## 10. 변경 이력

| 일자 | 작성자 | 변경 |
|---|---|---|
| 2026-06-07 | 초안 | P6 (Skew/Spread) 반영 + 운영 워크플로우 정리 |

---

## 11. 관련 문서

- [`margin-policy.md`](./margin-policy.md) — 산식 / 코드 측 명세
- [`margin-recompute.md`](./margin-recompute.md) — 재계산 절차
- [`conventions.md`](./conventions.md) — Channel/Site/Tier 정의
- [`auth.md`](./auth.md) — Site/Tier 추가 후 인증 위임
- [`operations.md`](./operations.md) — 서비스별 flag/env + 부트스트랩 순서
