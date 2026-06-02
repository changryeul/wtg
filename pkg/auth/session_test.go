package auth

import (
	"sync"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestSession_SubscribeUnsubscribe(t *testing.T) {
	s := &Session{}

	if s.IsSubscribed("USD/KRW") {
		t.Error("초기 상태에서 IsSubscribed 가 true 여서는 안 됨")
	}

	if !s.Subscribe("USD/KRW") {
		t.Error("최초 Subscribe 는 added=true 여야 함")
	}
	if s.Subscribe("USD/KRW") {
		t.Error("중복 Subscribe 는 added=false 여야 함")
	}
	if !s.IsSubscribed("USD/KRW") {
		t.Error("Subscribe 후 IsSubscribed=true 여야 함")
	}

	if !s.Unsubscribe("USD/KRW") {
		t.Error("존재하는 항목 Unsubscribe 는 removed=true 여야 함")
	}
	if s.Unsubscribe("USD/KRW") {
		t.Error("이미 제거된 항목 Unsubscribe 는 removed=false 여야 함")
	}
	if s.IsSubscribed("USD/KRW") {
		t.Error("Unsubscribe 후 IsSubscribed=false 여야 함")
	}
}

func TestSession_SubscriptionsSnapshot(t *testing.T) {
	s := &Session{}
	s.Subscribe("USD/KRW")
	s.Subscribe("EUR/KRW")
	s.Subscribe("JPY/KRW")

	got := s.Subscriptions()
	if len(got) != 3 {
		t.Fatalf("Subscriptions len = %d, want 3", len(got))
	}
	set := map[session.Pair]bool{}
	for _, p := range got {
		set[p] = true
	}
	for _, want := range []session.Pair{"USD/KRW", "EUR/KRW", "JPY/KRW"} {
		if !set[want] {
			t.Errorf("missing %q in snapshot", want)
		}
	}

	// snapshot 이 내부 상태와 분리되어 있는지 확인 (호출자 수정 → 내부 영향 없음)
	_ = append(got, "BOGUS")
	if s.IsSubscribed("BOGUS") {
		t.Error("snapshot 수정이 세션 상태에 영향을 줘서는 안 됨")
	}
}

func TestSession_ClearSubscriptions(t *testing.T) {
	s := &Session{}
	s.Subscribe("USD/KRW")
	s.Subscribe("EUR/KRW")

	s.ClearSubscriptions()

	if len(s.Subscriptions()) != 0 {
		t.Error("Clear 후 Subscriptions 가 비어있어야 함")
	}
	// Clear 후에도 재구독 가능
	if !s.Subscribe("USD/KRW") {
		t.Error("Clear 후 재구독 가능해야 함")
	}
}

// 동시 Subscribe/Unsubscribe 가 race 없이 동작하는지 (go test -race).
func TestSession_ConcurrentSubscribe(t *testing.T) {
	s := &Session{}
	pairs := []session.Pair{"USD/KRW", "EUR/KRW", "JPY/KRW", "GBP/KRW"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p := pairs[j%len(pairs)]
				s.Subscribe(p)
				_ = s.IsSubscribed(p)
				s.Unsubscribe(p)
				_ = s.Subscriptions()
			}
		}()
	}
	wg.Wait()
}

func TestSession_ProfileImmutable(t *testing.T) {
	// Profile 은 immutable value object — Session 에 박힌 후 비교 가능해야 함.
	p := session.Profile{
		Channel: session.ChannelWeb,
		Site:    session.SiteBranch,
		Tier:    session.TierVIP,
	}
	s := &Session{Profile: p}

	if s.Profile != p {
		t.Errorf("Profile mismatch: got %+v, want %+v", s.Profile, p)
	}
	if got := s.Profile.Key(); got != "WEB.BRANCH.VIP" {
		t.Errorf("Profile.Key = %q, want WEB.BRANCH.VIP", got)
	}
}
