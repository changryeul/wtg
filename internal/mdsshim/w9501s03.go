package mdsshim

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// W9501S03 (현재가 다건 조회) — W9501S02 의 bulk. mds 원형(mds/W9500/W9501S03.c)은
// nrec 만큼 루프 돌며 각 pair 를 W9501S02_core 로 채운다. 셔임도 동일 — 입력의
// 각 W9501S02_in 을 spot 조회해 W9501S02_out(546B) 로 채워 배열로 응답.
//
// wire (mds W9500.h):
//   in  = nrec[6] + W9501S02_in_t[N]   (N*64B)
//   out = nrec[6] + W9501S02_out_t[N]  (N*546B)
// 웹(/v1/tx) 경로는 앞에 COMHDR(512B) 동반 — 길이 정합으로 판별.

const RkeyW9501S03 = "W9501S03"

const w9501s03NrecLen = 6

// splitComhdrS03 은 가변길이 S03 입력에서 COMHDR 를 분리한다. S03 은 입력 길이가
// nrec 에 따라 가변이라 고정 inLen 을 못 쓴다 — nrec 를 raw/COMHDR 두 오프셋에서
// 읽어 전체 길이가 정합하는 쪽을 택한다.
func splitComhdrS03(b []byte) (hdr, body []byte) {
	// raw: nrec@0, 전체 = 6 + N*64
	if n, ok := parseNrec(b, 0); ok && len(b) == w9501s03NrecLen+n*w9501s02InLen {
		return nil, b
	}
	// COMHDR: nrec@512, 전체 = 512 + 6 + N*64
	if len(b) >= comhdrLen+w9501s03NrecLen {
		if n, ok := parseNrec(b, comhdrLen); ok && len(b) == comhdrLen+w9501s03NrecLen+n*w9501s02InLen {
			return b[:comhdrLen], b[comhdrLen:]
		}
	}
	// 정합 실패 — COMHDR 있으면 벗겨 최선 처리, 없으면 그대로.
	if len(b) >= comhdrLen+w9501s03NrecLen {
		return b[:comhdrLen], b[comhdrLen:]
	}
	return nil, b
}

// parseNrec 은 off 위치의 6B ASCII nrec 를 읽는다 (공백 트림). 음수/과대는 실패.
func parseNrec(b []byte, off int) (int, bool) {
	if off+w9501s03NrecLen > len(b) {
		return 0, false
	}
	s := strings.TrimSpace(string(b[off : off+w9501s03NrecLen]))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1000 {
		return 0, false
	}
	return n, true
}

// HandleW9501S03 은 다건 현재가 조회. 각 레코드를 spot 조회해 W9501S02 응답으로 채운다.
// 응답 규약은 HandleW9501S02 와 동일 (COMHDR 에코 + output).
func HandleW9501S03(u *mymq.Unsolicited, spot SpotFunc) (*mymq.FrameInput, error) {
	if cString(u.Header.Rkey[:]) != RkeyW9501S03 {
		return nil, nil
	}
	reply := newReplyFrame(u)
	hdr, body := splitComhdrS03(u.Body)
	if len(body) < w9501s03NrecLen {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: W9501S03 nrec 미달 (%d)", len(body))
	}
	nrec, ok := parseNrec(body, 0)
	if !ok {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: W9501S03 nrec 파싱 실패")
	}

	// 출력 조립 — nrec[6] + N*546B.
	out := make([]byte, w9501s03NrecLen+nrec*w9501s02OutLen)
	for i := range out {
		out[i] = ' '
	}
	copy(out[:w9501s03NrecLen], fmt.Sprintf("%-6d", nrec))

	for i := 0; i < nrec; i++ {
		inOff := w9501s03NrecLen + i*w9501s02InLen
		if inOff+w9501s02InLen > len(body) {
			break // 입력 부족 — 이후 레코드는 공백 유지
		}
		req, err := ParseW9501S02(body[inOff : inOff+w9501s02InLen])
		if err != nil {
			continue // 개별 레코드 파싱 실패는 공백으로 두고 계속
		}
		q, err := spot(req.Pair)
		if err != nil {
			// 시세 부재 레코드는 exnm/symb 만 에코, 값은 공백.
			q = &SpotQuote{}
		}
		rep := BuildW9501S02Reply(req, q) // 546B
		copy(out[w9501s03NrecLen+i*w9501s02OutLen:], rep)
	}

	if hdr != nil {
		h := make([]byte, comhdrLen)
		copy(h, hdr)
		h[comhdrEflgOff] = '0'
		reply.Body = append(h, out...)
	} else {
		reply.Body = out
	}
	return reply, nil
}
