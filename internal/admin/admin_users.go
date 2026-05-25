package admin

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// admin_users.go — broker 의 GET_USERS admin command (mymq.SubGetUsers).
//
// 패턴: iochdr (84 bytes) + _user[N] 가변 응답.
//   - argv[0] = usid 패턴 (fnmatch) 으로 필터, page/nofp 페이지네이션
//   - 응답 entry 는 64-bit linux 기준 96 bytes (time_t 8-byte 패딩 포함)
//
// C 측 정의: mymq/src/inc/user.h `struct _user`. 한 entry 96 bytes.
//
//	char  usid[16]              //  0-15
//	char  name[16]              // 16-31
//	char  chan[8]               // 32-39
//	char  maca[24]              // 40-63
//	in_addr addr (uint32)       // 64-67
//	uint32_t scid               // 68-71
//	pid_t  pid (int32)          // 72-75
//	int    clid                 // 76-79
//	int    sock                 // 80-83
//	(padding 4 bytes — 8-align)// 84-87
//	time_t when (int64)         // 88-95
//	─────────────────────────────────
//	                              96 bytes (64-bit linux)

const (
	mquserEntrySize  = 96
	mquserMaxEntries = 100
)

type mqusersJSON struct {
	Iochdr iochdrJSON   `json:"iochdr"`
	Users  []mquserItem `json:"users"`
}

type mquserItem struct {
	Usid    string `json:"usid"`
	Name    string `json:"name"`
	Chan    string `json:"chan"` // 채널 코드 (WEB/MOB/HTS/ADM/EMP/...)
	Maca    string `json:"maca"` // MAC address (text 12자 + ':' 등)
	IPAddr  string `json:"ip"`   // in_addr → dotted quad
	SCID    uint32 `json:"scid"` // session connection id
	Pid     int32  `json:"pid"`
	Clid    int32  `json:"clid"` // 고유 client id
	Sock    int32  `json:"sock"`
	WhenSec int64  `json:"when_sec"` // logon time (unix epoch seconds)
	WhenISO string `json:"when"`     // RFC3339 (사람이 읽기 쉬운)
}

func decodeMqusers(b []byte) (mqusersJSON, error) {
	var s mqusersJSON
	io, err := decodeIochdr(b)
	if err != nil {
		return s, err
	}
	s.Iochdr = io
	many := int(io.Many)
	if many > mquserMaxEntries {
		many = mquserMaxEntries
	}
	expected := iochdrSize + many*mquserEntrySize
	if len(b) < expected {
		return s, errShortBody{got: len(b), want: expected, what: "mqusers"}
	}
	be := binary.BigEndian
	s.Users = make([]mquserItem, many)
	for i := 0; i < many; i++ {
		off := iochdrSize + i*mquserEntrySize
		e := &s.Users[i]
		e.Usid = cstr(b[off : off+16])
		e.Name = cstr(b[off+16 : off+32])
		e.Chan = cstr(b[off+32 : off+40])
		e.Maca = cstr(b[off+40 : off+64])
		// in_addr 은 broker 가 host order 로 채울지 network order 로 둘지 모호.
		// addr 는 BE 로 직렬화 가정 (htonl 안 함 — broker admin.c 가 in_addr
		// 그대로 복사). dotted quad 로 변환.
		ipBytes := b[off+64 : off+68]
		e.IPAddr = net.IPv4(ipBytes[0], ipBytes[1], ipBytes[2], ipBytes[3]).String()
		e.SCID = be.Uint32(b[off+68:])
		e.Pid = int32(be.Uint32(b[off+72:]))
		e.Clid = int32(be.Uint32(b[off+76:]))
		e.Sock = int32(be.Uint32(b[off+80:]))
		// padding b[off+84 : off+88] 무시
		e.WhenSec = int64(be.Uint64(b[off+88:]))
		if e.WhenSec > 0 {
			e.WhenISO = time.Unix(e.WhenSec, 0).UTC().Format(time.RFC3339)
		}
	}
	return s, nil
}

// GetUsers — broker 의 GET_USERS admin command.
//
// query: ?argv0=usid_pattern&page=N&nofp=M (fnmatch glob 지원).
// 예: /v1/admin/users?argv0=trader* — usid 가 'trader' 로 시작하는 사용자만.
func GetUsers(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := encodeIochdr(parseIochdrQuery(r))
		fullBuf := make([]byte, iochdrSize+mquserMaxEntries*mquserEntrySize)
		copy(fullBuf, body)
		reply, err := callAdmin(r.Context(), r, deps, mymq.SubGetUsers, fullBuf)
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
		decoded, derr := decodeMqusers(reply.Body)
		if derr != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode_failed", derr.Error())
			return
		}
		raw, _ := json.Marshal(decoded)
		writeJSON(w, http.StatusOK, &AdminCmdResponse{Data: json.RawMessage(raw)})
	}
}
