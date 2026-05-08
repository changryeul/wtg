package netutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCIDRs(t *testing.T) {
	cases := []struct {
		in       string
		wantLen  int
		wantErr  bool
	}{
		{"", 0, false},
		{"10.0.0.0/8", 1, false},
		{"10.0.0.0/8, 192.168.0.0/16 ,127.0.0.1/32", 3, false},
		{"  ,  ", 0, false},
		{"not-a-cidr", 0, true},
	}
	for _, c := range cases {
		got, err := ParseCIDRs(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseCIDRs(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && len(got) != c.wantLen {
			t.Errorf("ParseCIDRs(%q) len=%d want=%d", c.in, len(got), c.wantLen)
		}
	}
}

func TestIPAllowList(t *testing.T) {
	allowed, err := ParseCIDRs("127.0.0.0/8, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseCIDRs: %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	mw := IPAllowList(allowed, nil)(next)

	cases := []struct {
		remote string
		want   int
	}{
		{"127.0.0.1:1234", 204},
		{"10.1.2.3:5555", 204},
		{"192.168.1.1:1234", 403},
		{"8.8.8.8:443", 403},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/x", nil)
		req.RemoteAddr = c.remote
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, req)
		if rr.Code != c.want {
			b, _ := io.ReadAll(rr.Body)
			t.Errorf("remote=%q got=%d want=%d body=%s", c.remote, rr.Code, c.want, string(b))
		}
	}
}

func TestIPAllowList_EmptyAllowsAll(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	mw := IPAllowList(nil, nil)(next)

	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "8.8.8.8:443"
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Errorf("empty allowed should pass-through, got %d", rr.Code)
	}
}
