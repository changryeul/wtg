package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func newStoreForTest(t *testing.T) auth.Store {
	t.Helper()
	s := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { s.Close() })
	return s
}

func depsWithStore(caller Caller, store auth.Store) *Deps {
	d := quietDeps(caller)
	d.Sessions = store
	d.SessionTTL = time.Hour
	return d
}

func mkCookie(usid string) *mymq.Cookie {
	c := &mymq.Cookie{Clid: 0xCAFEBABE}
	copy(c.Usid[:], usid)
	return c
}

func TestLoginSuccess(t *testing.T) {
	store := newStoreForTest(t)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Xchg != "ADMIN" || in.Rkey != "LOGON" {
				t.Errorf("디폴트 LOGON 라우팅: xchg=%q rkey=%q", in.Xchg, in.Rkey)
			}
			if in.Cookie != nil {
				t.Error("LOGON 호출 시 cookie 미첨부여야 함")
			}
			if !strings.Contains(string(in.Body), "trader01") {
				t.Errorf("body 전달 실패: %q", in.Body)
			}
			return &mymq.Reply{
				Body:   []byte(`{"welcome":"trader01"}`),
				Cookie: mkCookie("trader01"),
			}, nil
		},
	}
	deps := depsWithStore(caller, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"data":{"usid":"trader01","password":"x"}}`))
	req.ContentLength = int64(len(`{"data":{"usid":"trader01","password":"x"}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp LoginResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.SessionID == "" {
		t.Error("session_id 누락")
	}
	if resp.ExpiresAt.IsZero() {
		t.Error("expires_at 누락")
	}
	if resp.Channel != "WEB" {
		t.Errorf("channel=%q, want WEB", resp.Channel)
	}

	// 저장된 세션 검증.
	sess, err := store.Get(context.Background(), resp.SessionID)
	if err != nil {
		t.Fatalf("session 미저장: %v", err)
	}
	if sess.Usid != "trader01" {
		t.Errorf("session.usid=%q", sess.Usid)
	}
	if sess.Cookie == nil || sess.Cookie.Clid != 0xCAFEBABE {
		t.Errorf("cookie 보관 실패: %+v", sess.Cookie)
	}
}

func TestLoginCustomExchange(t *testing.T) {
	store := newStoreForTest(t)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Xchg != "AUTH" || in.Rkey != "LOGIN_V2" {
				t.Errorf("custom 라우팅: %q/%q", in.Xchg, in.Rkey)
			}
			return &mymq.Reply{Cookie: mkCookie("u1")}, nil
		},
	}
	rr := httptest.NewRecorder()
	Login(depsWithStore(caller, store))(rr,
		httptest.NewRequest(http.MethodPost, "/v1/login",
			strings.NewReader(`{"exchange":"AUTH","routing_key":"LOGIN_V2","data":{}}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLoginEngineRejection(t *testing.T) {
	store := newStoreForTest(t)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{
				Errn:   mymq.ErrAuth,
				ErrMsg: "Authentication failed",
			}, nil
		},
	}
	rr := httptest.NewRecorder()
	Login(depsWithStore(caller, store))(rr,
		httptest.NewRequest(http.MethodPost, "/v1/login",
			strings.NewReader(`{"data":{"usid":"u","password":"bad"}}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
	// 세션 미생성 확인.
	if mem, ok := store.(*auth.MemoryStore); ok && mem.Len() != 0 {
		t.Errorf("거부 후 세션 생성됨: %d", mem.Len())
	}
}

func TestLoginNoCookieFromEngine(t *testing.T) {
	store := newStoreForTest(t)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: []byte(`{}`), Cookie: nil}, nil
		},
	}
	rr := httptest.NewRecorder()
	Login(depsWithStore(caller, store))(rr,
		httptest.NewRequest(http.MethodPost, "/v1/login",
			strings.NewReader(`{"data":{}}`)))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502", rr.Code)
	}
}

