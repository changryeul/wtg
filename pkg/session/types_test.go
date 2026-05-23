package session

import "testing"

func TestProfile_Key(t *testing.T) {
	tests := []struct {
		name string
		p    Profile
		want string
	}{
		{"web branch vip", Profile{ChannelWeb, SiteBranch, TierVIP}, "WEB.BRANCH.VIP"},
		{"mobile hq std", Profile{ChannelMobile, SiteHQ, TierStandard}, "MOB.HQ.STD"},
		{"empty", Profile{}, ".."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Key(); got != tc.want {
				t.Errorf("Key() = %q, want %q", got, tc.want)
			}
		})
	}
}
