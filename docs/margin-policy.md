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

| 항목 | WTG default | 변형 옵션 | 변경 위치 |
|------|-------------|----------|-----------|
| HQ + Site 합산 | 추가 (additive) | max / replace | engine.go ApplyForCustomer |
| Customer override 가 swap 무시 | swap 유지 | swap 도 무시 | engine.go override 분기 |
| Customer 매칭 정책 | priority desc 첫 매칭 | 모든 매칭 합산 | engine.go matchCustomerRule |
| Cross 합성 | worse-side | mid / BBO | crossrate.go contrib* |
| Forward broken-date 보간 | 선형 | cubic spline / yield curve | valuedate.go InterpolateSwap |
| Tenor 일수 | DefaultTenorDays (ACT/365) | modified following | valuedate.go DefaultTenorDays |
| Holiday 적용 범위 | 단일 캘린더 | 통화 union | calendar.go (확장 필요) |
| Bid/Ask 부호 | bid 차감 / ask 가산 | (동일) | 변경 권장 X |

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
