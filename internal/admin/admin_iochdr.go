package admin

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"strconv"
)

// admin_iochdr.go — broker 의 iochdr-기반 admin command 공통 helper.
//
// GET_CLIENT / GET_USERS / GET_WHOIS 는 모두 같은 패턴:
//   - 요청: 84-byte iochdr 만 보내면 broker 가 받아준다 (next_l < sizeof(iochdr))
//   - argv[4][16] / page / nofp 를 채우면 필터/페이지네이션 활성
//   - 응답: iochdr (broker 가 maxr/many/next 채움) + record[many] 가변 길이
//
// C 측 정의: mymq/src/inc/admin.h `struct iochdr`. 총 84 bytes.

// iochdrSize 는 C 의 struct iochdr 의 총 byte 크기.
//
//	char argv[4][16]   //  64
//	int  maxr          //   4   (broker 가 채움 — 매칭된 전체 레코드 수)
//	int  nofp          //   4   (요청: 페이지당 레코드 수, broker 가 적용값으로 덮음)
//	int  many          //   4   (broker 가 채움 — 이번 페이지의 레코드 수)
//	int  page          //   4   (요청: 페이지 번호, 0=첫 페이지)
//	int  next          //   4   (broker 가 채움 — 다음 페이지 있음=1)
//	─────────────────────────
//	                       84 bytes
const iochdrSize = 84

// iochdrJSON 은 응답 iochdr 을 SPA 가 읽기 쉽게 노출.
//
// argv 는 응답에서 의미 없으니 생략 (broker 가 응답에서 zero out 하지 않음 →
// 요청 echo 라 노이즈). 페이지네이션 필드 4개만 노출.
type iochdrJSON struct {
	Maxr uint32 `json:"maxr"` // 매칭된 전체 레코드 수
	Nofp uint32 `json:"nofp"` // 페이지당 레코드 수 (broker 적용값)
	Many uint32 `json:"many"` // 이번 페이지의 레코드 수
	Page uint32 `json:"page"` // 요청 페이지 번호 (broker 가 echo)
	Next uint32 `json:"next"` // 다음 페이지 있음=1
}

// iochdrQuery 는 HTTP query string 으로 받는 요청 iochdr 입력.
type iochdrQuery struct {
	Argv [4]string // 명령별 의미 (예: GET_USERS 면 argv[0]=usid 패턴)
	Page uint32
	Nofp uint32
}

// parseIochdrQuery 는 r.URL.Query 에서 iochdrQuery 를 파싱.
//
// 지원하는 query:
//
//	?argv0=...&argv1=...&argv2=...&argv3=...
//	?page=N         (default 0)
//	?nofp=N         (default 0 — broker 가 max 적용)
//
// argv 는 16 byte 까지 — 초과 시 잘림.
func parseIochdrQuery(r *http.Request) iochdrQuery {
	q := r.URL.Query()
	var io iochdrQuery
	for i := 0; i < 4; i++ {
		key := "argv" + strconv.Itoa(i)
		v := q.Get(key)
		if len(v) > 15 {
			v = v[:15] // null-terminator 자리 1 byte 남김
		}
		io.Argv[i] = v
	}
	if v, err := strconv.ParseUint(q.Get("page"), 10, 32); err == nil {
		io.Page = uint32(v)
	}
	if v, err := strconv.ParseUint(q.Get("nofp"), 10, 32); err == nil {
		io.Nofp = uint32(v)
	}
	return io
}

// encodeIochdr 는 iochdrQuery 를 84-byte BE buffer 로 인코딩 (broker 요청용).
func encodeIochdr(q iochdrQuery) []byte {
	buf := make([]byte, iochdrSize)
	for i := 0; i < 4; i++ {
		copy(buf[i*16:(i+1)*16], q.Argv[i])
	}
	be := binary.BigEndian
	// maxr=0, many=0, next=0 — broker 가 응답에서 채움
	be.PutUint32(buf[68:], q.Nofp) // off 68
	be.PutUint32(buf[76:], q.Page) // off 76
	return buf
}

// decodeIochdr 는 응답 buffer 의 처음 84 bytes 를 iochdrJSON 으로 변환.
func decodeIochdr(b []byte) (iochdrJSON, error) {
	var io iochdrJSON
	if len(b) < iochdrSize {
		return io, errShortBody{got: len(b), want: iochdrSize}
	}
	be := binary.BigEndian
	io.Maxr = be.Uint32(b[64:])
	io.Nofp = be.Uint32(b[68:])
	io.Many = be.Uint32(b[72:])
	io.Page = be.Uint32(b[76:])
	io.Next = be.Uint32(b[80:])
	return io, nil
}

// errShortBody — 응답 body 가 예상보다 짧을 때.
type errShortBody struct {
	got, want int
	what      string
}

func (e errShortBody) Error() string {
	what := e.what
	if what == "" {
		what = "body"
	}
	return what + " 짧음 (" + strconv.Itoa(e.got) + " < " + strconv.Itoa(e.want) + ")"
}

// cstr 은 C-style null-terminated 문자열 추출 (bytes 안의 첫 NUL 까지).
// 운영 데이터 유효성 보존 — broker 응답의 fixed-array 안 NUL 이후 쓰레기 무시.
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
