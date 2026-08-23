package krx

import (
	"encoding/json"
	"testing"
	"time"
)

// buildKrxV2 — legacy(flat kind) 를 폴리모픽 v2 envelope 로 감싼다.
func TestBuildKrxV2(t *testing.T) {
	legacy := []byte(`{"kind":"fut.book","code":"101V6000","time":"090005123456","ask":[{"prc":265.80,"vol":1,"cnt":1}]}`)
	b, err := buildKrxV2("101V6000", legacy)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		EV         int             `json:"ev"`
		Type       string          `json:"type"`
		AssetClass string          `json:"asset_class"`
		Symbol     string          `json:"symbol"`
		TSUnixNano int64           `json:"ts_unix_nano"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if env.EV != 2 || env.Type != "krx.fut.book" || env.AssetClass != "FUTURE" {
		t.Errorf("헤더 판별자 오류: ev=%d type=%q ac=%q", env.EV, env.Type, env.AssetClass)
	}
	if env.Symbol != "101V6000" {
		t.Errorf("symbol=%q want 101V6000", env.Symbol)
	}
	if env.TSUnixNano == 0 {
		t.Error("ts_unix_nano 변환 실패 (0)")
	}
	// data 는 원 struct 를 무손실 보존 (호가 배열 포함).
	var data map[string]interface{}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["kind"] != "fut.book" || data["code"] != "101V6000" {
		t.Errorf("data 원본 미보존: %s", env.Data)
	}
	if ask, ok := data["ask"].([]interface{}); !ok || len(ask) != 1 {
		t.Errorf("data 호가배열 미보존: %s", env.Data)
	}
}

func TestAssetClassForKind(t *testing.T) {
	cases := map[string]string{
		"fut.trade": "FUTURE", "fut.book": "FUTURE", "fut.settle": "FUTURE", "fut.master": "FUTURE",
		"bond.trade": "BOND", "bond.book": "BOND", "bond.master": "BOND",
		"weird": "UNKNOWN", "": "UNKNOWN",
	}
	for in, want := range cases {
		if got := assetClassForKind(in); got != want {
			t.Errorf("assetClassForKind(%q)=%q want %q", in, got, want)
		}
	}
}

func TestKrxTimeToUnixNano(t *testing.T) {
	if kstLoc == nil {
		t.Skip("Asia/Seoul TZ 미로드 — 스킵")
	}
	// 09:00:05.123456 (KST) → 오늘 날짜 기준.
	got := krxTimeToUnixNano("090005123456")
	if got == 0 {
		t.Fatal("유효 시각인데 0 반환")
	}
	tm := time.Unix(0, got).In(kstLoc)
	if tm.Hour() != 9 || tm.Minute() != 0 || tm.Second() != 5 {
		t.Errorf("시각 파싱 오류: %v", tm)
	}
	if tm.Nanosecond() != 123456*1000 {
		t.Errorf("마이크로초 파싱 오류: %d", tm.Nanosecond())
	}
	// 불가/빈값 → 0.
	for _, bad := range []string{"", "12", "9a0005000000", "250000000000"} {
		if v := krxTimeToUnixNano(bad); v != 0 {
			t.Errorf("krxTimeToUnixNano(%q)=%d want 0", bad, v)
		}
	}
}

// BroadcastBySymbolV — 같은 종목 구독한 ev=0/ev=2 subscriber 가 각자 다른 버전 수신.
func TestHub_BroadcastBySymbolV_PerConnectionVersion(t *testing.T) {
	h := NewHub()
	legacySub := NewSubscriber("legacy", 4, 0)
	v2Sub := NewSubscriber("v2", 4, 2)
	h.Add(legacySub)
	h.Add(v2Sub)

	sent, dropped := h.BroadcastBySymbolV("101V6000", []byte("V1"), []byte("V2"))
	if sent != 2 || dropped != 0 {
		t.Fatalf("sent=%d dropped=%d want 2/0", sent, dropped)
	}
	if got := string(<-legacySub.sendQ); got != "V1" {
		t.Errorf("legacy 수신 %q want V1", got)
	}
	if got := string(<-v2Sub.sendQ); got != "V2" {
		t.Errorf("v2 수신 %q want V2", got)
	}
}
