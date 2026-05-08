package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCaller — Caller mock.
type fakeCaller struct {
	reply func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error)
	last  *mymq.FrameInput
}

func (f *fakeCaller) Call(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
	f.last = in
	return f.reply(ctx, in)
}

func newDeps(c *fakeCaller) *HandlerDeps {
	return &HandlerDeps{
		MQ:          c,
		CallTimeout: 1 * time.Second,
		Logger:      quietLogger(),
	}
}

func TestAdminCmdSuccess(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Func != mymq.FCAdmin {
				t.Errorf("Func: %d, want FCAdmin(3)", in.Func)
			}
			if in.Subc != mymq.SubGetStatus {
				t.Errorf("Subc: %d", in.Subc)
			}
			if in.Dirf != mymq.DirIoctl {
				t.Errorf("Dirf: %d, want DirIoctl(0)", in.Dirf)
			}
			// admin 명령은 Xchg/Rkey 비어있어야 (자동 navi 채움 안 됨).
			if in.Xchg != "" || in.Rkey != "" {
				t.Errorf("Xchg/Rkey 비어있어야: %q/%q", in.Xchg, in.Rkey)
			}
			return &mymq.Reply{Body: []byte(`{"clients":42,"uptime_s":3600}`)}, nil
		},
	}
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"subc":150}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/cmd", body)
	AdminCmd(newDeps(caller))(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var env AdminCmdResponse
	_ = json.NewDecoder(rr.Body).Decode(&env)
	if env.Errn != 0 {
		t.Errorf("Errn: %d", env.Errn)
	}
	var data map[string]int
	_ = json.Unmarshal(env.Data, &data)
	if data["clients"] != 42 {
		t.Errorf("data.clients: %v", data["clients"])
	}
}

func TestAdminCmdMissingSubc(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/cmd", strings.NewReader(`{}`))
	AdminCmd(newDeps(&fakeCaller{reply: func(_ context.Context, _ *mymq.FrameInput) (*mymq.Reply, error) {
		t.Error("subc 없을 때 Call 호출되면 안 됨")
		return nil, nil
	}}))(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestAdminCmdBadJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/cmd", strings.NewReader(`{not json`))
	AdminCmd(newDeps(&fakeCaller{}))(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestShortcuts(t *testing.T) {
	cases := []struct {
		name     string
		handler  func(*HandlerDeps) http.HandlerFunc
		wantSubc mymq.Subc
		path     string
	}{
		{"status", GetStatus, mymq.SubGetStatus, "/v1/admin/status"},
		{"clients", GetClients, mymq.SubGetClient, "/v1/admin/clients"},
		{"exchanges", GetExchanges, mymq.SubGetExchange, "/v1/admin/exchanges"},
		{"users", GetUsers, mymq.SubGetUsers, "/v1/admin/users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caller := &fakeCaller{
				reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
					if in.Subc != c.wantSubc {
						t.Errorf("Subc: %d, want %d", in.Subc, c.wantSubc)
					}
					return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
				},
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			c.handler(newDeps(caller))(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("status: %d", rr.Code)
			}
		})
	}
}

func TestGetWhoisRequiresUsid(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/whois", nil) // usid 없음
	GetWhois(newDeps(&fakeCaller{}))(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rr.Code)
	}
}

func TestGetWhoisForwardsUsid(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			var got map[string]string
			_ = json.Unmarshal(in.Body, &got)
			if got["usid"] != "trader01" {
				t.Errorf("body.usid: %v", got["usid"])
			}
			return &mymq.Reply{Body: []byte(`{"chan":"WEB","ip":"1.2.3.4"}`)}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/whois?usid=trader01", nil)
	GetWhois(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestBrokerErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"timeout", mymq.ErrTimeoutErr, http.StatusGatewayTimeout},
		{"reconnecting", mymq.ErrReconnecting, http.StatusServiceUnavailable},
		{"closed", mymq.ErrClientClosed, http.StatusServiceUnavailable},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"broker_err", &mymq.Error{Errn: mymq.ErrAuth, Msg: "denied"}, http.StatusUnprocessableEntity},
		{"unknown", errors.New("network"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			caller := &fakeCaller{
				reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
					return nil, c.err
				},
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/admin/status", nil)
			GetStatus(newDeps(caller))(rr, req)
			if rr.Code != c.status {
				t.Errorf("status: %d, want %d", rr.Code, c.status)
			}
		})
	}
}

func TestPing(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	PingHandler()(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got["service"] != "mci-admin" {
		t.Errorf("service: %q", got["service"])
	}
}

// SessionMode 인증을 통과한 admin 명령은 cookie_t 가 첨부된다.
func TestAdminCmdAttachesCookie(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Cookie == nil || in.Cookie.Clid != 0xCAFE {
				t.Errorf("admin 명령 cookie 첨부 실패: %+v", in.Cookie)
			}
			return &mymq.Reply{}, nil
		},
	}
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"subc":150}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/cmd", body)
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:    "admin01",
		Channel: "ADMIN",
		Cookie:  &mymq.Cookie{Clid: 0xCAFE},
	}))
	AdminCmd(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

// DevMode (Cookie nil) 에서는 admin 명령에도 cookie 미첨부.
func TestAdminCmdNoCookieInDevMode(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Cookie != nil {
				t.Errorf("DevMode 인데 cookie 첨부됨: %+v", in.Cookie)
			}
			return &mymq.Reply{}, nil
		},
	}
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"subc":150}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/cmd", body)
	// Principal 없음 (DevMode 도 아니고 인증 미통과 — 미들웨어가 401 처리하지만,
	// 핸들러 단독 호출은 cookie 미첨부만 확인).
	AdminCmd(newDeps(caller))(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
}
