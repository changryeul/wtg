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

// TestGetStatusSendsPlaceholder — broker 의 GET_STATUS 가 sizeof(mqstat) 검사를
// 통과하려면 116-byte zero-filled body 를 보내야 한다 (admin.c:39-43).
// nil/짧은 body 를 보내면 broker 가 errn=1010 으로 거부 — 그래서 SPA 의
// 브로커 명령 페이지가 "전부 오류" 상태였던 사고의 회귀 방지 테스트.
func TestGetStatusSendsPlaceholder(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Subc != mymq.SubGetStatus {
				t.Errorf("Subc: %d, want SubGetStatus", in.Subc)
			}
			if len(in.Body) != mqstatSize {
				t.Errorf("Body 길이: %d, want %d", len(in.Body), mqstatSize)
			}
			for i, b := range in.Body {
				if b != 0 {
					t.Errorf("placeholder byte[%d] = %x, want 0", i, b)
					break
				}
			}
			// broker 가 정상 응답한 척 — 116 byte buffer 를 그대로 echo (모두 0).
			return &mymq.Reply{Body: make([]byte, mqstatSize)}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil)
	GetStatus(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}
}

// TestGetStatusDecodesResponse — broker 가 채워준 116-byte BE buffer 를
// 정확한 필드별 JSON 으로 디코드하는지 검증.
func TestGetStatusDecodesResponse(t *testing.T) {
	// 알려진 값들로 buffer 구성 — broker 가 htonl 로 넣는 그대로.
	buf := make([]byte, mqstatSize)
	be := binary.BigEndian
	off := 0
	put := func(v uint32) { be.PutUint32(buf[off:], v); off += 4 }
	put(11217)               // tcp_port_number
	put(1)                   // how_to_routing[0]
	put(2)                   // how_to_routing[1]
	put(8)                   // no_of_threads
	put(42)                  // no_of_clients[0]
	put(7)                   // no_of_clients[1]
	put(100)                 // no_of_brokers[0]
	put(95)                  // no_of_brokers[1]
	put(1024)                // no_of_packets[0]
	put(900)                 // no_of_packets[1]
	put(15)                  // no_of_exchange[0]
	put(15)                  // no_of_exchange[1]
	put(20)                  // no_of_queues[0]
	put(18)                  // no_of_queues[1]
	put(5)                   // no_of_linkage[0]
	put(4)                   // no_of_linkage[1]
	put(30)                  // heartbeat
	for i := 0; i < 4; i++ { // queue[4]
		put(uint32(100 + i)) // que_no
		put(uint32(200 + i)) // que_sz
		put(uint32(i % 2))   // que_ad
	}

	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: buf}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil)
	GetStatus(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", rr.Code, rr.Body.String())
	}

	var env AdminCmdResponse
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if env.Errn != 0 {
		t.Errorf("Errn: %d, want 0", env.Errn)
	}
	var got mqstatJSON
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("data decode: %v", err)
	}
	if got.TCPPortNumber != 11217 {
		t.Errorf("tcp_port_number: %d, want 11217", got.TCPPortNumber)
	}
	if got.HowToRouting != [2]uint32{1, 2} {
		t.Errorf("how_to_routing: %v, want [1 2]", got.HowToRouting)
	}
	if got.NoOfClients != [2]uint32{42, 7} {
		t.Errorf("no_of_clients: %v, want [42 7]", got.NoOfClients)
	}
	if got.Heartbeat != 30 {
		t.Errorf("heartbeat: %d, want 30", got.Heartbeat)
	}
	for i := 0; i < 4; i++ {
		if got.Queue[i].QueNo != uint32(100+i) {
			t.Errorf("queue[%d].que_no: %d, want %d", i, got.Queue[i].QueNo, 100+i)
		}
		if got.Queue[i].QueSz != uint32(200+i) {
			t.Errorf("queue[%d].que_sz: %d, want %d", i, got.Queue[i].QueSz, 200+i)
		}
		if got.Queue[i].QueAd != uint32(i%2) {
			t.Errorf("queue[%d].que_ad: %d, want %d", i, got.Queue[i].QueAd, i%2)
		}
	}
}

// TestGetStatusBrokerErrn — broker 가 reply.Errn 을 채워서 돌려주면
// 422 + AdminCmdResponse{Errn,Errm} 으로 그대로 전달.
func TestGetStatusBrokerErrn(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 1234, ErrMsg: "broker rejected"}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil)
	GetStatus(newDeps(caller))(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: %d, want 422", rr.Code)
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	if env.Errn != 1234 || env.Errm != "broker rejected" {
		t.Errorf("envelope: %+v", env)
	}
}

// TestGetStatusShortBody — broker 가 116 미만 짧은 body 로 응답하면 500.
// 정상 broker 라면 발생 안 하지만 stub/proxy 환경에서 방어.
func TestGetStatusShortBody(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: make([]byte, 50)}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil)
	GetStatus(newDeps(caller))(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d, want 500", rr.Code)
	}
}

// TestDecodeMqstatRoundTrip — pure decoder 단위 테스트.
func TestDecodeMqstatRoundTrip(t *testing.T) {
	if _, err := decodeMqstat(make([]byte, mqstatSize-1)); err == nil {
		t.Errorf("짧은 buffer: nil 에러 — want 길이 부족 에러")
	}
	if _, err := decodeMqstat(make([]byte, mqstatSize)); err != nil {
		t.Errorf("정확한 buffer: err=%v", err)
	}
	// 더 큰 buffer 도 허용 (앞 116 bytes 만 사용).
	if _, err := decodeMqstat(make([]byte, mqstatSize+10)); err != nil {
		t.Errorf("긴 buffer: err=%v", err)
	}
}
