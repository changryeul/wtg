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

func TestGetClientsForwardsIochdrQuery(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Subc != mymq.SubGetClient {
				t.Errorf("Subc: %d", in.Subc)
			}
			// argv[0]=trader* 에 있어야
			if string(in.Body[0:7]) != "trader*" {
				t.Errorf("argv[0]: %q", in.Body[0:7])
			}
			// page=2, nofp=20 검증 (off 76, 68)
			if got := binary.BigEndian.Uint32(in.Body[76:]); got != 2 {
				t.Errorf("page: %d, want 2", got)
			}
			if got := binary.BigEndian.Uint32(in.Body[68:]); got != 20 {
				t.Errorf("nofp: %d, want 20", got)
			}
			// 빈 응답 (many=0)
			out := make([]byte, iochdrSize)
			return &mymq.Reply{Body: out}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/clients?argv0=trader*&page=2&nofp=20", nil)
	GetClients(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}
}

func TestGetClientsDecodesOneEntry(t *testing.T) {
	buf := make([]byte, iochdrSize+mqclientEntrySize)
	be := binary.BigEndian
	be.PutUint32(buf[64:], 1) // maxr
	be.PutUint32(buf[72:], 1) // many
	off := iochdrSize
	be.PutUint16(buf[off:], 7)    // flag
	be.PutUint16(buf[off+2:], 2)  // type
	be.PutUint32(buf[off+4:], 99) // sock
	copy(buf[off+8:off+24], "mci-api")
	be.PutUint32(buf[off+24:], 1234) // pid
	copy(buf[off+28:off+44], "10.0.0.5")
	copy(buf[off+44:off+60], "trader01")
	copy(buf[off+60:off+76], "ORDER_Q")
	be.PutUint32(buf[off+76:], 1)  // qflg
	be.PutUint32(buf[off+100:], 5) // nsvc

	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: buf}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/clients", nil)
	GetClients(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	var got mqclientJSON
	_ = json.Unmarshal(env.Data, &got)
	if got.Iochdr.Many != 1 || len(got.Clients) != 1 {
		t.Fatalf("many: %d, clients len: %d", got.Iochdr.Many, len(got.Clients))
	}
	c := got.Clients[0]
	if c.Name != "mci-api" || c.Pid != 1234 || c.IPAddr != "10.0.0.5" ||
		c.User != "trader01" || c.QName != "ORDER_Q" || c.NSvc != 5 {
		t.Errorf("decoded: %+v", c)
	}
}
