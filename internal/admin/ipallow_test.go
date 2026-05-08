package admin

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIPAllowListEmpty(t *testing.T) {
	// 빈 allow list → 모두 허용.
	called := false
	mw := IPAllowList(nil, quietLogger())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !called {
		t.Error("빈 allow list 면 모두 통과해야 함")
	}
}

func TestIPAllowListMatch(t *testing.T) {
	allowed := []*net.IPNet{
		mustCIDR(t, "10.0.0.0/8"),
		mustCIDR(t, "127.0.0.0/8"),
	}
	mw := IPAllowList(allowed, quietLogger())

	cases := []struct {
		remote string
		ok     bool
	}{
		{"10.5.5.5:1000", true},
		{"127.0.0.1:1000", true},
		{"192.168.1.1:1000", false},
		{"8.8.8.8:1000", false},
	}
	for _, c := range cases {
		t.Run(c.remote, func(t *testing.T) {
			called := false
			h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = c.remote
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if called != c.ok {
				t.Errorf("remote=%s: called=%v, want %v", c.remote, called, c.ok)
			}
			if !c.ok && rr.Code != http.StatusForbidden {
				t.Errorf("거부 시 403 기대, got %d", rr.Code)
			}
		})
	}
}

func TestIPAllowListIPv6(t *testing.T) {
	allowed := []*net.IPNet{
		mustCIDR(t, "fd00::/8"),
	}
	mw := IPAllowList(allowed, quietLogger())
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[fd00::1]:5000"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !called {
		t.Errorf("IPv6 매칭 실패: status=%d", rr.Code)
	}
}

func TestIPAllowListBadRemote(t *testing.T) {
	allowed := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	mw := IPAllowList(allowed, quietLogger())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("파싱 불가 시 통과되면 안 됨")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "garbage"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status: %d, want 403", rr.Code)
	}
}

func TestParseCIDRsValid(t *testing.T) {
	nets, err := parseCIDRs("10.0.0.0/8, 127.0.0.0/8 , 192.168.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 3 {
		t.Errorf("count: %d", len(nets))
	}
}

func TestParseCIDRsInvalid(t *testing.T) {
	if _, err := parseCIDRs("10.0.0.0/8,bad"); err == nil {
		t.Error("invalid CIDR 에 에러 기대")
	}
}
