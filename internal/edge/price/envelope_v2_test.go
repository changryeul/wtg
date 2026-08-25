package price

import (
	"encoding/json"
	"testing"

	"github.com/gorilla/websocket"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// v2 envelope 구조 — 안정 헤더(ev/type/asset_class/symbol/ts_unix_nano) + 자산별 data.
func TestEncodeCustomerQuoteV2(t *testing.T) {
	cq := &wtgpb.CustomerQuote{
		Pair:         "USD/KRW",
		Channel:      "WEB",
		Site:         "HQ",
		Tier:         "VIP",
		Tenor:        "SPOT",
		Bid:          1330.20,
		Ask:          1330.80,
		BidSize:      1000000,
		AskSize:      1500000,
		RawBid:       1330.40,
		RawAsk:       1330.60,
		TsUnixNano:   1699999999000,
		TableVersion: 42,
	}
	b, err := encodeCustomerQuoteV2(cq)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		EV         int    `json:"ev"`
		Type       string `json:"type"`
		AssetClass string `json:"asset_class"`
		Symbol     string `json:"symbol"`
		TSUnixNano int64  `json:"ts_unix_nano"`
		Data       struct {
			Bid          float64 `json:"bid"`
			Ask          float64 `json:"ask"`
			BidSize      float64 `json:"bid_size"`
			AskSize      float64 `json:"ask_size"`
			RawBid       float64 `json:"raw_bid"`
			Tenor        string  `json:"tenor"`
			Channel      string  `json:"chan"`
			TableVersion int64   `json:"v"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if env.EV != 2 || env.Type != "fx.quote" || env.AssetClass != "FX" {
		t.Errorf("헤더 판별자 오류: ev=%d type=%q ac=%q", env.EV, env.Type, env.AssetClass)
	}
	if env.Symbol != "USD/KRW" || env.TSUnixNano != 1699999999000 {
		t.Errorf("헤더 심볼/시각 오류: symbol=%q ts=%d", env.Symbol, env.TSUnixNano)
	}
	if env.Data.Bid != 1330.20 || env.Data.Ask != 1330.80 || env.Data.RawBid != 1330.40 {
		t.Errorf("data 마진가/원시값 오류: %+v", env.Data)
	}
	if env.Data.BidSize != 1000000 || env.Data.AskSize != 1500000 {
		t.Errorf("avail 미노출: bid_size=%v ask_size=%v", env.Data.BidSize, env.Data.AskSize)
	}
	if env.Data.Tenor != "SPOT" || env.Data.Channel != "WEB" || env.Data.TableVersion != 42 {
		t.Errorf("data profile/가격표버전 오류: %+v", env.Data)
	}
	// v2 는 최상위에 pair/bid 를 두지 않는다 (data 안으로 이동).
	var flat map[string]json.RawMessage
	_ = json.Unmarshal(b, &flat)
	if _, ok := flat["pair"]; ok {
		t.Error("v2 최상위에 pair 가 있으면 안 됨 (data.symbol 로 이동)")
	}
	if _, ok := flat["bid"]; ok {
		t.Error("v2 최상위에 bid 가 있으면 안 됨 (data.bid 로 이동)")
	}
}

// SendVersioned — ev 에 따라 v1/v2 선택. ev>=2 & v2!=nil → v2, 그 외 → v1.
func TestSubscriber_SendVersioned(t *testing.T) {
	cases := []struct {
		ev       int
		v2       []byte
		wantV1   bool // true 면 v1 payload 를 받아야
		wantPayl string
	}{
		{ev: 0, v2: []byte("V2"), wantV1: true, wantPayl: "V1"},  // legacy
		{ev: 1, v2: []byte("V2"), wantV1: true, wantPayl: "V1"},  // 명시 v1
		{ev: 2, v2: []byte("V2"), wantV1: false, wantPayl: "V2"}, // v2
		{ev: 2, v2: nil, wantV1: true, wantPayl: "V1"},           // v2 인코딩 실패 → 폴백
	}
	for _, c := range cases {
		sub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{
			EnvelopeVersion: c.ev, SendQueueSize: 2,
		})
		if err := sub.SendVersioned([]byte("V1"), c.v2); err != nil {
			t.Fatalf("ev=%d SendVersioned: %v", c.ev, err)
		}
		got := string(<-sub.send)
		if got != c.wantPayl {
			t.Errorf("ev=%d: 수신 %q, want %q", c.ev, got, c.wantPayl)
		}
	}
}

// SendByProfileV — 같은 profile 의 두 subscriber(ev=1, ev=2)가 각자 다른 버전 수신.
func TestRegistry_SendByProfileV_PerConnectionVersion(t *testing.T) {
	r := NewRegistry(nil)
	legacySub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{
		ProfileKey: "WEB.HQ.VIP", EnvelopeVersion: 0, SendQueueSize: 2,
	})
	v2Sub := NewSubscriber(&websocket.Conn{}, SubscriberOptions{
		ProfileKey: "WEB.HQ.VIP", EnvelopeVersion: 2, SendQueueSize: 2,
	})
	r.Add(legacySub)
	r.Add(v2Sub)

	sent, dropped := r.SendByProfileV("WEB.HQ.VIP", "", []byte("V1"), []byte("V2"))
	if sent != 2 || dropped != 0 {
		t.Fatalf("sent=%d dropped=%d, want 2/0", sent, dropped)
	}
	if got := string(<-legacySub.send); got != "V1" {
		t.Errorf("legacy sub 수신 %q, want V1", got)
	}
	if got := string(<-v2Sub.send); got != "V2" {
		t.Errorf("v2 sub 수신 %q, want V2", got)
	}
}

// controlRequest.items — symbols(통일) + pairs(legacy alias) 합집합, 중복 제거.
func TestControlRequest_Items(t *testing.T) {
	cases := []struct {
		name string
		req  controlRequest
		want []string
	}{
		{"symbols only", controlRequest{Symbols: []string{"USD/KRW", "101V6000"}}, []string{"USD/KRW", "101V6000"}},
		{"pairs only (legacy)", controlRequest{Pairs: []string{"EUR/USD"}}, []string{"EUR/USD"}},
		{"both union dedup", controlRequest{Symbols: []string{"USD/KRW"}, Pairs: []string{"USD/KRW", "EUR/USD"}}, []string{"USD/KRW", "EUR/USD"}},
		{"empty", controlRequest{}, nil},
	}
	for _, c := range cases {
		got := c.req.items()
		if len(got) != len(c.want) {
			t.Errorf("%s: items()=%v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: items()[%d]=%q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}
