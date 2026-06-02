# WTG 마진 정책 명세

WTG (Winway Trading Gateway) 가 raw 시장 호가에 마진을 적용해 고객 노출
호가를 산출하는 산식 정의서. 운영자가 정책 변경 의사결정 시점에 참조하고,
회사별로 달라질 수 있는 분기 항목을 명시.

코드 참조: `pkg/pricing/engine.go` (Apply / ApplyAt / ApplyForCustomer /
ApplyForValueDate), `pkg/pricing/crossrate.go` (ComputeCross),
`pkg/pricing/calendar.go` (Calendar / HolidayCalendar).

---

## 1. 5-Layer 마진

WTG 의 마진은 다음 5 layer 의 누계로 산출:

| # | Layer | 출처 (DB) | 적용 대상 |
|---|-------|-----------|----------|
| 1 | **Swap point** | TB_FXB_CMG021M | `(pair, tenor)` |
| 2 | **HQ margin (본점)** | TB_FXB_CMG019M | `(pair, tier)` |
| 3 | **Site margin (영업점)** | TB_FXB_CMG015M | `(pair, channel, site)` |
| 4 | **Customer margin** | TB_FXB_CMG016M / 038M | `(customer_id, pair)` |
| 5 | **TimeWindow** | (운영 추가) | 위 layer 의 entry 별 시간대 매칭 |

### 부호 규칙 (worse-side)

- bid (고객 매도 가격) 는 **차감** — 낮아짐 (고객 불리)
- ask (고객 매수 가격) 는 **가산** — 높아짐 (고객 불리)
- swap point 는 **음수도 허용** (만기/금리차에 따라)

### 기본 산식 (`ApplyForCustomer`)

`customer match 없거나 mode=add`:
```
bid = raw.bid − (swap.bid + HQ.bid + Site.bid [+ customer.bid_delta if add])
ask = raw.ask + (swap.ask + HQ.ask + Site.ask [+ customer.ask_delta if add])
```

`customer match + mode=override`:
```
bid = raw.bid − (swap.bid + customer.bid_delta)        ← HQ/Site 무시
ask = raw.ask + (swap.ask + customer.ask_delta)
```

> ⚠️ **회사별 변형**: 일부 회사는 `mode=override` 가 swap 도 무시. WTG 기본은
> swap 유지 (만기 비용은 마진 정책과 독립이라는 판단). 변경하려면 `engine.go`
> 의 `ApplyForCustomer` 의 override 분기 수정.

### Lookup 우선순위 (각 layer)

#### HQ — `lookupHQ(pair, tier, activeWindows)`
1. `(pair, tier, w)` — activeWindow 매칭 우선
2. `(pair, tier, "")` — window 미정 (모든 시간)
3. `(pair, "", w)` — tier 와일드카드 + window 매칭
4. `(pair, "", "")` — tier 와일드카드 + window 미정
5. zero (매칭 없음)

#### Site — `lookupSite(pair, channel, site, activeWindows)`
chain × window 매칭/미정:
1. `(pair, channel, site)` — exact
2. `(pair, "", site)` — channel 와일드카드
3. `(pair, channel, "")` — site 와일드카드
4. zero

#### Customer — `matchCustomerRule(customerID, pair, activeWindows)`
priority desc 정렬 후 첫 매칭:
- `rule.customer_id == customerID`
- `rule.pair == pair` 또는 `rule.pair == ""` (와일드카드)
- `rule.window == ""` 또는 `rule.window ∈ activeWindows`

> ⚠️ **회사별 변형**: WTG 는 첫 매칭으로 종료 (priority 기반). 일부 회사는
> 모든 매칭의 합산. 변경하려면 `engine.go` 의 `matchCustomerRule` 반복문 수정.

### Site margin 의 BRANCH/HQ 의미

