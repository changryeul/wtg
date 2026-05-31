package session

import "testing"

func TestParseProfileKey(t *testing.T) {
	tests := []struct {
		key     string
		want    Profile
		wantErr bool
	}{
		{"WEB.BRANCH.VIP", Profile{ChannelWeb, SiteBranch, TierVIP}, false},
		{"MOB.HQ.STD", Profile{ChannelMobile, SiteHQ, TierStandard}, false},
		{"..", Profile{}, false}, // 빈 token 도 enum 검증 X (호출자 책임)
		{"WEB.BRANCH", Profile{}, true},
		{"WEB.BRANCH.VIP.EXTRA", Profile{}, true},
		{"", Profile{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			got, err := ParseProfileKey(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Errorf("err 기대했지만 nil: %+v", got)
				}
				return
			}
			if err != nil {
				t.Errorf("err = %v, want nil", err)
				return
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Round-trip: Profile.Key() ↔ ParseProfileKey.
func TestProfileKey_RoundTrip(t *testing.T) {
	p := Profile{ChannelWeb, SiteBranch, TierVIP}
	parsed, err := ParseProfileKey(p.Key())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != p {
		t.Errorf("round-trip: %+v vs %+v", parsed, p)
	}
}

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
