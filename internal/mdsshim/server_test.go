package mdsshim

import (
	"testing"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

func unsolFor(rkey string, ckey uint32, body []byte) *mymq.Unsolicited {
	u := &mymq.Unsolicited{Body: body}
	copy(u.Header.Rkey[:], rkey)
	copy(u.Header.Xchg[:], "dom")
	u.Header.Ckey = ckey
	// 요청 navi: [0]=origin(호출자), [1]=목적지(우리 큐) — broker 수신 형태
	var origin, dest mymq.Navi
	copy(origin.Xchg[:], "dom")
	copy(origin.Rkey[:], "CALLER")
	origin.Scid = 42
	copy(dest.Xchg[:], "dom")
	copy(dest.Rkey[:], rkey)
	u.Decoded = &mymq.DecodedFrame{Navis: []mymq.Navi{origin, dest}}
	return u
}

func TestHandleW9504A01(t *testing.T) {
	body := buildW2006A01("1", "USD/KRW", "1", [][9]string{
		{"M01", "15", "25", "", "", "", "", "", ""},
	})

	var gotPair string
	var gotUps []SwapUpdate
	var gotClear bool
	apply := func(pair string, ups []SwapUpdate, clear bool) error {
		gotPair, gotUps, gotClear = pair, ups, clear
		return nil
	}

	reply, err := HandleW9504A01(unsolFor("W9504A01", 77, body),
		func(string) int { return 2 }, apply)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gotPair != "USD/KRW" || gotClear || len(gotUps) != 1 ||
		gotUps[0].Tenor != "1M" || gotUps[0].Bid != 0.15 {
		t.Fatalf("applier 인자 불일치: pair=%q clear=%v ups=%+v", gotPair, gotClear, gotUps)
	}
	if reply.Ckey != 77 || reply.Errn != 0 || reply.Dirf != mymq.DirOrigin {
		t.Fatalf("reply 프레임 불일치: %+v", reply)
	}
	// navi 역순 — origin (Scid=42) 이 마지막이어야 broker 가 역송한다
	if len(reply.Navis) != 2 || reply.Navis[1].Scid != 42 {
		t.Fatalf("navi 역순 실패: %+v", reply.Navis)
	}
	if reply.Rkey != "CALLER" {
		t.Fatalf("목적지 rkey=%q, want CALLER", reply.Rkey)
	}
	if string(reply.Body) != "USD/KRW " {
		t.Fatalf("reply body=%q", reply.Body)
	}
}

func TestHandleW9504A01_ServiceMismatch(t *testing.T) {
	reply, err := HandleW9504A01(unsolFor("W9999X99", 1, nil),
		func(string) int { return 0 },
		func(string, []SwapUpdate, bool) error { return nil })
	if reply != nil || err != nil {
		t.Fatalf("불일치 서비스는 (nil,nil) 이어야 함: %v %v", reply, err)
	}
}

func TestHandleW9504A01_ParseError(t *testing.T) {
	reply, err := HandleW9504A01(unsolFor("W9504A01", 5, []byte("bad")),
		func(string) int { return 0 },
		func(string, []SwapUpdate, bool) error { return nil })
	if err == nil {
		t.Fatalf("파싱 에러여야 함")
	}
	if reply == nil || reply.Errn == 0 || reply.Ckey != 5 {
		t.Fatalf("에러 응답도 ckey echo + errn 필요: %+v", reply)
	}
}