- `Profile.Site == BRANCH` : 영업점 사용자 — HQ + Site 둘 다 적용
- `Profile.Site == HQ` : 본점 사용자 — HQ 만 적용 (Site margin 매칭 시도 후 zero 가 일반적)

> ⚠️ **회사별 변형**: 영업점 마진을 HQ 위에 추가 vs 영업점 마진이 HQ 를 대체 vs
> max(HQ, Site). WTG 기본은 **추가** (additive).

---

## 2. Cross-rate (재정통화) 합성

direct pair (USD/KRW, USD/JPY 등) 의 BEST 호가로부터 cross pair (EUR/KRW,
JPY/KRW 등) 호가를 합성. `pkg/pricing.ComputeCross` 의 worse-side 정책.

### 산식

```
bid_result = scale × contribBid(LegA, OpA) × contribBid(LegB, OpB)
ask_result = scale × contribAsk(LegA, OpA) × contribAsk(LegB, OpB)

contribBid(leg, mul) = leg.bid          (그대로, 낮은 쪽)
contribBid(leg, div) = 1 / leg.ask      (분모 ask, 역수도 낮은 쪽)
contribAsk(leg, mul) = leg.ask          (그대로, 높은 쪽)
contribAsk(leg, div) = 1 / leg.bid      (분모 bid, 역수도 높은 쪽)
```

### 예시

- `EUR/KRW = EUR/USD × USD/KRW`  → `OpA=mul, OpB=mul, scale=1`
- `100JPY/KRW = USD/KRW / USD/JPY × 100` → `OpA=mul, OpB=div, scale=100`
- `CNY/KRW = USD/KRW / USD/CNY` → `OpA=mul, OpB=div, scale=1`

### 동작 가드

| 항목 | 기본값 | 위치 |
|------|--------|------|
| Debounce | 10ms | CrossRateConsumer.DebounceWindow |
| Max staleness | 30s | CrossRateConsumer.MaxStaleness |
| Self-reentry 차단 | Source=CROSS skip | CrossRateConsumer.OnTick |
| Out-of-range extrapolation | 거절 (ErrOutOfRange) | InterpolateSwap |

> ⚠️ **회사별 변형**: worse-side 가 표준이지만 일부 회사는 mid (best_bid+best_ask)/2
> 합성. 변경하려면 `crossrate.go` 의 `contribBid/contribAsk` 수정.

---

## 3. Value-date (broken-date forward) 보간

고객이 화면에서 임의 결제일을 선택하면, 인접 standard tenor 의 swap point 를
선형 보간해 임의 결제일의 swap 산출.

### 산식 (선형)

```
offset_days = BusinessDaysBetween(SpotDate(now, spot_days), value_date)
prev = max(tenor where DefaultTenorDays[tenor] <= offset_days)
next = min(tenor where DefaultTenorDays[tenor] >  offset_days)
weight = (offset_days - prev.days) / (next.days - prev.days)
swap_bid = prev.swap_bid + (next.swap_bid - prev.swap_bid) × weight
swap_ask = prev.swap_ask + (next.swap_ask - prev.swap_ask) × weight
```

### DefaultTenorDays (SPOT 대비 영업일 offset)

| Tenor | days |
|-------|------|
| TOD | -2 |
| TOM | -1 |
| SPOT | 0 |
| 1W | 7 |
| 2W | 14 |
| 1M | 30 |
| 2M | 61 |
| 3M | 91 |
| 6M | 183 |
| 9M | 274 |
| 1Y | 365 |

> ⚠️ **회사별 변형**: 한국 외환 컨벤션상 1M 의 영업일 offset 이 31 일 수도 있고
> modified-following 조정에 따라 다름. 운영 환경에서 PricingTable 에 별도
> `tenor_calendar` 필드 추가 또는 `DefaultTenorDays` overwrite 권장.

### 가드

- 양 끝 (offset < 최소 tenor.days 또는 > 최대) → **ErrOutOfRange** (운영 안전)
- `customer_match + mode=override` 일 때도 swap 은 보간 후 적용

