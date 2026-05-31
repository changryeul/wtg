package price

import (
	"encoding/json"
	"testing"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// legacy envelope 변환 — cs framework 가 broker subscribe 시 받던 schema 와 동일한지.
func TestEncodeTickLegacyJSON_FullBidAsk(t *testing.T) {
	body := `{"sym":"USDKRW","bid":1380.10,"ask":1380.50,"src":"BEST","seq":42,"ts":"2026-05-31T10:00:00Z"}`
	tick := &wtgpb.Tick{
		Symbol: "USDKRW",
		SeqNum: 42,
		Body:   []byte(body),
	}
	out, err := encodeTickLegacyJSON(tick)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["msgtype"] != "incremental" {
		t.Errorf("msgtype=%v, want incremental", got["msgtype"])
	}
	if got["symbol"] != "USDKRW" {
		t.Errorf("symbol=%v", got["symbol"])
	}
	if got["feed"] != "BEST" {
		t.Errorf("feed=%v, want BEST", got["feed"])
	}
	if got["ts"] != "2026-05-31T10:00:00Z" {
		t.Errorf("ts=%v", got["ts"])
	}
	entries, _ := got["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries count=%d, want 2 (bid+ask)", len(entries))
	}
	// 첫 entry = bid
	bid := entries[0].(map[string]any)
	if bid["type"] != "bid" || bid["px"] != 1380.10 {
		t.Errorf("bid entry: %+v", bid)
	}
	ask := entries[1].(map[string]any)
	if ask["type"] != "ask" || ask["px"] != 1380.50 {
		t.Errorf("ask entry: %+v", ask)
	}
}

// 한쪽 호가만 — 그것만 entry 추가.
func TestEncodeTickLegacyJSON_BidOnly(t *testing.T) {
	body := `{"sym":"USDKRW","bid":1380.10,"src":"BEST","ts":"2026-05-31T10:00:00Z"}`
	tick := &wtgpb.Tick{Body: []byte(body)}
	out, err := encodeTickLegacyJSON(tick)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	entries, _ := got["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["type"] != "bid" {
		t.Errorf("expected single bid entry, got %+v", entries)
	}
}

// body 가 JSON 아니면 에러.
func TestEncodeTickLegacyJSON_NonJSON(t *testing.T) {
	tick := &wtgpb.Tick{Body: []byte("not-json")}
	_, err := encodeTickLegacyJSON(tick)
	if err == nil {
		t.Fatal("expected error for non-JSON body")
	}
}

// 빈 body — 에러.
func TestEncodeTickLegacyJSON_EmptyBody(t *testing.T) {
	tick := &wtgpb.Tick{}
	_, err := encodeTickLegacyJSON(tick)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}
