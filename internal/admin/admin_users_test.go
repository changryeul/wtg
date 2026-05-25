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

func TestGetUsersDecodesEntry(t *testing.T) {
	buf := make([]byte, iochdrSize+mquserEntrySize)
	be := binary.BigEndian
	be.PutUint32(buf[64:], 1) // maxr
	be.PutUint32(buf[72:], 1) // many
	off := iochdrSize
	copy(buf[off:off+16], "trader42")
	copy(buf[off+16:off+32], "Trader Forty-Two")
	copy(buf[off+32:off+40], "WEB")
	copy(buf[off+40:off+64], "AA:BB:CC:DD:EE:FF")
	// in_addr 192.168.1.10
	buf[off+64], buf[off+65], buf[off+66], buf[off+67] = 192, 168, 1, 10
	be.PutUint32(buf[off+68:], 0xCAFEBABE) // scid
	be.PutUint32(buf[off+72:], 5678)       // pid
	be.PutUint32(buf[off+76:], 100)        // clid
	be.PutUint32(buf[off+80:], 33)         // sock
	// padding 84-87 비워둠
	be.PutUint64(buf[off+88:], 1700000000) // when (2023-11-14 UTC 근처)

	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Subc != mymq.SubGetUsers {
				t.Errorf("Subc: %d", in.Subc)
			}
			if len(in.Body) < iochdrSize {
				t.Errorf("Body 길이: %d, want >= %d", len(in.Body), iochdrSize)
			}
			return &mymq.Reply{Body: buf}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil)
	GetUsers(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	var got mqusersJSON
	_ = json.Unmarshal(env.Data, &got)
	if len(got.Users) != 1 {
		t.Fatalf("users len: %d", len(got.Users))
	}
	u := got.Users[0]
	if u.Usid != "trader42" || u.Chan != "WEB" || u.IPAddr != "192.168.1.10" ||
		u.SCID != 0xCAFEBABE || u.Pid != 5678 || u.Clid != 100 || u.WhenSec != 1700000000 {
		t.Errorf("decoded: %+v", u)
	}
	if u.WhenISO == "" {
		t.Errorf("when_iso 비어있음 — when_sec=%d", u.WhenSec)
	}
}
