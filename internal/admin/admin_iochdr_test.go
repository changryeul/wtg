package admin

import (
	"encoding/binary"
	"net/http/httptest"
	"testing"
)

func TestEncodeDecodeIochdrRoundTrip(t *testing.T) {
	in := iochdrQuery{
		Argv: [4]string{"trader*", "NEW", "ORDER_Q", ""},
		Page: 2,
		Nofp: 50,
	}
	buf := encodeIochdr(in)
	if len(buf) != iochdrSize {
		t.Fatalf("encode len: %d, want %d", len(buf), iochdrSize)
	}
	// argv 들이 0..63 에 NUL-padded 로 들어가는지
	if string(buf[0:7]) != "trader*" {
		t.Errorf("argv[0]: %q", buf[0:7])
	}
	if string(buf[16:19]) != "NEW" {
		t.Errorf("argv[1]: %q", buf[16:19])
	}
	if string(buf[32:39]) != "ORDER_Q" {
		t.Errorf("argv[2]: %q", buf[32:39])
	}
	// nofp/page off 68/76, BE
	if got := binary.BigEndian.Uint32(buf[68:]); got != 50 {
		t.Errorf("nofp: %d, want 50", got)
	}
	if got := binary.BigEndian.Uint32(buf[76:]); got != 2 {
		t.Errorf("page: %d, want 2", got)
	}

	// broker 가 응답에서 채울 maxr/many/next 시뮬레이션
	binary.BigEndian.PutUint32(buf[64:], 17) // maxr
	binary.BigEndian.PutUint32(buf[72:], 5)  // many
	binary.BigEndian.PutUint32(buf[80:], 1)  // next

	out, err := decodeIochdr(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Maxr != 17 || out.Nofp != 50 || out.Many != 5 || out.Page != 2 || out.Next != 1 {
		t.Errorf("decoded: %+v", out)
	}
}

func TestParseIochdrQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/?argv0=trader*&argv1=NEW&page=3&nofp=20", nil)
	q := parseIochdrQuery(req)
	if q.Argv[0] != "trader*" || q.Argv[1] != "NEW" || q.Argv[2] != "" || q.Argv[3] != "" {
		t.Errorf("argv: %v", q.Argv)
	}
	if q.Page != 3 || q.Nofp != 20 {
		t.Errorf("page=%d nofp=%d", q.Page, q.Nofp)
	}
}

func TestParseIochdrQueryArgvTruncate(t *testing.T) {
	long := "abcdefghijklmnop_extra" // 22자
	req := httptest.NewRequest("GET", "/?argv0="+long, nil)
	q := parseIochdrQuery(req)
	if len(q.Argv[0]) != 15 {
		t.Errorf("argv[0] 길이: %d, want 15 (16바이트 - NUL 1)", len(q.Argv[0]))
	}
}

func TestDecodeIochdrShortBuffer(t *testing.T) {
	if _, err := decodeIochdr(make([]byte, iochdrSize-1)); err == nil {
		t.Errorf("짧은 buffer: nil 에러 — want errShortBody")
	}
}
