package mdsshim

import (
	"fmt"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// RkeyW9504A01 은 shim 이 수신하는 첫 서비스 (수동 스왑포인트/마진 등록).
const RkeyW9504A01 = "W9504A01"

// Applier 는 파싱된 스왑포인트를 카탈로그 (etcd) 에 반영한다.
// clear=true 는 regTp=2(해제) — 해당 pair 의 수동 스왑 전체 삭제.
type Applier func(pair string, ups []SwapUpdate, clear bool) error

// ZdivFunc 는 pair 의 소수 스케일 (mds fold->zdiv 동등) 을 돌려준다.
// symbols 카탈로그 미비 시 0 (스케일 없음) 을 반환하면 된다.
type ZdivFunc func(pair string) int

// HandleW9504A01 은 broker 가 라우팅해 준 요청 프레임 1건을 처리하고
// 응답 FrameInput 을 만든다. 서비스 불일치 프레임이면 (nil, nil).
//
// 응답 프레임 규약 (mymqd message_packet_transfer 역방향):
//   - ckey echo (호출측 멀티플렉싱 매칭)
//   - navi 는 수신 프레임 그대로 (broker 가 origin 으로 역주행)
//   - Dirf=DirBackward, Func=FCTran
//   - 실패 시 Errn ≠ 0 + 빈 본문 (엔진 컨벤션 — errn 은 그대로 전달)
//
// ※ 실 mymqd 대상 검증 전까지의 가정 — EC2 실 broker 스모크가 게이트.
func HandleW9504A01(u *mymq.Unsolicited, zdiv ZdivFunc, apply Applier) (*mymq.FrameInput, error) {
	rkey := cString(u.Header.Rkey[:])
	if rkey != RkeyW9504A01 {
		return nil, nil
	}

	// 응답 = C mymq_reply 동등: Dirf=ORIGIN + 요청 navi 유지 — broker 가
	// navi[0] (origin client) 로 역송한다 (mq_send.c dosend/ORIGIN 참조).
	reply := &mymq.FrameInput{
		Func: mymq.FCTran,
		Dirf: mymq.DirOrigin,
		Ckey: u.Header.Ckey,
		Clid: u.Header.Clid, // origin client id echo — broker 의 역송 대상 식별
		Wkey: u.Header.Wkey,
		Chan: u.Header.Chan,
	}
	if u.Decoded != nil && len(u.Decoded.Navis) > 0 {
		// broker 는 방향과 무관하게 navi[nvia-1] 을 목적지로 삼는다
		// (mqd/message.c: to = &navi[nvia-1]). 응답은 요청 경로의 역순 —
		// origin (요청 navi[0], Scid 포함) 이 마지막에 오도록 뒤집는다.
		src := u.Decoded.Navis
		rev := make([]mymq.Navi, len(src))
		for i, n := range src {
			rev[len(src)-1-i] = n
		}
		reply.Navis = rev
		origin := src[0]
		reply.Xchg = cString(origin.Xchg[:])
		reply.Rkey = cString(origin.Rkey[:])
	}

	req, err := ParseW2006A01(u.Body)
	if err != nil {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: %s 파싱 실패: %w", rkey, err)
	}

	ups, skipped := req.SwapUpdates(zdiv(req.Pair))
	if err := apply(req.Pair, ups, req.RegTp == RegTpUnregister); err != nil {
		reply.Errn = 1
		return reply, fmt.Errorf("mdsshim: %s 반영 실패 (pair=%s): %w", rkey, req.Pair, err)
	}
	_ = skipped // 미지 tenor 는 mds 와 동일하게 무시 — 호출부에서 로그

	reply.Body = BuildW9504A01Reply(req.Pair)
	return reply, nil
}

// cString 은 고정폭 NUL/공백 패딩 필드를 Go string 으로 변환한다.
func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