---

## 4. Holiday calendar

`pkg/pricing/calendar.go` 의 `Calendar` interface.

### 기본 구현

- **WeekendCalendar** : 토/일 비영업. 휴일 미반영.
- **HolidayCalendar** : 주말 + 명시 휴일 set (`PricingTable.Holidays`, "YYYY-MM-DD").

### SPOT 결제일 계산

```
SpotDate(now, spot_days=2) = AddBusinessDaysCal(startOfDay(now), 2, calendar)
```

- 휴일 등록 시 SPOT 자동 roll-forward (modified-following 단순화).

> ⚠️ **회사별 변형**: WTG 의 단일 캘린더는 1차 근사. 정확하게는 통화별 휴일 unin
> (예: USD/KRW = USD 휴일 ∪ KRW 휴일). DB CMG009M/010M/011M 가 통화별로
> 가지고 있으니 후속 단계에서 통화별 캘린더로 확장 가능.

---

## 5. TimeWindow

마진 entry 의 `window` 필드가 어느 시간대에 활성인지 정의. 시각 매칭은
`TimeWindowRule.IsActive(now, allWindows)` 가 판정.

### 정의

```json
{
  "name": "regular",
  "start": "09:00", "end": "15:30",
  "tz": "Asia/Seoul",
  "days": "MON-FRI"
}
{
  "name": "off_hours",
  "complement_of": "regular"
}
```

- `complement_of` — 다른 window 의 보집합 (시각 / 요일 조건 무시).

### 적용

- 매 tick 마다 `ActiveWindows(now)` → 활성 window 이름 목록
- Lookup chain 안에서 entry 의 `window` 필드와 매칭

> ⚠️ **회사별 변형**: WTG 는 lookup chain 안에서 window 매칭 entry 가 미정
> entry 보다 우선. 일부 회사는 더 specific 매칭 (시각 가장 가까운) 우선 — WTG
> 미지원.

---

## 6. 결정 트리 — `ApplyForCustomer` 흐름

```
입력: raw quote (pair, bid, ask, ts), profile (channel, site, tier),
       tenor, now, customer_id

1. activeWindows = ActiveWindows(now)
2. swap = lookupSwap(pair, tenor)
3. customer rule = matchCustomerRule(customer_id, pair, activeWindows)
4. 만약 customer rule 매칭 + mode == "override":
     bid = raw.bid - swap.bid - customer.bid_delta
     ask = raw.ask + swap.ask + customer.ask_delta
   아니면 (customer 미매칭 또는 mode == "add"):
     hq = lookupHQ(pair, profile.tier, activeWindows)
     site = lookupSite(pair, profile.channel, profile.site, activeWindows)
     bid = raw.bid - swap.bid - hq.bid - site.bid [- customer.bid_delta if add]
     ask = raw.ask + swap.ask + hq.ask + site.ask [+ customer.ask_delta if add]
5. CustomerQuote { pair, profile, tenor, bid, ask, raw_bid, raw_ask,
                   table_version, ts }
```

---

## 7. 회사별 변형 항목 정리

### 7.1 §1~§6 산식 자체의 분기점

이미 구현된 layer 의 **산식 옵션** — DB 데이터 X, 코드 수정 필요.

| 항목 | WTG default | 변형 옵션 | 변경 위치 |
|------|-------------|----------|-----------|
| HQ + Site 합산 | 추가 (additive) | max / replace | `engine.go` `ApplyForCustomer` |
| Customer override 가 swap 무시 | swap 유지 | swap 도 무시 | `engine.go` override 분기 |
| Customer 매칭 정책 | priority desc 첫 매칭 | 모든 매칭 합산 | `engine.go` `matchCustomerRule` |
| Cross 합성 | worse-side | mid / BBO | `crossrate.go` `contrib*` |
| Forward broken-date 보간 | 선형 | cubic spline / yield curve | `valuedate.go` `InterpolateSwap` |
| Tenor 일수 | DefaultTenorDays (ACT/365) | modified following | `valuedate.go` `DefaultTenorDays` |
| Holiday 적용 범위 | 단일 캘린더 | 통화 union | `calendar.go` (확장 필요) |
| Bid/Ask 부호 | bid 차감 / ask 가산 | (동일) | 변경 권장 X |

