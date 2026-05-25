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

func TestGetExchangesSendsPlaceholder(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Subc != mymq.SubGetExchange {
				t.Errorf("Subc: %d", in.Subc)
			}
			if len(in.Body) != mqexchangeSize {
				t.Errorf("Body 길이: %d, want %d", len(in.Body), mqexchangeSize)
			}
			// many=0 응답 — broker 가 빈 exchange 카탈로그 돌려준 척
			out := make([]byte, mqexchangeSize)
			return &mymq.Reply{Body: out}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/exchanges", nil)
	GetExchanges(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}
}

func TestGetExchangesDecodesEntries(t *testing.T) {
	buf := make([]byte, mqexchangeSize)
	be := binary.BigEndian
	be.PutUint32(buf[0:], 3) // many=3
	for i := 0; i < 3; i++ {
		off := 4 + i*mqexchangeEntrySize
		copy(buf[off:off+16], []byte("EXCH"+string(rune('A'+i))))
		be.PutUint32(buf[off+16:], uint32(10+i)) // type
		be.PutUint32(buf[off+20:], uint32(i*2))  // link
	}
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: buf}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/exchanges", nil)
	GetExchanges(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	var got mqexchangeJSON
	_ = json.Unmarshal(env.Data, &got)
	if got.Many != 3 || len(got.Exch) != 3 {
		t.Fatalf("many: %d, exch len: %d", got.Many, len(got.Exch))
	}
	if got.Exch[0].Name != "EXCHA" || got.Exch[0].Type != 10 {
		t.Errorf("[0]: %+v", got.Exch[0])
	}
	if got.Exch[2].Name != "EXCHC" || got.Exch[2].Link != 4 {
		t.Errorf("[2]: %+v", got.Exch[2])
	}
}
