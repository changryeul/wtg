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

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func mkJWTDeps(t *testing.T, caller Caller) (*Deps, *auth.Verifier) {
	t.Helper()
	priv, err := auth.GenerateRSAKeyPair(2048)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: priv})
	if err != nil {
		t.Fatal(err)
	}
	ver, err := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })
	rstore := auth.NewMemoryRefreshStore(auth.MemoryRefreshStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { rstore.Close() })

	d := quietDeps(caller)
	d.Sessions = store
	d.RefreshStore = rstore
	d.JWTIssuer = iss
	d.AccessTokenTTL = 15 * time.Minute
	d.RefreshTokenTTL = 8 * time.Hour
	return d, ver
}

func TestLoginIssuesJWTAndRefresh(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			c := &mymq.Cookie{Clid: 0xCAFEBABE}
			copy(c.Usid[:], "trader01")
			return &mymq.Reply{Cookie: c}, nil
		},
	}
	deps, ver := mkJWTDeps(t, caller)

	rr := httptest.NewRecorder()
	body := `{"data":{"usid":"trader01"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Login(deps)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp LoginResponse
	json.NewDecoder(rr.Body).Decode(&resp)

	if resp.AccessToken == "" {
		t.Error("access_token 누락")
	}
	if resp.RefreshToken == "" {
		t.Error("refresh_token 누락")
	}
	if resp.SessionID == "" {
		t.Error("session_id 누락")
	}

	// access JWT 검증.
	claims, err := ver.Verify(resp.AccessToken)
	if err != nil {
		t.Fatalf("발급된 JWT 검증 실패: %v", err)
	}
	if claims.SID != resp.SessionID {
		t.Errorf("SID 불일치: jwt.SID=%q resp.SessionID=%q", claims.SID, resp.SessionID)
	}
	if claims.Usid != "trader01" {
		t.Errorf("Usid: %q", claims.Usid)
	}
}

// /v1/refresh — refresh token 으로 새 access + refresh 발급.
func TestRefreshHappy(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			c := &mymq.Cookie{Clid: 0xBEEF}
			copy(c.Usid[:], "u1")
			return &mymq.Reply{Cookie: c}, nil
		},
	}
	deps, ver := mkJWTDeps(t, caller)

	// 먼저 login → refresh 받기.
	loginRR := httptest.NewRecorder()
	body := `{"data":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Login(deps)(loginRR, req)
	if loginRR.Code != http.StatusOK {
		t.Fatalf("login: %d %s", loginRR.Code, loginRR.Body.String())
	}
	var loginResp LoginResponse
	json.NewDecoder(loginRR.Body).Decode(&loginResp)

	// refresh 호출.
	rbody, _ := json.Marshal(RefreshRequest{RefreshToken: loginResp.RefreshToken})
	rr := httptest.NewRecorder()
	rreq := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(string(rbody)))
	rreq.ContentLength = int64(len(rbody))
	Refresh(deps)(rr, rreq)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", rr.Code, rr.Body.String())
	}

	var refResp RefreshResponse
	json.NewDecoder(rr.Body).Decode(&refResp)
	if refResp.AccessToken == "" || refResp.RefreshToken == "" {
		t.Errorf("토큰 페어 누락: %+v", refResp)
	}
	if refResp.RefreshToken == loginResp.RefreshToken {
		t.Error("refresh rotation 안됨 — 동일 토큰 재발급")
	}

	// 새 access JWT 가 정상 검증되는지.
	if _, err := ver.Verify(refResp.AccessToken); err != nil {
		t.Errorf("새 access 검증 실패: %v", err)
	}

	// 옛 refresh 는 single-use — 재사용 거부.
	rbody2, _ := json.Marshal(RefreshRequest{RefreshToken: loginResp.RefreshToken})
	rr2 := httptest.NewRecorder()
	rreq2 := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(string(rbody2)))
	rreq2.ContentLength = int64(len(rbody2))
	Refresh(deps)(rr2, rreq2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("재사용 status=%d, want 401", rr2.Code)
	}
}

// 미존재 refresh → 401.
func TestRefreshUnknownToken(t *testing.T) {
	deps, _ := mkJWTDeps(t, &fakeCaller{})
	body := `{"refresh_token":"nope"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Refresh(deps)(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// 만료된 refresh → 401.
func TestRefreshExpiredToken(t *testing.T) {
	deps, _ := mkJWTDeps(t, &fakeCaller{})
	// 만료된 refresh 직접 주입.
	deps.RefreshStore.Put(context.Background(), &auth.RefreshToken{
		Token: "expired-rt", SID: "sess",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	body := `{"refresh_token":"expired-rt"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Refresh(deps)(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// 세션이 사라진 (logout 등) refresh → 401.
func TestRefreshSessionGone(t *testing.T) {
	deps, _ := mkJWTDeps(t, &fakeCaller{})
	// refresh 만 등록 — 세션 미등록.
	deps.RefreshStore.Put(context.Background(), &auth.RefreshToken{
		Token: "rt-orphan", SID: "ghost-sess",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	body := `{"refresh_token":"rt-orphan"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Refresh(deps)(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "session_gone") {
		t.Errorf("session_gone 메시지 없음: %s", rr.Body.String())
	}
}

// 미구성 — 503.
func TestRefreshUnconfigured(t *testing.T) {
	deps := quietDeps(nil) // RefreshStore/JWTIssuer/Sessions 모두 nil
	body := `{"refresh_token":"x"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/refresh", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	Refresh(deps)(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}

// Logout 이 RefreshStore 도 청소하는지.
func TestLogoutRevokesRefresh(t *testing.T) {
	deps, _ := mkJWTDeps(t, &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{}, nil
		},
	})
	// 세션 + refresh 직접 주입.
	deps.Sessions.Put(context.Background(), &auth.Session{
		ID: "sid-x", Usid: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	deps.RefreshStore.Put(context.Background(), &auth.RefreshToken{
		Token: "rt-1", SID: "sid-x", ExpiresAt: time.Now().Add(time.Hour),
	})

	rr := doLogout(t, deps, "sid-x", &mymq.Cookie{Clid: 1}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("logout: %d", rr.Code)
	}
	// 같은 SID 의 refresh 가 사라졌는지.
	if _, err := deps.RefreshStore.Consume(context.Background(), "rt-1"); !errors.Is(err, auth.ErrRefreshNotFound) {
		t.Errorf("logout 후 refresh 가 남음: %v", err)
	}
}
