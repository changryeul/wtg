package admin

import (
	"encoding/binary"
	"encoding/json"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// admin_exchanges.go — broker 의 GET_EXCHANGE admin command (mymq.SubGetExchange).
//
// status 와 동일한 fixed-size 패턴 — iochdr 없음. broker 가 sizeof(mqexchange)
// 검사를 통과하려면 1924-byte zero-filled body 가 필요하다.
//
// C 측 정의: mymq/src/inc/admin.h `struct mqexchange`.
//
//	int many                                              //   4
//	struct {                                              //
//	  char name[16]                                       //  16
//	  int  type                                           //   4
//	  int  link                                           //   4
//	} exch[80]                                            // 80*24 = 1920
//	─────────────────────────────────────────────────────
//	                                                          1924 bytes

const (
	mqexchangeSize       = 4 + 80*mqexchangeEntrySize
	mqexchangeEntrySize  = 24
	mqexchangeMaxEntries = 80
)

type mqexchangeJSON struct {
	Many uint32           `json:"many"`
	Exch []mqexchangeItem `json:"exch"`
}

type mqexchangeItem struct {
	Name string `json:"name"`
	Type uint32 `json:"type"` // exchange 타입 (DIRECT/FANOUT 등)
	Link uint32 `json:"link"` // 연결된 binding 수
}

// decodeMqexchange — broker 응답 buffer (1924 bytes BE) → mqexchangeJSON.
//
// many 만큼만 exch[] 를 슬라이스로 반환. 빈 슬롯은 무시.
func decodeMqexchange(b []byte) (mqexchangeJSON, error) {
	var s mqexchangeJSON
	if len(b) < mqexchangeSize {
		return s, errShortBody{got: len(b), want: mqexchangeSize, what: "mqexchange"}
	}
	be := binary.BigEndian
	s.Many = be.Uint32(b[0:])
	if s.Many > mqexchangeMaxEntries {
		s.Many = mqexchangeMaxEntries // broker 가 잘못 보내도 buffer overrun 방지
	}
	s.Exch = make([]mqexchangeItem, s.Many)
	for i := uint32(0); i < s.Many; i++ {
		off := 4 + int(i)*mqexchangeEntrySize
		s.Exch[i] = mqexchangeItem{
			Name: cstr(b[off : off+16]),
			Type: be.Uint32(b[off+16:]),
			Link: be.Uint32(b[off+20:]),
		}
	}
	return s, nil
}

// GetExchanges — broker 의 GET_EXCHANGE admin command.
func GetExchanges(deps *HandlerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		placeholder := make([]byte, mqexchangeSize)
		reply, err := callAdmin(r.Context(), r, deps, mymq.SubGetExchange, placeholder)
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
		decoded, derr := decodeMqexchange(reply.Body)
		if derr != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode_failed", derr.Error())
			return
		}
		raw, _ := json.Marshal(decoded)
		writeJSON(w, http.StatusOK, &AdminCmdResponse{Data: json.RawMessage(raw)})
	}
}
