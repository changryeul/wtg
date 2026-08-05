package mdsshim

import (
	"fmt"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// W9501S02 (실시간 시세 스냅샷 조회) — wire 명세는 mds/W9500/W9500.h 의
// W9501S02_in_t / out_t 와 동일 (__MID__ 블록 제외):
// in 64B (exnm16 + symb16 + pay_ymd16 + exp_ymd16),
// out 546B (16B×30 + source 1B×2 + 16B×4).
// 백엔드는 mci-price GET /v1/quote/spot 의 raw 호가 — 원본 mds 도 LP/BEST raw 를
// 반환하고, 마진은 클라이언트 밴드마진(fn_getBndMrgn)이 얹는 구조라 raw 가 등가다.
// NH 주문화면 32종 (4001/4002/4004/4203, 8001/8002 MM 등) 이 시장가 세팅에 쓴다.

const RkeyW9501S02 = "W9501S02"

const (
	w9501s02InLen  = 4 * 16
	w9501s02OutLen = 30*16 + 2 + 4*16 // 546
)

// 웹(/v1/tx) 경로는 mci-api svcio 가 COMHDR(512B, comhdr.h) 를 input 앞에 붙인다 —
// C 원장 AP 와 동일한 규약 (AP 는 헤더를 에코하고 eflg/rcod/mesg 로 결과를 알린다).
// C-SDK raw 경로 (wtgquery 계열) 는 input 만 온다 — 길이로 판별한다.
const (
	comhdrLen     = 512
	comhdrEflgOff = 182 // char eflg[1] — '0' 정상 / '1' 오류
)

// splitComhdr 는 body 를 (COMHDR, input) 으로 나눈다. COMHDR 없으면 hdr=nil.
func splitComhdr(b []byte, inLen int) (hdr, in []byte) {
	if len(b) >= comhdrLen+inLen {
		return b[:comhdrLen], b[comhdrLen:]
	}
	return nil, b
}

// W9501S02Request 는 파싱된 W9501S02 입력이다.
type W9501S02Request struct {
	Exnm   string // 'BEST' 등 — 현재 BEST 만 의미 (mci-price 가 BEST 산출)
	Symb   string // 원문 ("USD/KRW" 또는 C 채널의 "USDKRW")
	Pair   string // WTG 표기 ("USD/KRW")
	PayYmd string
	ExpYmd string
}

// SpotQuote 는 mci-price 스냅샷 1건 (핸들러가 백엔드에서 받아 옴).
type SpotQuote struct {
	Bid, Ask float64
	Source   string // "BEST" 등 — out.bid_source/ask_source 1B 로 축약
}

// SpotFunc 는 pair 의 현물 스냅샷을 돌려준다 (main 이 mci-price REST 로 배선).
type SpotFunc func(pair string) (*SpotQuote, error)

func ParseW9501S02(b []byte) (*W9501S02Request, error) {
	if len(b) < w9501s02InLen {
		return nil, fmt.Errorf("mdsshim: W9501S02 입력 미달 (%d < %d)", len(b), w9501s02InLen)
	}
	symb := field(b, 16, 16)
	if symb == "" {
		return nil, fmt.Errorf("mdsshim: W9501S02 symb 누락")
	}
	pair := symb
	if !strings.Contains(pair, "/") {
		pair = symbToPair(symb)
	}
	return &W9501S02Request{
		Exnm:   field(b, 0, 16),
		Symb:   symb,
		Pair:   pair,
		PayYmd: field(b, 32, 16),
		ExpYmd: field(b, 48, 16),
	}, nil
}

// BuildW9501S02Reply 는 out 546B 를 조립한다. 시가/고저가/전일대비 등 audit 성
// 필드는 봉 데이터 연동 전까지 공백 (docs/mds-coverage.md 의 잔여 항목) —
// 호출부 (NH 화면) 핵심 경로는 bid/ask 만 읽는다.
func BuildW9501S02Reply(req *W9501S02Request, q *SpotQuote) []byte {
	out := make([]byte, w9501s02OutLen)
	for i := range out {
		out[i] = ' '
	}
	put16 := func(idx int, v string) { copy(out[idx*16:idx*16+16], v) }
	f := func(v float64) string { return fmt.Sprintf("%.5f", v) }
	put16(0, req.Exnm)
	put16(1, req.Symb)
	put16(4, req.PayYmd)
	put16(5, req.ExpYmd)
	put16(17, f(q.Bid)) // bid
	put16(19, f(q.Ask)) // ask
	put16(28, f(q.Bid)) // bid_best — BEST 원천 스냅샷이므로 동일
	put16(29, f(q.Ask)) // ask_best
	src := byte('B')
	if q.Source != "" {
		src = q.Source[0]
	}
	out[480] = src // bid_source
	out[481] = src // ask_source
	return out
}

// HandleW9501S02 은 W9504A01/W9501S01 핸들러와 동일 응답 규약 (DirOrigin + navi 역순).
// 웹 경로 (COMHDR 동반) 는 응답도 COMHDR 에코 (eflg='0') + output 으로 돌려준다 —
// mci-api 가 응답 COMHDR 의 eflg/rcod/mesg 를 header 로, output 을 data 로 역직렬화한다.
func HandleW9501S02(u *mymq.Unsolicited, spot SpotFunc) (*mymq.FrameInput, error) {
	if cString(u.Header.Rkey[:]) != RkeyW9501S02 {
		return nil, nil
	}
	reply := newReplyFrame(u)
	hdr, in := splitComhdr(u.Body, w9501s02InLen)
	req, err := ParseW9501S02(in)
	if err != nil {
		reply.Errn = 1
		return reply, err
	}
	q, err := spot(req.Pair)
	if err != nil {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: spot 조회 실패 (pair=%s): %w", req.Pair, err)
	}
	out := BuildW9501S02Reply(req, q)
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
