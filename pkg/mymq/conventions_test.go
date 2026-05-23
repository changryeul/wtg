package mymq

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

// RKeyQuote 결과가 모든 Profile 조합에서 LRkey(16) 안에 들어가는지 보장한다.
// 향후 session.Channel / Site / Tier 에 enum 값을 추가할 때 회귀 방지용.
func TestRKeyQuote_FitsLRkey(t *testing.T) {
	channels := []session.Channel{
		session.ChannelWeb,
		session.ChannelMobile,
		session.ChannelCS,
		session.ChannelFIX,
		session.ChannelAdmin,
	}
	sites := []session.Site{session.SiteBranch, session.SiteHQ}
	tiers := []session.Tier{session.TierVIP, session.TierGold, session.TierStandard}

	for _, c := range channels {
		for _, s := range sites {
			for _, ti := range tiers {
				p := session.Profile{Channel: c, Site: s, Tier: ti}
				key := RKeyQuote(p.Key())
				if len(key) > LRkey {
					t.Errorf("Profile %+v 의 routing-key %q (%d bytes) 가 LRkey(%d) 초과",
						p, key, len(key), LRkey)
				}
				if key == "" {
					t.Errorf("Profile %+v 가 빈 routing-key 를 생성", p)
				}
			}
		}
	}
}

// RKeyQuote 는 Profile.Key() 결과를 그대로 사용해야 한다 (identity).
func TestRKeyQuote_Identity(t *testing.T) {
	p := session.Profile{
		Channel: session.ChannelWeb,
		Site:    session.SiteBranch,
		Tier:    session.TierVIP,
	}
	want := "WEB.BRANCH.VIP"
	if got := RKeyQuote(p.Key()); got != want {
		t.Errorf("RKeyQuote = %q, want %q", got, want)
	}
}

// ExchangeQuote 는 LXchg(8) 안에 들어가야 한다.
func TestExchangeQuote_FitsLXchg(t *testing.T) {
	if len(ExchangeQuote) > LXchg {
		t.Errorf("ExchangeQuote %q (%d bytes) 가 LXchg(%d) 초과", ExchangeQuote, len(ExchangeQuote), LXchg)
	}
}
