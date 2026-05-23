package pricing

import "github.com/winwaysystems/wtg/pkg/session"

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
// 호출자 책임:
//   - tenor 는 보통 SPOT. 선물 거래일 경우 호출자가 알맞은 만기를 지정.
//   - PricingTable 은 immutable — Apply 도중 외부에서 수정되어선 안 됨.
func (t *PricingTable) Apply(raw Quote, profile session.Profile, tenor Tenor) CustomerQuote {
	swap := t.lookupSwap(raw.Pair, tenor)
	hq := t.lookupHQ(raw.Pair, profile.Tier)
	site := t.lookupSite(raw.Pair, profile.Channel, profile.Site)

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
