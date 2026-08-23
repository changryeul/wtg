package price

import (
	"encoding/json"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/winwaysystems/wtg/pkg/instrument"
	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// encodeKrxVariants — v1=payload(legacy flat), v2=폴리모픽 wrap.
func TestEncodeKrxVariants(t *testing.T) {
	payload := []byte(`{"kind":"fut.book","code":"101V6000","ask":[{"prc":405.1}]}`)
	ev := &wtgpb.KrxEvent{
		Type: "krx.fut.book", AssetClass: "FUTURE", Symbol: "101V6000",
		TsUnixNano: 1699999999000, Payload: payload,
	}
	v1, v2 := encodeKrxVariants(ev)
	if string(v1) != string(payload) {
		t.Errorf("v1 은 payload 그대로여야: %s", v1)
	}
	var env struct {
		EV         int             `json:"ev"`
		Type       string          `json:"type"`
		AssetClass string          `json:"asset_class"`
		Symbol     string          `json:"symbol"`
		TSUnixNano int64           `json:"ts_unix_nano"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(v2, &env); err != nil {
		t.Fatal(err)
	}
	if env.EV != 2 || env.Type != "krx.fut.book" || env.AssetClass != "FUTURE" || env.Symbol != "101V6000" || env.TSUnixNano != 1699999999000 {
		t.Errorf("v2 헤더 오류: %+v", env)
	}
	var data map[string]interface{}
	_ = json.Unmarshal(env.Data, &data)
	if data["kind"] != "fut.book" || data["code"] != "101V6000" {
		t.Errorf("v2 data 원본 미보존: %s", env.Data)
	}
}

// gateSubscribe — 카탈로그 active 심볼은 FX pairValidator 를 우회해 허용.
func TestGateSubscribe_CatalogAccepts(t *testing.T) {
	cat := instrument.NewCatalog()
	cat.Replace([]instrument.Instrument{
		{Symbol: "101V6000", AssetClass: instrument.AssetFuture, Upstream: instrument.UpstreamKRX, Active: true},
		{Symbol: "DEAD", AssetClass: instrument.AssetFuture, Upstream: instrument.UpstreamKRX, Active: false},
	})
	// pairValidator 는 USD/KRW 만 허용 — KRX 심볼은 모름(거부 대상).
	pv := NewMemoryPairValidator()
	pv.Add("USD/KRW")
	s := &Server{catalog: cat, pairValidator: pv}

	acc, rej := s.gateSubscribe(nil, []string{"101V6000", "USD/KRW", "DEAD", "NOPE"})
	accSet := map[string]bool{}
	for _, a := range acc {
		accSet[a] = true
	}
	if !accSet["101V6000"] {
		t.Error("카탈로그 active KRX 심볼이 허용 안 됨")
	}
	if !accSet["USD/KRW"] {
		t.Error("FX pairValidator 허용 심볼이 거부됨")
	}
	// DEAD(비활성) + NOPE(미등록) → 카탈로그 miss → pairValidator 도 거부 → rejected.
	rejSet := map[string]bool{}
	for _, r := range rej {
		rejSet[r] = true
	}
	if !rejSet["DEAD"] || !rejSet["NOPE"] {
		t.Errorf("비활성/미등록이 거부되지 않음: rej=%v", rej)
	}
}

// BroadcastBySymbolV — symbol 매칭 구독자에게 version-aware 송신 (profile 무관).
func TestRegistry_BroadcastBySymbolV(t *testing.T) {
	r := NewRegistry(nil)
	// 101V6000 구독 (ev=2), 다른 종목 구독(ev=0), all 모드(ev=2).
	subV2 := NewSubscriber(&websocket.Conn{}, SubscriberOptions{EnvelopeVersion: 2, SendQueueSize: 2})
	subV2.SubscribePairs([]string{"101V6000"})
	subOther := NewSubscriber(&websocket.Conn{}, SubscriberOptions{EnvelopeVersion: 0, SendQueueSize: 2})
	subOther.SubscribePairs([]string{"105V3000"})
	r.Add(subV2)
	r.Add(subOther)

	sent, _ := r.BroadcastBySymbolV("101V6000", []byte("V1"), []byte("V2"))
	if sent != 1 {
		t.Fatalf("sent=%d want 1 (101V6000 구독자만)", sent)
	}
	if got := string(<-subV2.send); got != "V2" {
		t.Errorf("v2 구독자 수신 %q want V2", got)
	}
	select {
	case got := <-subOther.send:
		t.Errorf("다른 종목 구독자가 수신: %q", got)
	default:
	}
}