func TestLoginNoStore(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("store 가 없으면 broker 호출되면 안 됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller) // Sessions 미설정
	rr := httptest.NewRecorder()
	Login(deps)(rr, httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}

func TestLoginBadJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	Login(depsWithStore(&fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("bad json 인데 broker 호출됨")
			return nil, nil
		},
	}, newStoreForTest(t)))(rr, httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{not json`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d", rr.Code)
	}
}

// 인증 미들웨어 통과 후 logout 호출.
func doLogout(t *testing.T, deps *Deps, sid string, cookie *mymq.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := strings.NewReader(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/logout", r)
	req.ContentLength = int64(r.Len())
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:      "trader01",
		Channel:   "WEB",
		SessionID: sid,
		Cookie:    cookie,
	}))
	rr := httptest.NewRecorder()
	Logout(deps)(rr, req)
	return rr
}

func TestLogoutSuccess(t *testing.T) {
	store := newStoreForTest(t)
	store.Put(context.Background(), &auth.Session{
		ID: "sid-1", Usid: "trader01", ExpiresAt: time.Now().Add(time.Hour),
	})

	called := false
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			called = true
			if in.Xchg != "ADMIN" || in.Rkey != "LOGOFF" {
				t.Errorf("디폴트 LOGOFF: %q/%q", in.Xchg, in.Rkey)
			}
			if in.Cookie == nil {
				t.Error("LOGOFF 시 cookie 첨부되어야 함")
			}
			return &mymq.Reply{}, nil
		},
	}
	rr := doLogout(t, depsWithStore(caller, store), "sid-1", mkCookie("trader01"), "")

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !called {
		t.Error("broker LOGOFF 호출되지 않음")
	}
	if _, err := store.Get(context.Background(), "sid-1"); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("세션 삭제되지 않음: %v", err)
	}
}

// LOGOFF Call 실패해도 세션은 삭제됨 (로그아웃은 멱등).
func TestLogoutBrokerFailureStillDeletes(t *testing.T) {
	store := newStoreForTest(t)
	store.Put(context.Background(), &auth.Session{
		ID: "sid-2", Usid: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return nil, mymq.ErrReconnecting
		},
	}
	rr := doLogout(t, depsWithStore(caller, store), "sid-2", mkCookie("u"), "")
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d (broker 실패해도 200)", rr.Code)
	}
	if _, err := store.Get(context.Background(), "sid-2"); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("세션이 삭제되지 않음: %v", err)
	}
}

// LOGOFF errn 은 응답에 노출되지만 200.
func TestLogoutBrokerErrnExposed(t *testing.T) {
	store := newStoreForTest(t)
	store.Put(context.Background(), &auth.Session{
		ID: "sid-3", Usid: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Errn: mymq.ErrAuth, ErrMsg: "session expired"}, nil
		},
	}
	rr := doLogout(t, depsWithStore(caller, store), "sid-3", mkCookie("u"), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var got map[string]any
	json.NewDecoder(rr.Body).Decode(&got)
	if got["broker_errn"] == nil {
		t.Errorf("broker_errn 노출 안됨: %+v", got)
	}
}

// SessionID 없는 (DevMode) Principal 로 logout 호출 → 400.
func TestLogoutWithoutSessionID(t *testing.T) {
	deps := depsWithStore(&fakeCaller{}, newStoreForTest(t))
	req := httptest.NewRequest(http.MethodPost, "/v1/logout", strings.NewReader(""))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "trader01", // SessionID 없음
	}))
	rr := httptest.NewRecorder()
	Logout(deps)(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rr.Code)
	}
}

// 인증 통과 안 된 Logout → 401.
func TestLogoutNoPrincipal(t *testing.T) {
	deps := depsWithStore(&fakeCaller{}, newStoreForTest(t))
	rr := httptest.NewRecorder()
	Logout(deps)(rr, httptest.NewRequest(http.MethodPost, "/v1/logout", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}
