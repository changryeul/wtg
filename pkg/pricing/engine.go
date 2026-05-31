package pricing

import (
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Apply 는 raw quote 에 PricingTable 의 마진을 적용해 CustomerQuote 를 반환한다.
//
// 산식:
//
//	bid = raw.Bid - (swap.BidAmount + hq.BidAmount + site.BidAmount)
//	ask = raw.Ask + (swap.AskAmount + hq.AskAmount + site.AskAmount)
//
// Bid 는 차감, Ask 는 가산 (스프레드 확대 = 고객 불리) 방향이 표준.
// 스왑포인트는 음수도 허용 — 만기·금리차에 따라 부호 반전 가능하므로 그대로 합산.
//
// Time window 미사용 환경 (TimeWindows 비어있는 PricingTable) 에서는 기존 동작과
// 완전 동일. backward compat 보장.
//
// 호출자 책임:
//   - tenor 는 보통 SPOT. 선물 거래일 경우 호출자가 알맞은 만기를 지정.
//   - PricingTable 은 immutable — Apply 도중 외부에서 수정되어선 안 됨.
func (t *PricingTable) Apply(raw Quote, profile session.Profile, tenor Tenor) CustomerQuote {
	return t.ApplyAt(raw, profile, tenor, time.Now())
}

// ApplyAt — Apply 의 explicit-time 변형. 분쟁 / 마진 재계산 / 봉 시각의 정확한
// time window 매칭이 필요할 때 사용. now 가 zero 면 time.Now() 로 fallback.
//
// 흐름:
//   1. ActiveWindows(now) — 현재 시각 활성 window 목록
//   2. lookupHQ / lookupSite 가 window 매칭 우선 + window="" fallback
//   3. 누계 + raw 적용
//
// customer margin 미적용 — customer ID 가 있는 경로는 ApplyForCustomer 사용.
func (t *PricingTable) ApplyAt(raw Quote, profile session.Profile, tenor Tenor, now time.Time) CustomerQuote {
	return t.ApplyForCustomer(raw, profile, tenor, now, "")
}

// ApplyForCustomer — Phase 3. customer-specific margin 까지 적용한 quote 산출.
//
// customerID == "" 면 ApplyAt 과 동일 (HQ + Site + Swap).
//
// customerID 매칭 시 다음 산식:
//
//	add 모드:    bid -= swap + HQ + Site + customer.BidDelta
//	             ask += swap + HQ + Site + customer.AskDelta
//	override:    bid -= swap + customer.BidDelta             (HQ/Site 무시)
//	             ask += swap + customer.AskDelta
//
// 매칭 규칙 (priority desc 순회, 첫 매칭):
//   - rule.CustomerID == customerID
//   - rule.Pair == raw.Pair  또는  rule.Pair == "" (와일드카드)
//   - rule.Window == ""  또는  rule.Window ∈ activeWindows
//
// swap 은 override 에서도 항상 적용 — 만기 비용은 마진과 독립.
func (t *PricingTable) ApplyForCustomer(raw Quote, profile session.Profile, tenor Tenor, now time.Time, customerID string) CustomerQuote {
	if now.IsZero() {
		now = time.Now()
	}
	activeWindows := t.ActiveWindows(now)
	swap := t.lookupSwap(raw.Pair, tenor)

	// customer 매칭 시도 (있을 때만).
	rule, ok := t.matchCustomerRule(customerID, raw.Pair, activeWindows)

	var totalBid, totalAsk float64
	if ok && rule.Mode == "override" {
		// HQ/Site 무시. swap + customer 만.
		totalBid = swap.BidAmount + rule.BidDelta
		totalAsk = swap.AskAmount + rule.AskDelta
	} else {
		// add 모드 또는 customer 미매칭 — HQ/Site 누계.
		hq := t.lookupHQ(raw.Pair, profile.Tier, activeWindows)
		site := t.lookupSite(raw.Pair, profile.Channel, profile.Site, activeWindows)
		totalBid = swap.BidAmount + hq.BidAmount + site.BidAmount
		totalAsk = swap.AskAmount + hq.AskAmount + site.AskAmount
		if ok { // mode=add
			totalBid += rule.BidDelta
			totalAsk += rule.AskDelta
		}
	}

	return CustomerQuote{
		Pair:         raw.Pair,
		Profile:      profile,
		Tenor:        tenor,
		Bid:          raw.Bid - totalBid,
		Ask:          raw.Ask + totalAsk,
		TS:           raw.TS,
		RawBid:       raw.Bid,
		RawAsk:       raw.Ask,
		TableVersion: t.Version,
	}
}

// ApplyForValueDate — P5 5단계. 결제일 (value_date) 기반 broken-date 마진 적용.
//
// 흐름:
//   1. SpotDate(now, spotDays) → 거래일 기준 SPOT 결제일.
//   2. BusinessDaysBetween(spot, valueDate) → offsetDays (weekend-only).
//   3. InterpolateSwap(pair, offsetDays) → SwapInterpolation (선형 보간).
//      - offsetDays 가 standard tenor 와 정확 일치 → Exact (보간 X, lookupSwap 동등).
//      - 그 사이 → 선형 보간된 swap.
//      - 양 끝 → ErrOutOfRange.
//      - 본 pair 의 swap_point 미등록 → ErrNoSwap.
//   4. HQ/Site/Customer 는 ApplyForCustomer 와 동일 (보간 swap 만 lookupSwap 대체).
//
// CustomerQuote.Tenor 는 broken-date 시 빈값 — interpolation 정보는 별도 반환.
// 호출자가 quoteid.Record 에 SwapInterpolation 의 필드를 보존 (감사 추적).
func (t *PricingTable) ApplyForValueDate(raw Quote, profile session.Profile,
	valueDate time.Time, now time.Time, spotDays int, customerID string,
) (CustomerQuote, SwapInterpolation, error) {

	if now.IsZero() {
		now = time.Now()
	}
	cal := t.Cal()
	spotDate := SpotDateCal(now, spotDays, cal)
	offsetDays := BusinessDaysBetweenCal(spotDate, valueDate, cal)
	interp, err := t.InterpolateSwap(raw.Pair, offsetDays)
	if err != nil {
		return CustomerQuote{}, SwapInterpolation{OffsetDays: offsetDays}, err
	}

	activeWindows := t.ActiveWindows(now)
	rule, ok := t.matchCustomerRule(customerID, raw.Pair, activeWindows)

	var totalBid, totalAsk float64
	if ok && rule.Mode == "override" {
		totalBid = interp.Margin.BidAmount + rule.BidDelta
		totalAsk = interp.Margin.AskAmount + rule.AskDelta
	} else {
		hq := t.lookupHQ(raw.Pair, profile.Tier, activeWindows)
		site := t.lookupSite(raw.Pair, profile.Channel, profile.Site, activeWindows)
		totalBid = interp.Margin.BidAmount + hq.BidAmount + site.BidAmount
		totalAsk = interp.Margin.AskAmount + hq.AskAmount + site.AskAmount
		if ok {
			totalBid += rule.BidDelta
			totalAsk += rule.AskDelta
		}
	}

	// CustomerQuote.Tenor — Exact 일 때는 매칭 tenor, 아닐 때 빈값.
	// Record 에 SwapInterpolation 의 모든 필드가 보존되므로 빈값이어도 OK.
	tenor := Tenor("")
	if interp.Exact {
		tenor = interp.From
	}

	return CustomerQuote{
		Pair:         raw.Pair,
		Profile:      profile,
		Tenor:        tenor,
		Bid:          raw.Bid - totalBid,
		Ask:          raw.Ask + totalAsk,
		TS:           raw.TS,
		RawBid:       raw.Bid,
		RawAsk:       raw.Ask,
		TableVersion: t.Version,
	}, interp, nil
}

// matchCustomerRule — priority desc 정렬된 CustomerMargin 순회하며 첫 매칭 반환.
// 매칭 없으면 zero rule + false.
//
// 매칭 조건:
//   - rule.CustomerID == customerID
//   - rule.Pair == pair  또는  rule.Pair == ""
//   - rule.Window == ""  또는  rule.Window ∈ activeWindows
//
// customerID 가 "" 이면 무조건 false — customer-anonymous 경로 (ApplyAt) 보호.
func (t *PricingTable) matchCustomerRule(customerID string, pair session.Pair, activeWindows []string) (CustomerRule, bool) {
	if customerID == "" {
		return CustomerRule{}, false
	}
	for _, r := range t.CustomerMargin {
		if r.CustomerID != customerID {
			continue
		}
		if r.Pair != "" && r.Pair != pair {
			continue
		}
		if r.Window != "" {
			matched := false
			for _, w := range activeWindows {
				if w == r.Window {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		return r, true
	}
	return CustomerRule{}, false
}
