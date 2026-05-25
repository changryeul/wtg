package admin

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// admin_status.go — broker 의 GET_STATUS admin command (mymq.SubGetStatus) 처리.
//
// 핵심 패턴 (broker 의 admin command 공통):
//   - 요청: broker 가 채워줄 placeholder buffer 만큼의 zero-filled body 를 보낸다.
//     (broker 가 sizeof 검사로 거부하므로 nil 본문은 errn=1010 "INVALID ARGUMENTS")
//   - 응답: broker 가 그 buffer 자리를 BE network byte order int 로 채워서 돌려준다.
//
// C 측 정의: mymq/src/inc/admin.h `struct mqstat`. 총 116 bytes (모두 int=4B,
// 패딩 없음). 매매 엔진과의 wire 호환성을 위해 fields 를 그 순서/크기 그대로 유지.

// mqstatSize 는 C 의 struct mqstat 의 총 byte 크기.
//
//	int  tcp_port_number          //  4
//	int  how_to_routing[2]        //  8
//	int  no_of_threads            //  4
//	int  no_of_clients[2]         //  8
//	int  no_of_brokers[2]         //  8
//	int  no_of_packets[2]         //  8
//	int  no_of_exchange[2]        //  8
//	int  no_of_queues[2]          //  8
//	int  no_of_linkage[2]         //  8
//	int  heartbeat                //  4
//	struct { int que_no, que_sz, que_ad; } queue[4]   // 12*4 = 48
//	───────────────────────────────────────────────────
//	                                                 116 bytes
const mqstatSize = 116

// mqstatJSON 은 broker 응답을 JSON 으로 직렬화하기 위한 view.
//
// uint32 사용 — broker 가 htonl 로 채우는 값들은 모두 비음수 (port/count/size).
// JSON 키는 C 필드명을 snake_case 그대로 — UI 가 검색/필터링하기 쉬움.
type mqstatJSON struct {
	TCPPortNumber uint32     `json:"tcp_port_number"`
	HowToRouting  [2]uint32  `json:"how_to_routing"` // [0]=AP↔AP, [1]=AP↔Broker
	NoOfThreads   uint32     `json:"no_of_threads"`
	NoOfClients   [2]uint32  `json:"no_of_clients"` // [0]=총, [1]=빈자리
	NoOfBrokers   [2]uint32  `json:"no_of_brokers"` // [0]=max_rec, [1]=available
	NoOfPackets   [2]uint32  `json:"no_of_packets"` // [0]=alloc, [1]=free
	NoOfExchange  [2]uint32  `json:"no_of_exchange"`
	NoOfQueues    [2]uint32  `json:"no_of_queues"`
	NoOfLinkage   [2]uint32  `json:"no_of_linkage"`
	Heartbeat     uint32     `json:"heartbeat"`
	Queue         [4]mqstatQ `json:"queue"`
}

type mqstatQ struct {
	QueNo uint32 `json:"que_no"`
	QueSz uint32 `json:"que_sz"`
	QueAd uint32 `json:"que_ad"`
}

// decodeMqstat 은 broker 응답 buffer (116 bytes BE) 를 mqstatJSON 으로 변환.
func decodeMqstat(b []byte) (mqstatJSON, error) {
	var s mqstatJSON
	if len(b) < mqstatSize {
		return s, fmt.Errorf("mqstat: body 짧음 (%d < %d)", len(b), mqstatSize)
	}
	be := binary.BigEndian
	off := 0
	read := func() uint32 {
		v := be.Uint32(b[off:])
		off += 4
		return v
	}
	s.TCPPortNumber = read()
	s.HowToRouting[0], s.HowToRouting[1] = read(), read()
	s.NoOfThreads = read()
	s.NoOfClients[0], s.NoOfClients[1] = read(), read()
	s.NoOfBrokers[0], s.NoOfBrokers[1] = read(), read()
	s.NoOfPackets[0], s.NoOfPackets[1] = read(), read()
	s.NoOfExchange[0], s.NoOfExchange[1] = read(), read()
	s.NoOfQueues[0], s.NoOfQueues[1] = read(), read()
	s.NoOfLinkage[0], s.NoOfLinkage[1] = read(), read()
	s.Heartbeat = read()
	for i := 0; i < 4; i++ {
		s.Queue[i].QueNo = read()
		s.Queue[i].QueSz = read()
		s.Queue[i].QueAd = read()
	}
	return s, nil
}

// GetStatus — broker 의 GET_STATUS admin command 호출.
//
// nil body 로 보내면 broker 가 sizeof(mqstat) 검사에서 거부 (errn=1010).
// 116-byte zero-filled placeholder 를 보내야 broker 가 그 자리에 결과를 채워서
// 돌려준다. 응답은 BE int 들 → mqstatJSON 으로 변환해서 SPA 가 읽기 쉽게 노출.
func GetStatus(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		placeholder := make([]byte, mqstatSize)
		reply, err := callAdmin(r.Context(), r, deps, mymq.SubGetStatus, placeholder)
		if err != nil {
			writeBrokerError(w, deps.Logger, r, err)
			return
		}
		if reply.Errn != 0 {
			writeJSON(w, http.StatusUnprocessableEntity, &AdminCmdResponse{
				Errn: reply.Errn,
				Errm: reply.ErrMsg,
			})
			return
		}
		decoded, derr := decodeMqstat(reply.Body)
		if derr != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode_failed", derr.Error())
			return
		}
		raw, _ := json.Marshal(decoded)
		writeJSON(w, http.StatusOK, &AdminCmdResponse{Data: json.RawMessage(raw)})
	}
}
