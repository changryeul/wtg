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
// Phase 2 — customer margin 은 미통합. Phase 3 에서 ApplyAt 의 customerID
// 인자 추가 예정.
func (t *PricingTable) ApplyAt(raw Quote, profile session.Profile, tenor Tenor, now time.Time) CustomerQuote {
	if now.IsZero() {
		now = time.Now()
	}
	activeWindows := t.ActiveWindows(now)
	swap := t.lookupSwap(raw.Pair, tenor)
	hq := t.lookupHQ(raw.Pair, profile.Tier, activeWindows)
	site := t.lookupSite(raw.Pair, profile.Channel, profile.Site, activeWindows)

	totalBid := swap.BidAmount + hq.BidAmount + site.BidAmount
	totalAsk := swap.AskAmount + hq.AskAmount + site.AskAmount

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
