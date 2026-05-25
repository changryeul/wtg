package admin

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// admin_whois.go — broker 의 GET_WHOIS admin command (mymq.SubGetWhois).
//
// 패턴: iochdr (84 bytes) + _whois[N] 가변 응답.
//
// **broker 의 진짜 시멘틱**:
//   - argv[0] = exchange 이름 (정확 매칭, strncasecmp)
//   - argv[1] = routing key 패턴 (fnmatch glob)
//   - argv[2] = queue 이름 (정확 매칭)
//   → "어떤 transaction 이 어디(어느 broker/AP)로 가는가" 를 찾는 도구.
//   사용자 ID 검색 아님 — usid 검색은 GET_USERS 의 argv[0] 사용.
//
// C 측 정의: mymq/src/inc/admin.h `struct _whois`. 한 entry 76 bytes.
//
//	char  xchg[L_XCHG+1=9]      //  0-8
//	char  rkey[L_RKEY+1=17]     //  9-25
//	char  appl[L_NAME+1=17]     // 26-42
//	char  qnam[L_NAME+1=17]     // 43-59
//	int   mqid                  // 60-63
//	int   msid                  // 64-67
//	char  qflg                  // 68
//	char  qatt                  // 69
//	char  eatt                  // 70
//	uint8_t link                // 71
//	uint32_t addr               // 72-75
//	──────────────────────────────────
//	                              76 bytes

const (
	mqwhoisEntrySize  = 76
	mqwhoisMaxEntries = 200
)

type mqwhoisJSON struct {
	Iochdr iochdrJSON    `json:"iochdr"`
	Whois  []mqwhoisItem `json:"whois"`
}

type mqwhoisItem struct {
	Xchg  string `json:"xchg"` // exchange 이름
	Rkey  string `json:"rkey"` // routing key
	Appl  string `json:"appl"` // 등록한 ApplName
	QName string `json:"qnam"` // queue 이름
	MqID  int32  `json:"mqid"`
	MsID  int32  `json:"msid"`
	QFlag uint8  `json:"qflg"`
	QAttr uint8  `json:"qatt"`
	EAttr uint8  `json:"eatt"`           // exchange attribute
	Link  uint8  `json:"link"`           // linked AP 수
	Addr  string `json:"addr,omitempty"` // inter-net svc 주소 (dotted quad, 0 이면 omit)
}

func decodeMqwhois(b []byte) (mqwhoisJSON, error) {
	var s mqwhoisJSON
	io, err := decodeIochdr(b)
	if err != nil {
		return s, err
	}
	s.Iochdr = io
	many := int(io.Many)
	if many > mqwhoisMaxEntries {
		many = mqwhoisMaxEntries
	}
	expected := iochdrSize + many*mqwhoisEntrySize
	if len(b) < expected {
		return s, errShortBody{got: len(b), want: expected, what: "mqwhois"}
	}
	be := binary.BigEndian
	s.Whois = make([]mqwhoisItem, many)
	for i := 0; i < many; i++ {
		off := iochdrSize + i*mqwhoisEntrySize
		e := &s.Whois[i]
		e.Xchg = cstr(b[off : off+9])
		e.Rkey = cstr(b[off+9 : off+26])
		e.Appl = cstr(b[off+26 : off+43])
		e.QName = cstr(b[off+43 : off+60])
		e.MqID = int32(be.Uint32(b[off+60:]))
		e.MsID = int32(be.Uint32(b[off+64:]))
		e.QFlag = b[off+68]
		e.QAttr = b[off+69]
		e.EAttr = b[off+70]
		e.Link = b[off+71]
		addrRaw := be.Uint32(b[off+72:])
		if addrRaw != 0 {
			e.Addr = net.IPv4(byte(addrRaw>>24), byte(addrRaw>>16),
				byte(addrRaw>>8), byte(addrRaw)).String()
		}
	}
	return s, nil
}

// GetWhois — broker 의 GET_WHOIS admin command.
//
// query: ?argv0=xchg&argv1=rkey_pattern&argv2=qnam&page=N&nofp=M
// 예: /v1/admin/whois?argv0=ECHOSVC          — ECHOSVC exchange 의 모든 등록
//
//	/v1/admin/whois?argv1=PING             — routing key 가 PING 인 모든 등록
//	/v1/admin/whois?argv0=ORDER&argv1=NEW* — ORDER exchange 의 NEW* rkey
func GetWhois(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := encodeIochdr(parseIochdrQuery(r))
		fullBuf := make([]byte, iochdrSize+mqwhoisMaxEntries*mqwhoisEntrySize)
		copy(fullBuf, body)
		reply, err := callAdmin(r.Context(), r, deps, mymq.SubGetWhois, fullBuf)
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
		decoded, derr := decodeMqwhois(reply.Body)
		if derr != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode_failed", derr.Error())
			return
		}
		raw, _ := json.Marshal(decoded)
		writeJSON(w, http.StatusOK, &AdminCmdResponse{Data: json.RawMessage(raw)})
	}
}
