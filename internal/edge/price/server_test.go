package price

import (
	"encoding/json"
	"strings"
	"testing"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func TestEncodeTickJSONWithJSONBody(t *testing.T) {
	tick := &wtgpb.Tick{
		MarketId:         42,
		Symbol:           "USDKRW",
		SeqNum:           7,
		Mask:             0xFF,
		Type:             1,
		Body:             []byte(`{"bid":1300.5}`),
		ReceivedUnixNano: 1735689600000000000,
	}
	out, err := encodeTickJSON(tick)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["symbol"] != "USDKRW" {
		t.Errorf("symbol: %v", got["symbol"])
	}
	// data 는 raw JSON object.
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 가 object 아님: %v", got["data"])
	}
	if data["bid"].(float64) != 1300.5 {
		t.Errorf("data.bid: %v", data["bid"])
	}
}

func TestEncodeTickJSONWithRawBody(t *testing.T) {
	tick := &wtgpb.Tick{
		Symbol: "USDKRW",
		Body:   []byte("RAW_TEXT"),
	}
	out, err := encodeTickJSON(tick)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	// raw text 는 string 으로 wrap.
	if data, ok := got["data"].(string); !ok || data != "RAW_TEXT" {
		t.Errorf("data: %v", got["data"])
	}
}

func TestEncodeTickJSONNoBody(t *testing.T) {
	tick := &wtgpb.Tick{Symbol: "USDKRW"}
	out, err := encodeTickJSON(tick)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"data"`) {
		t.Errorf("body 없을 때 data 필드 누락 안 됨: %s", out)
	}
}
