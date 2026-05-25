package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// TestGetWhoisForwardsXchgRkey — broker 의 진짜 시멘틱은 argv[0]=xchg, argv[1]=rkey.
func TestGetWhoisForwardsXchgRkey(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Subc != mymq.SubGetWhois {
				t.Errorf("Subc: %d", in.Subc)
			}
			if string(in.Body[0:7]) != "ECHOSVC" {
				t.Errorf("argv[0]: %q", in.Body[0:7])
			}
			if string(in.Body[16:20]) != "PING" {
				t.Errorf("argv[1]: %q", in.Body[16:20])
			}
			out := make([]byte, iochdrSize)
			return &mymq.Reply{Body: out}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/whois?argv0=ECHOSVC&argv1=PING", nil)
	GetWhois(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}
}

func TestGetWhoisDecodesEntry(t *testing.T) {
	buf := make([]byte, iochdrSize+mqwhoisEntrySize)
	be := binary.BigEndian
	be.PutUint32(buf[64:], 1) // maxr
	be.PutUint32(buf[72:], 1) // many
	off := iochdrSize
	copy(buf[off:off+9], "ECHOSVC")
	copy(buf[off+9:off+26], "PING")
	copy(buf[off+26:off+43], "test_service")
	copy(buf[off+43:off+60], "ECHO_Q")
	be.PutUint32(buf[off+60:], 1234)       // mqid
	be.PutUint32(buf[off+64:], 567)        // msid
	buf[off+68] = 1                        // qflg
	buf[off+69] = 2                        // qatt
	buf[off+70] = 3                        // eatt
	buf[off+71] = 4                        // link
	be.PutUint32(buf[off+72:], 0xC0A80001) // 192.168.0.1

	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: buf}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/whois", nil)
	GetWhois(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	var got mqwhoisJSON
	_ = json.Unmarshal(env.Data, &got)
	if len(got.Whois) != 1 {
		t.Fatalf("whois len: %d", len(got.Whois))
	}
	w := got.Whois[0]
	if w.Xchg != "ECHOSVC" || w.Rkey != "PING" || w.Appl != "test_service" ||
		w.QName != "ECHO_Q" || w.MqID != 1234 || w.MsID != 567 ||
		w.QFlag != 1 || w.QAttr != 2 || w.EAttr != 3 || w.Link != 4 ||
		w.Addr != "192.168.0.1" {
		t.Errorf("decoded: %+v", w)
	}
}