### 7.2 §1~§6 에 없는 회사별 비즈니스 룰 카테고리

WTG 가 **현재 지원하지 않는** 정책 패턴들. 운영 도입 결정 시 §7.3 의 hook 으로 확장.

| 카테고리 | 의도 | 입력 데이터 | 적용 시점 | WTG 확장 위치 |
|---------|------|-----------|----------|--------------|
| **A. 누적 거래량 기반 할인** | 월별 N$ 이상 거래 고객에게 마진 X% 할인 | customer_id 별 월 누적 / etcd 또는 Redis | `ApplyForCustomer` 의 customer layer | 새 layer `Volume`, customer.go 옆 `volume.go` |
| **B. 시간대별 마진 (TimeWindow 확장)** | 장외/주말/야간에 마진 가산 (변동성 보상) | 시간대 룰 (`time_windows` 이미 도입) | 모든 layer entry 의 `window` 필드 매칭 | 이미 구현 — DB CMG015/019 에 `time_window` 컬럼 추가만 필요 |
| **C. 변동성 추적 마진 (ATR)** | 최근 N분 ATR 이 임계 초과 시 마진 동적 확장 | mci-price 의 BestConsumer recent tick window | `Apply` 직전 추가 dynamic layer | 새 `volatility.go` + ring buffer 기반 ATR |
| **D. 환위험 한도 기반 자동 마진** | 회사 net position 이 한 방향 누적 시 그쪽 가격 unfavorable | 외환 시스템의 hedge 잔량 | `Apply` 결과에 ±α 가산 | 새 layer `RiskAdjust` — 외부 REST/etcd 입력 |
| **E. 통화 group 별 차등** | KRW pair / Major / 신흥국 group 별 다른 base 마진 | currency master 의 `group` 컬럼 | HQ lookup 전 group 매칭 우선 | `engine.go` `lookupHQ` 분기 추가 |
| **F. 최소 마진 floor / 최대 마진 cap** | 시장 변동성 이하로 낮아지지 않도록 / 너무 높지 않도록 | floor/cap 정책 entry | `Apply` 결과 후처리 | `engine.go` 끝부분 `clamp(minSpread, maxSpread)` |
| **G. 시장 휴장 시 마진 가산** | 주요 시장 (런던/뉴욕) 휴장 중에는 spread 확장 | session_clock + 거래소 캘린더 | TimeWindow 와 유사 | 새 `market_hours.go` |
| **H. 신규 고객 promotion 마진** | 등록 N일 이내 고객에게 마진 −α | customer 의 created_at | customer layer 의 priority 보정 | `matchCustomerRule` 의 추가 필터 |
| **I. 채널별 보너스 (모바일 vs 영업점)** | 모바일 채널 마진 −α (디지털 가입 유도) | Profile.Channel | Site margin 의 channel 별 entry 로 이미 가능 | DB 만 추가 |
| **J. 종가 (closing rate) 우선 매칭** | 일정 시각 이후 거래는 직전 종가 ±α 만 허용 | daily snapshot | `Apply` 진입 전 시각 분기 | `engine.go` 앞단 분기 — closing snapshot lookup |
| **K. 거래 통화 한도** | 통화별 일/월 한도 초과 시 가격 unfavorable + 알림 | 누적 거래량 / 한도 정책 | engine 외부 (mci-api `/v1/tx` 진입 시 거부) | **WTG 책임 X** — 매매 엔진에 위임 (`docs/auth.md`) |

