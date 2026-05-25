package admin

import (
	"encoding/binary"
	"encoding/json"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// admin_clients.go — broker 의 GET_CLIENT admin command (mymq.SubGetClient).
//
// 패턴: iochdr (84 bytes) + client[N] 가변 응답.
//   - 요청은 iochdr 만 보내면 broker 통과 (next_l < sizeof(iochdr))
//   - argv[0] = usid 패턴 (fnmatch) 으로 필터, page/nofp 페이지네이션
//   - 응답은 broker 가 iochdr.many 만큼만 client[] 를 채워서 plen 정확히 셋팅
//
// C 측 정의: mymq/src/inc/admin.h `struct client`. 한 entry 120 bytes.
//
//	short flag                  //  0-1
//	short type                  //  2-3
//	int   sock                  //  4-7
//	char  name[16]              //  8-23
//	pid_t pid                   // 24-27   (4 bytes)
//	struct s_parms {            // 28-59
//	  char ipad[16]             // 28-43
//	  char user[16]             // 44-59
//	}
//	struct q_parms {            // 60-99
//	  char qnam[16]             // 60-75
//	  int  qflg                 // 76-79
//	  int  qatt                 // 80-83
//	  int  qkey                 // 84-87
//	  int  mqid                 // 88-91
//	  int  msid                 // 92-95
//	  int  link                 // 96-99
//	}
//	int     nsvc                // 100-103
//	uint8_t cymd[6]             // 104-109   (YY-MM-DD HH:MM:SS BCD)
//	uint8_t rtim[6]             // 110-115
//	uint8_t doxy[4]             // 116-119
//	─────────────────────────────────
//	                              120 bytes

const (
	mqclientEntrySize  = 120
	mqclientMaxEntries = 100
)

type mqclientJSON struct {
	Iochdr  iochdrJSON      `json:"iochdr"`
	Clients []mqclientEntry `json:"clients"`
}

type mqclientEntry struct {
	Flag   int16  `json:"flag"`
	Type   int16  `json:"type"`
	Sock   int32  `json:"sock"`
	Name   string `json:"name"` // ApplName (예: mci-api)
	Pid    int32  `json:"pid"`
	IPAddr string `json:"ip"`   // 접속 IP (s_parms.ipad)
	User   string `json:"user"` // 로그인 사용자 (s_parms.user)
	QName  string `json:"qnam"`
	QFlag  int32  `json:"qflg"`
	QAttr  int32  `json:"qatt"`
	QKey   int32  `json:"qkey"`
	MqID   int32  `json:"mqid"`
	MsID   int32  `json:"msid"`
	QLink  int32  `json:"link"`
	NSvc   int32  `json:"nsvc"`    // bound services 수
	ConnTS string `json:"conn_ts"` // YY-MM-DD HH:MM:SS (cymd)
	RegTS  string `json:"reg_ts"`  // (rtim)
	Domain []byte `json:"doxy"`    // domain X & Y flag (4 bytes raw)
}

// decodeBCD6 — broker 의 6-byte BCD 시각 (YY MM DD HH MM SS) → "YY-MM-DD HH:MM:SS".
// 값이 모두 0 이면 빈 문자열 반환 (미설정 상태).
func decodeBCD6(b []byte) string {
	if len(b) < 6 {
		return ""
	}
	zero := true
	for _, x := range b {
		if x != 0 {
			zero = false
			break
		}
	}
	if zero {
		return ""
	}
	return formatTS6(b)
}

func formatTS6(b []byte) string {
	// 형식: 20YY-MM-DD HH:MM:SS — broker 의 cymd/rtim 은 단순 binary (year=YY).
	out := make([]byte, 0, 19)
	out = append(out, '2', '0')
	out = append(out, d2(b[0])...)
	out = append(out, '-')
	out = append(out, d2(b[1])...)
	out = append(out, '-')
	out = append(out, d2(b[2])...)
	out = append(out, ' ')
	out = append(out, d2(b[3])...)
	out = append(out, ':')
	out = append(out, d2(b[4])...)
	out = append(out, ':')
	out = append(out, d2(b[5])...)
	return string(out)
}

func d2(v uint8) []byte {
	if v >= 100 {
		v = 99
	}
	return []byte{'0' + (v / 10), '0' + (v % 10)}
}

func decodeMqclient(b []byte) (mqclientJSON, error) {
	var s mqclientJSON
	io, err := decodeIochdr(b)
	if err != nil {
		return s, err
	}
	s.Iochdr = io
	many := int(io.Many)
	if many > mqclientMaxEntries {
		many = mqclientMaxEntries
	}
	expected := iochdrSize + many*mqclientEntrySize
	if len(b) < expected {
		return s, errShortBody{got: len(b), want: expected, what: "mqclient"}
	}
	be := binary.BigEndian
	s.Clients = make([]mqclientEntry, many)
	for i := 0; i < many; i++ {
		off := iochdrSize + i*mqclientEntrySize
		e := &s.Clients[i]
		e.Flag = int16(be.Uint16(b[off:]))
		e.Type = int16(be.Uint16(b[off+2:]))
		e.Sock = int32(be.Uint32(b[off+4:]))
		e.Name = cstr(b[off+8 : off+24])
		e.Pid = int32(be.Uint32(b[off+24:]))
		e.IPAddr = cstr(b[off+28 : off+44])
		e.User = cstr(b[off+44 : off+60])
		e.QName = cstr(b[off+60 : off+76])
		e.QFlag = int32(be.Uint32(b[off+76:]))
		e.QAttr = int32(be.Uint32(b[off+80:]))
		e.QKey = int32(be.Uint32(b[off+84:]))
		e.MqID = int32(be.Uint32(b[off+88:]))
		e.MsID = int32(be.Uint32(b[off+92:]))
		e.QLink = int32(be.Uint32(b[off+96:]))
		e.NSvc = int32(be.Uint32(b[off+100:]))
		e.ConnTS = decodeBCD6(b[off+104 : off+110])
		e.RegTS = decodeBCD6(b[off+110 : off+116])
		e.Domain = append([]byte(nil), b[off+116:off+120]...)
	}
	return s, nil
}

// GetClients — broker 의 GET_CLIENT admin command.
//
// query: ?argv0=usid_pattern&page=N&nofp=M (fnmatch glob 지원).
func GetClients(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := encodeIochdr(parseIochdrQuery(r))
		// broker 응답 buffer 크기 = iochdr + max client entries
		// 응답 plen 은 broker 가 정확히 셋팅하므로 placeholder 는 풀 사이즈 확보.
		fullBuf := make([]byte, iochdrSize+mqclientMaxEntries*mqclientEntrySize)
		copy(fullBuf, body)
		reply, err := callAdmin(r.Context(), r, deps, mymq.SubGetClient, fullBuf)
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
		decoded, derr := decodeMqclient(reply.Body)
		if derr != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode_failed", derr.Error())
			return
		}
		raw, _ := json.Marshal(decoded)
		writeJSON(w, http.StatusOK, &AdminCmdResponse{Data: json.RawMessage(raw)})
	}
}
