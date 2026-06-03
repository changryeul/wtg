package transform

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/routing"
)

func TestEnvelopeValidateRequest(t *testing.T) {
	cases := []struct {
		name    string
		env     Envelope
		wantErr string
	}{
		{"ok", Envelope{Exchange: "ORDER", RoutingKey: "NEW"}, ""},
		{"no exchange ok", Envelope{RoutingKey: "STATUS"}, ""},
		{"no rkey", Envelope{Exchange: "ORDER"}, "alias 또는 routing_key 필요"},
		{"alias only ok", Envelope{Alias: "ORDER_NEW"}, ""},
		{"long rkey", Envelope{RoutingKey: strings.Repeat("X", 17)}, "routing_key too long"},
		{"long xchg", Envelope{Exchange: strings.Repeat("X", 9), RoutingKey: "Y"}, "exchange too long"},
		{"long pkey", Envelope{RoutingKey: "Y", Pkey: strings.Repeat("P", 81)}, "pkey/nkey too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.env.ValidateRequest()
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("기대: 통과, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("기대 %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestEnvelopeBuildFrame(t *testing.T) {
	env := &Envelope{
		Exchange:   "ORDER",
		RoutingKey: "NEW",
		Data:       json.RawMessage(`{"symbol":"USDKRW","qty":100}`),
	}
	frame, err := env.BuildFrame(0xCAFE, "trader01", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Func != mymq.FCTran || frame.Subc != mymq.SubTranMsg {
		t.Errorf("func/subc: %d/%d", frame.Func, frame.Subc)
	}
	if frame.Xchg != "ORDER" || frame.Rkey != "NEW" {
		t.Errorf("xchg/rkey: %q/%q", frame.Xchg, frame.Rkey)
	}
	if frame.Ckey != 0xCAFE {
		t.Errorf("ckey: 0x%X", frame.Ckey)
	}
	if !bytes.Equal(frame.Body, []byte(`{"symbol":"USDKRW","qty":100}`)) {
		t.Errorf("body: %q", frame.Body)
	}
	if frame.Keyc != mymq.KeySend {
		t.Errorf("keyc default: %v", frame.Keyc)
	}
}

func TestEnvelopeBuildFrameWithKeys(t *testing.T) {
	env := &Envelope{
		RoutingKey: "QUERY",
		Keyc:       "N",
		Pkey:       "p1",
		Nkey:       "n2",
	}
	frame, err := env.BuildFrame(1, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Keyc != mymq.KeyNext {
		t.Errorf("keyc: %v", frame.Keyc)
	}
	if string(frame.Pkey) != "p1" {
		t.Errorf("pkey: %q", frame.Pkey)
	}
	if string(frame.Nkey) != "n2" {
		t.Errorf("nkey: %q", frame.Nkey)
	}
}

func TestEnvelopeBuildFrameRejectsInvalid(t *testing.T) {
	env := &Envelope{} // alias/routing_key 둘 다 없음
	if _, err := env.BuildFrame(0, "", "", nil); err == nil {
		t.Error("validation 에러를 기대했으나 통과")
	}
}

// alias 가 활성 룰로 등록되어 있으면 exchange/routing_key 가 룰 값으로 치환됨.
func TestEnvelopeBuildFrameResolvesAlias(t *testing.T) {
	reg := routing.NewInMemoryRegistry(nil)
	reg.Put(&routing.Rule{
		Alias: "ORDER_NEW", Exchange: "ORDER_V2", RoutingKey: "NEW_V2", Active: true,
	}, "admin")

	env := &Envelope{
		Alias:      "ORDER_NEW",
		Exchange:   "OLD_EXCHANGE",    // 무시되어야 함
		RoutingKey: "OLD_ROUTING_KEY", // 무시되어야 함
	}
	frame, err := env.BuildFrame(0, "u", "", reg)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Xchg != "ORDER_V2" || frame.Rkey != "NEW_V2" {
		t.Errorf("alias 치환 실패: xchg=%q rkey=%q", frame.Xchg, frame.Rkey)
	}
}

// alias 가 미등록이면 ErrUnknownAlias.
func TestEnvelopeBuildFrameUnknownAlias(t *testing.T) {
	reg := routing.NewInMemoryRegistry(nil)
	env := &Envelope{Alias: "NOPE", RoutingKey: "fallback-ignored"}
	_, err := env.BuildFrame(0, "u", "", reg)
	if !errors.Is(err, ErrUnknownAlias) {
		t.Errorf("err=%v, want ErrUnknownAlias", err)
	}
}

// alias 가 비활성이면 ErrUnknownAlias (보수적 거부).
func TestEnvelopeBuildFrameInactiveAlias(t *testing.T) {
	reg := routing.NewInMemoryRegistry(nil)
	reg.Put(&routing.Rule{Alias: "OFF", Exchange: "X", RoutingKey: "Y", Active: false}, "admin")
	env := &Envelope{Alias: "OFF"}
	_, err := env.BuildFrame(0, "u", "", reg)
	if !errors.Is(err, ErrUnknownAlias) {
		t.Errorf("err=%v, want ErrUnknownAlias", err)
	}
}

// alias 미사용 + Registry nil — 기존 raw passthrough.
func TestEnvelopeBuildFrameNoAliasNoRegistry(t *testing.T) {
	env := &Envelope{Exchange: "ORDER", RoutingKey: "NEW"}
	frame, err := env.BuildFrame(0, "u", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Xchg != "ORDER" || frame.Rkey != "NEW" {
		t.Errorf("raw passthrough 실패: %q/%q", frame.Xchg, frame.Rkey)
	}
}

func TestEnvelopeFromReplyJSONBody(t *testing.T) {
	reply := &mymq.Reply{
		Body:   []byte(`{"order_id":"O-1","status":"ACCEPTED"}`),
		Errn:   0,
		ErrMsg: "",
	}
	env := FromReply(reply)
	if env.Errn != 0 {
		t.Errorf("Errn: %d", env.Errn)
	}
	// data 는 raw JSON 그대로.
	var got map[string]string
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["order_id"] != "O-1" {
		t.Errorf("order_id: %q", got["order_id"])
	}
}

func TestEnvelopeFromReplyNonJSONBody(t *testing.T) {
	// 엔진이 JSON 아닌 raw text 를 보내면 string 으로 감싸서 노출.
	reply := &mymq.Reply{
		Body: []byte("RAW_PAYLOAD"),
	}
	env := FromReply(reply)
	var s string
	if err := json.Unmarshal(env.Data, &s); err != nil {
		t.Fatal(err)
	}
	if s != "RAW_PAYLOAD" {
		t.Errorf("string wrap: %q", s)
	}
}

func TestEnvelopeFromReplyError(t *testing.T) {
	reply := &mymq.Reply{
		Errn:   mymq.ErrAuth,
		ErrMsg: "Authentication failed",
	}
	env := FromReply(reply)
	if env.Errn != mymq.ErrAuth {
		t.Errorf("Errn: %d", env.Errn)
	}
	if env.Errm != "Authentication failed" {
		t.Errorf("Errm: %q", env.Errm)
	}
	if len(env.Data) != 0 {
		t.Errorf("Data 비어있어야 함: %s", env.Data)
	}
}

func TestEnvelopeFromReplyNil(t *testing.T) {
	env := FromReply(nil)
	if env == nil {
		t.Fatal("nil reply 에서 nil envelope 반환")
	}
	if env.Errn != 0 || len(env.Data) != 0 {
		t.Errorf("non-zero envelope from nil: %+v", env)
	}
}