> 카테고리 K 는 **WTG 가 처리하지 말 것** — 비즈니스 권한이라 매매 엔진 (`mymqd` AP) 책임.
> WTG 는 가격 산출만, 거래 거부는 엔진의 응답을 그대로 전달.

### 7.3 코드 확장 hook 위치 (plugin 화 후보)

현재 `engine.go.ApplyForCustomer` 가 모든 layer 를 inline 으로 계산. 회사별 변형이
많아지면 다음 hook 으로 분리 권장:

```go
// pkg/pricing/engine.go — 미구현, 도입 시 시그니처 제안
type CustomLayer interface {
    // 누적 마진에 contribution. ctx 는 (customer, table version, now).
    Contribute(ctx ApplyContext, current MarginAccum) MarginContribution
    Name() string  // audit / metric 라벨
}

type PricingTable struct {
    // ... 기존 필드 ...
    customLayers []CustomLayer  // 등록 순서대로 누적
}
```

도입 시 §7.2 의 A/C/D/G 가 각 `CustomLayer` 구현체로 빠짐.
각 contribution 은 `quoteid.Record` 에 별도 필드로 보존 (분쟁 추적용).

### 7.4 회사별 정책 예시 (가상)

| 회사 | 정책 조합 | 특징 |
|------|-----------|------|
| 시중은행 A | §1 + §2 + §3 + §4 + §F (cap) + §A (VIP) | 표준 + 보호 장치 + VIP 할인 |
| 증권사 B | §1 + §2 + §6 (TimeWindow) + §C (ATR) | 변동성 민감 — 장중/장외 차등 + ATR 추적 |
| 환전 fintech C | §1 + §2 + §H (promotion) + §I (모바일 보너스) | 가입 유도형 — 신규/모바일 할인 |
| 기업금융 D | §1 + §2 + §3 + §D (위험 한도) + §F (floor) | 대형 거래 + 회사 hedge 위주 |

> WTG core 는 §1~§6 의 5-layer 산식만 제공. §7.2 의 추가 layer 는 운영자가
> 회사 요구사항 확정 후 `CustomLayer` plugin 으로 추가하는 구조 권장 — core
> 코드 fork 없이 회사별 deploy 분기.

---

## 8. Audit (분쟁 추적)

매 `forward/lock` 발급 시 `quoteid.Record` 에 모든 산출 근거 보존:

- Pair / Profile / Tenor (Exact 일 때만)
- Bid / Ask / RawBid / RawAsk
- Issued / ValidUntil
- TableVersion (어느 PricingTable snapshot)
- ValueDate / OffsetDays / InterpolatedFrom / InterpolatedTo / InterpolationWeight /
  InterpolatedSwapBid / InterpolatedSwapAsk (broken-date 시)

→ `Registry.Get(quote_id)` 로 그 시점의 마진 산출 산식 정확 재현.

---

## 9. 마진 계산기 UI 와의 매핑

admin UI 의 마진 계산기 (`page-margin-calc`) 는 본 문서의 산식을 JavaScript
로 미러링. `mcInterpolateSwap` / `mcLookupHQ` / `mcLookupSite` /
`mcMatchCustomer` 함수가 위 §1~§5 와 동등 동작.

라이브 검증 (📡 서버 호출) 으로 client 계산 vs server 계산 1e-9 일치 확인.

---

## 10. 운영 시 변경 절차

1. **DB 에서 변경** (CMG015 / 018 / 019 / 021 / 038): 운영자가 외환 시스템에서
2. **fx-sync 실행**: `fx-sync --table=all` 또는 변경된 layer 만
3. **mci-price hot reload**: EtcdTableWatcher 자동 — version 증가 + Replace
4. **검증**: admin UI 의 마진 계산기 → 📡 서버 호출 → ✓ 일치 확인
5. **모니터링**: Grafana `wtg_pricing_*` rate / `wtg_master_*` size 점검

산식 자체 변경 (위 §7 의 회사별 변형 적용) 은 코드 수정 + 재배포 필요.
