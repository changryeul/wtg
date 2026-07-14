package mdsshim

import (
	"fmt"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// W9501S01 (종가 조회) — wire 명세는 cside/wtgquery/wtgquery.h 와 동일:
// in 40B (pdcd4+type4+symb16+tenor16), dat 208B (16B×13), out 헤더 40B.
// 백엔드는 mci-chart GET /v1/chart?tf=1d (wtgquery PoC 검증 경로).

const RkeyW9501S01 = "W9501S01"

const (
	w9501InLen  = 4 + 4 + 16 + 16
	w9501DatLen = 13 * 16
	w9501HdrLen = 4 + 16 + 16 + 4
)

// W9501Request 는 파싱된 W9501S01 입력이다.
type W9501Request struct {
	Pdcd string // "SPT" 만 지원 — "FWD" 는 forward-snapshot 연동 전 미지원
	Symb string // 원문 6자 ("USDKRW")
	Pair string // WTG 표기 ("USD/KRW")
}

// ChartBar 는 mci-chart 일봉 1건 (핸들러가 백엔드에서 받아 옴).
type ChartBar struct {
	Kymd, Khms                                     string
	BidO, BidH, BidL, BidC, AskO, AskH, AskL, AskC float64
}

// ChartFunc 는 pair 의 일봉을 돌려준다 (main 이 mci-chart REST 로 배선).
type ChartFunc func(pair string) ([]ChartBar, error)

func ParseW9501S01(b []byte) (*W9501Request, error) {
	if len(b) < w9501InLen {
		return nil, fmt.Errorf("mdsshim: W9501S01 입력 미달 (%d < %d)", len(b), w9501InLen)
	}
	pdcd := field(b, 0, 4)
	if pdcd != "SPT" {
		return nil, fmt.Errorf("mdsshim: 미지원 pdcd %q (FWD 는 phase 2)", pdcd)
	}
	symb := field(b, 8, 16)
	if len(symb) != 6 {
		return nil, fmt.Errorf("mdsshim: symb 형식 불량 %q", symb)
	}
	return &W9501Request{Pdcd: pdcd, Symb: symb, Pair: symbToPair(symb)}, nil
}

// BuildW9501S01Reply 는 out 헤더 + dat×n 을 조립한다 (수치는 "%.5f" ASCII).
func BuildW9501S01Reply(pdcd, symb string, bars []ChartBar) []byte {
	out := make([]byte, w9501HdrLen+len(bars)*w9501DatLen)
	for i := range out {
		out[i] = ' '
	}
	copy(out[0:4], pdcd)
	copy(out[4:20], symb)
	copy(out[36:40], fmt.Sprintf("%d", len(bars)))
	for i, bar := range bars {
		d := out[w9501HdrLen+i*w9501DatLen:]
		put := func(off int, v string) { copy(d[off:off+16], v) }
		put(0, symb)
		put(32, bar.Kymd)
		put(48, bar.Khms)
		f := func(v float64) string { return fmt.Sprintf("%.5f", v) }
		put(64, f(bar.BidO))
		put(80, f(bar.BidH))
		put(96, f(bar.BidL))
		put(112, f(bar.BidC))
		put(128, f(bar.AskO))
		put(144, f(bar.AskH))
		put(160, f(bar.AskL))
		put(176, f(bar.AskC))
	}
	return out
}

// HandleW9501S01 은 W9504A01 핸들러와 동일 응답 규약 (DirOrigin + navi 역순).
func HandleW9501S01(u *mymq.Unsolicited, chart ChartFunc) (*mymq.FrameInput, error) {
	if cString(u.Header.Rkey[:]) != RkeyW9501S01 {
		return nil, nil
	}
	reply := newReplyFrame(u)
	req, err := ParseW9501S01(u.Body)
	if err != nil {
		reply.Errn = 1
		return reply, err
	}
	bars, err := chart(req.Pair)
	if err != nil {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: chart 조회 실패 (pair=%s): %w", req.Pair, err)
	}
	reply.Body = BuildW9501S01Reply(req.Pdcd, req.Symb, bars)
	return reply, nil
}

// symbToPair 는 "USDKRW" → "USD/KRW". 6자 아니면 원문 유지.
func symbToPair(symb string) string {
	if len(symb) == 6 && !strings.Contains(symb, "/") {
		return symb[:3] + "/" + symb[3:]
	}
	return symb
}
