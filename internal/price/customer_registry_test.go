package price

import (
	"fmt"
	"sync"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

// CustomerRegistry — Phase 4a 의 기본 동작 검증.

func TestCustomerRegistry_RegisterAndCount(t *testing.T) {
	r := NewCustomerRegistry()
	if r.Count() != 0 {
		t.Fatalf("init count = %d, want 0", r.Count())
	}
	r.Register("VIP-7", session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP})
	r.Register("GOLD-3", session.Profile{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierStandard})
	if r.Count() != 2 {
		t.Errorf("after 2 register: count = %d, want 2", r.Count())
	}
	// 같은 ID 재등록은 count 증가 X.
	r.Register("VIP-7", session.Profile{Tier: session.TierStandard})
	if r.Count() != 2 {
		t.Errorf("re-register: count = %d, want 2", r.Count())
	}
}

func TestCustomerRegistry_Unregister(t *testing.T) {
	r := NewCustomerRegistry()
	r.Register("A", session.Profile{Tier: session.TierVIP})
	r.Register("B", session.Profile{Tier: session.TierVIP})
	r.Unregister("A")
	if r.Count() != 1 {
		t.Errorf("after unregister A: count = %d, want 1", r.Count())
	}
	// 미등록 ID 는 no-op.
	r.Unregister("ZZZ")
	if r.Count() != 1 {
		t.Errorf("after unregister unknown: count = %d, want 1", r.Count())
	}
}

func TestCustomerRegistry_Snapshot(t *testing.T) {
	r := NewCustomerRegistry()
	r.Register("A", session.Profile{Tier: session.TierVIP, Channel: session.ChannelWeb})
	r.Register("B", session.Profile{Tier: session.TierStandard, Channel: session.ChannelMobile})

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	// 순서 무관.
	ids := map[string]session.Profile{}
	for _, e := range snap {
		ids[e.CustomerID] = e.Profile
	}
	if ids["A"].Tier != session.TierVIP || ids["A"].Channel != session.ChannelWeb {
		t.Errorf("A profile: %+v", ids["A"])
	}
	if ids["B"].Tier != session.TierStandard || ids["B"].Channel != session.ChannelMobile {
		t.Errorf("B profile: %+v", ids["B"])
	}
}

func TestCustomerRegistry_RegisterUpdatesProfile(t *testing.T) {
	r := NewCustomerRegistry()
	r.Register("A", session.Profile{Tier: session.TierVIP})
	r.Register("A", session.Profile{Tier: session.TierStandard}) // 갱신
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("after update: count = %d, want 1", len(snap))
	}
	if snap[0].Profile.Tier != session.TierStandard {
		t.Errorf("after update: Tier = %s, want STANDARD", snap[0].Profile.Tier)
	}
}

func TestCustomerRegistry_EmptyIDIgnored(t *testing.T) {
	r := NewCustomerRegistry()
	r.Register("", session.Profile{Tier: session.TierVIP})
	r.Unregister("")
	if r.Count() != 0 {
		t.Errorf("empty ID: count = %d, want 0", r.Count())
	}
}

// 동시 등록/해제 race test — go test -race 에서 의미.
func TestCustomerRegistry_ConcurrentAccess(t *testing.T) {
	r := NewCustomerRegistry()
	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("CUST-%d", i)
			r.Register(id, session.Profile{Tier: session.TierVIP})
			_ = r.Count()
			_ = r.Snapshot()
			r.Unregister(id)
		}(i)
	}
	wg.Wait()
	if r.Count() != 0 {
		t.Errorf("after concurrent reg/unreg: count = %d, want 0", r.Count())
	}
}
