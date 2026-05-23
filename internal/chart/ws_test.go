package chart

import (
	"testing"

	"github.com/gorilla/websocket"
)

func TestSubscriber_FiltersMatch(t *testing.T) {
	sub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{})
	// 빈 필터 — 모두 매칭.
	if !sub.Matches("USD/KRW", "1m") {
		t.Error("빈 필터에서 매칭 실패")
	}

	// pair 필터.
	sub.SetFilters([]string{"USD/KRW"}, nil)
	if !sub.Matches("USD/KRW", "1m") {
		t.Error("USD/KRW 매칭 실패")
	}
	if sub.Matches("EUR/KRW", "1m") {
		t.Error("EUR/KRW 가 잘못 매칭")
	}

	// tf 필터 추가.
	sub.SetFilters([]string{"USD/KRW"}, []string{"5m"})
	if sub.Matches("USD/KRW", "1m") {
		t.Error("1m 이 5m 필터에서 매칭됨")
	}
	if !sub.Matches("USD/KRW", "5m") {
		t.Error("USD/KRW 5m 매칭 실패")
	}

	// 필터 해제.
	sub.SetFilters(nil, nil)
	if !sub.Matches("XAU/USD", "1d") {
		t.Error("필터 해제 후 매칭 실패")
	}
}

func TestHub_PublishMatchesOnly(t *testing.T) {
	hub := NewHub(nil)
	a := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4})
	a.SetFilters([]string{"USD/KRW"}, []string{"1m"})
	b := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4})
	b.SetFilters([]string{"EUR/KRW"}, nil)
	c := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4})
	// no filter — c 는 모든 publish 받음.
	hub.Add(a)
	hub.Add(b)
	hub.Add(c)

	sent, _ := hub.Publish("USD/KRW", "1m", []byte("usd-1m"))
	if sent != 2 {
		t.Errorf("USD/KRW 1m sent = %d, want 2 (a + c)", sent)
	}
	// a 가 받았는지 확인.
	select {
	case got := <-a.send:
		if string(got) != "usd-1m" {
			t.Errorf("a payload mismatch: %q", got)
		}
	default:
		t.Error("a 가 안 받음")
	}
	// b 는 안 받음.
	select {
	case got := <-b.send:
		t.Errorf("b 가 잘못 받음: %q", got)
	default:
	}
}

func TestHub_PublishDifferentTF(t *testing.T) {
	hub := NewHub(nil)
	one := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4})
	one.SetFilters(nil, []string{"1m"})
	five := NewSubscriber(&websocket.Conn{}, SubscriberOptions{SendQueueSize: 4})
	five.SetFilters(nil, []string{"5m"})
	hub.Add(one)
	hub.Add(five)

	sent5m, _ := hub.Publish("USD/KRW", "5m", []byte("5m"))
	if sent5m != 1 {
		t.Errorf("5m sent = %d, want 1", sent5m)
	}
	sent1m, _ := hub.Publish("USD/KRW", "1m", []byte("1m"))
	if sent1m != 1 {
		t.Errorf("1m sent = %d, want 1", sent1m)
	}
}
