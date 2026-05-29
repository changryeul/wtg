package price

import (
	"testing"

	"github.com/gorilla/websocket"
)

// Subscriber 의 profileKey 가 설정되고 immutable 한지.
func TestSubscriber_ProfileKey(t *testing.T) {
	sub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{
		ProfileKey: "WEB.BRANCH.VIP",
	})
	if got := sub.ProfileKey(); got != "WEB.BRANCH.VIP" {
		t.Errorf("ProfileKey = %q, want WEB.BRANCH.VIP", got)
	}
}

// Registry.SendByProfile 가 매칭 subscriber 에게만 송신.
func TestRegistry_SendByProfile_MatchesOnly(t *testing.T) {
	r := NewRegistry(nil)

	vipSub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{ProfileKey: "WEB.BRANCH.VIP", SendQueueSize: 4})
	stdSub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{ProfileKey: "WEB.BRANCH.STD", SendQueueSize: 4})
	bareSub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4}) // profileKey="" — quote 미수신
	r.Add(vipSub)
	r.Add(stdSub)
	r.Add(bareSub)

	// pair="" → pair 필터 없이 profile 만 검증 (기존 의도 유지)
	sent, dropped := r.SendByProfile("WEB.BRANCH.VIP", "", []byte("vip-payload"))
	if sent != 1 || dropped != 0 {
		t.Errorf("sent=%d dropped=%d, want 1/0", sent, dropped)
	}

	// VIP send queue 에 들어있어야 함.
	select {
	case got := <-vipSub.send:
		if string(got) != "vip-payload" {
			t.Errorf("vip 수신 payload = %q", got)
		}
	default:
		t.Error("VIP subscriber 가 payload 수신 안 함")
	}

	// STD, bare 는 받지 말아야.
	select {
	case got := <-stdSub.send:
		t.Errorf("STD 가 잘못 수신: %q", got)
	default:
	}
	select {
	case got := <-bareSub.send:
		t.Errorf("bare(profile-less) 가 잘못 수신: %q", got)
	default:
	}
}

// SendByProfile 에 빈 profileKey 를 전달하면 아무도 안 받음.
func TestRegistry_SendByProfile_EmptyKey(t *testing.T) {
	r := NewRegistry(nil)
	sub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{ProfileKey: "WEB.BRANCH.VIP", SendQueueSize: 4})
	r.Add(sub)

	sent, _ := r.SendByProfile("", "", []byte("x"))
	if sent != 0 {
		t.Errorf("empty key 인데 sent=%d", sent)
	}
}

// 일반 Broadcast 는 profileKey 무관하게 전체 송신.
func TestRegistry_Broadcast_IgnoresProfile(t *testing.T) {
	r := NewRegistry(nil)
	a := NewSubscriber(&websocket.Conn{}, SubscriberOptions{ProfileKey: "WEB.BRANCH.VIP", SendQueueSize: 4})
	b := NewSubscriber(&websocket.Conn{}, SubscriberOptions{ProfileKey: "MOB.HQ.STD", SendQueueSize: 4})
	c := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4}) // bare
	r.Add(a)
	r.Add(b)
	r.Add(c)

	sent, _ := r.Broadcast([]byte("hello"))
	if sent != 3 {
		t.Errorf("Broadcast sent=%d, want 3", sent)
	}
}
