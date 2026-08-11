package mdsshim

import (
	"strconv"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/mymq"
)

// W9501S03 bulk — 2건 입력을 spot 조회해 nrec+2*546B 로 응답하는지.
func TestHandleW9501S03_Bulk(t *testing.T) {
	// 입력: nrec=2 + 2 × W9501S02_in(64B: exnm16+symb16+pay16+exp16)
	mkin := func(exnm, symb string) []byte {
		b := make([]byte, w9501s02InLen)
		for i := range b {
			b[i] = ' '
		}
		copy(b[0:16], exnm)
		copy(b[16:32], symb)
		return b
	}
	body := []byte("2     ") // nrec[6]
	body = append(body, mkin("BEST", "USD/KRW")...)
	body = append(body, mkin("BEST", "EUR/KRW")...)

	u := &mymq.Unsolicited{Body: body}
	copy(u.Header.Rkey[:], "W9501S03")

	spot := func(pair string) (*SpotQuote, error) {
		switch pair {
		case "USD/KRW":
			return &SpotQuote{Bid: 1380.5, Ask: 1381.0, Source: "BEST"}, nil
		case "EUR/KRW":
			return &SpotQuote{Bid: 1500.2, Ask: 1500.9, Source: "BEST"}, nil
		}
		return &SpotQuote{}, nil
	}

	reply, err := HandleW9501S03(u, spot)
	if err != nil {
		t.Fatalf("HandleW9501S03: %v", err)
	}
	want := w9501s03NrecLen + 2*w9501s02OutLen
	if len(reply.Body) != want {
		t.Fatalf("응답 길이 %d, want %d", len(reply.Body), want)
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(string(reply.Body[:6]))); n != 2 {
		t.Errorf("nrec=%d, want 2", n)
	}
	// 1번째 레코드 bid(idx17) 채워졌나
	rec0 := reply.Body[w9501s03NrecLen:]
	bid0 := strings.TrimSpace(string(rec0[17*16 : 17*16+16]))
	if bid0 != "1380.50000" {
		t.Errorf("rec0 bid=%q, want 1380.50000", bid0)
	}
	// 2번째 레코드 symb(idx1) 에코
	rec1 := reply.Body[w9501s03NrecLen+w9501s02OutLen:]
	symb1 := strings.TrimSpace(string(rec1[1*16 : 1*16+16]))
	if symb1 != "EUR/KRW" {
		t.Errorf("rec1 symb=%q, want EUR/KRW", symb1)
	}
}

// COMHDR 동반(웹) 경로 판별.
func TestHandleW9501S03_ComHdr(t *testing.T) {
	in := make([]byte, w9501s02InLen)
	for i := range in {
		in[i] = ' '
	}
	copy(in[0:16], "BEST")
	copy(in[16:32], "USD/KRW")
	body := make([]byte, comhdrLen)
	body = append(body, []byte("1     ")...)
	body = append(body, in...)

	u := &mymq.Unsolicited{Body: body}
	copy(u.Header.Rkey[:], "W9501S03")
	spot := func(string) (*SpotQuote, error) { return &SpotQuote{Bid: 1, Ask: 2, Source: "BEST"}, nil }

	reply, err := HandleW9501S03(u, spot)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// COMHDR(512) 에코 + nrec6 + 546
	if len(reply.Body) != comhdrLen+w9501s03NrecLen+w9501s02OutLen {
		t.Fatalf("COMHDR 경로 길이 %d 예상밖", len(reply.Body))
	}
	if reply.Body[comhdrEflgOff] != '0' {
		t.Errorf("eflg 미설정")
	}
}
