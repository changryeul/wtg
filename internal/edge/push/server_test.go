package push

import (
	"encoding/json"
	"strings"
	"testing"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

func TestEncodePushJSONWithJSONBody(t *testing.T) {
	msg := &wtgpb.PushMessage{
		Func:     13,
		Subc:     54,
		Exchange: "EXEC",
		LogonId:  "trader01",
		Data:     []byte(`{"order_id":"O-1"}`),
	}
	out, err := encodePushJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["logon_id"] != "trader01" {
		t.Errorf("logon_id: %v", got["logon_id"])
	}
	data, ok := got["data"].(map[string]any)
	if !ok || data["order_id"] != "O-1" {
		t.Errorf("data: %v", got["data"])
	}
}

func TestEncodePushJSONRawBody(t *testing.T) {
	msg := &wtgpb.PushMessage{
		LogonId: "trader01",
		Data:    []byte("ALERT_RAW"),
	}
	out, err := encodePushJSON(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if s, _ := got["data"].(string); s != "ALERT_RAW" {
		t.Errorf("data wrap: %v", got["data"])
	}
}

func TestEncodePushJSONNoBody(t *testing.T) {
	msg := &wtgpb.PushMessage{LogonId: "trader01"}
	out, _ := encodePushJSON(msg)
	if strings.Contains(string(out), `"data"`) {
		t.Errorf("body 없을 때 data 누락 안 됨: %s", out)
	}
}

func TestServerDispatchToUser(t *testing.T) {
	s := NewServer(DefaultConfig(), discardLogger())
	c := mkTestConn("trader01", 4)
	s.registry.Add(c)

	msg := &wtgpb.PushMessage{
		LogonId: "trader01",
		Data:    []byte(`{"x":1}`),
	}
	s.dispatch(msg)

	if len(c.send) != 1 {
		t.Errorf("queue: %d, want 1", len(c.send))
	}
	if s.totalDelivered.Load() != 1 {
		t.Errorf("totalDelivered: %d", s.totalDelivered.Load())
	}
}

func TestServerDispatchBroadcast(t *testing.T) {
	s := NewServer(DefaultConfig(), discardLogger())
	c1 := mkTestConn("trader01", 4)
	c2 := mkTestConn("trader02", 4)
	s.registry.Add(c1)
	s.registry.Add(c2)

	msg := &wtgpb.PushMessage{Data: []byte(`{"alert":"x"}`)}
	s.dispatch(msg)

	if len(c1.send) != 1 || len(c2.send) != 1 {
		t.Errorf("broadcast: c1=%d c2=%d", len(c1.send), len(c2.send))
	}
	if s.totalDelivered.Load() != 2 {
		t.Errorf("totalDelivered: %d", s.totalDelivered.Load())
	}
}

func TestServerDispatchUnknownUser(t *testing.T) {
	s := NewServer(DefaultConfig(), discardLogger())
	msg := &wtgpb.PushMessage{
		LogonId: "ghost",
		Data:    []byte(`{}`),
	}
	s.dispatch(msg)

	if s.totalDropped.Load() != 1 {
		t.Errorf("totalDropped: %d, want 1", s.totalDropped.Load())
	}
}
