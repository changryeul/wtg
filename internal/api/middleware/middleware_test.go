package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRequestIDInjects(t *testing.T) {
	mw := RequestID()
	var seenID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	h.ServeHTTP(rr, req)

	if seenID == "" {
		t.Error("핸들러 context 에 request id 가 없음")
	}
	if rr.Header().Get("X-Request-ID") != seenID {
		t.Error("응답 헤더 X-Request-ID 가 다르거나 없음")
	}
}

func TestRequestIDPreservesIncoming(t *testing.T) {
	mw := RequestID()
	var seenID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "client-supplied-123")
	h.ServeHTTP(rr, req)

	if seenID != "client-supplied-123" {
		t.Errorf("기존 X-Request-ID 보존 실패: %q", seenID)
	}
}

func TestRecoverConvertsPanicTo500(t *testing.T) {
	mw := Recover(discardLogger())
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "internal server error") {
		t.Errorf("body: %q", rr.Body.String())
	}
}

func TestAuthDevModeAcceptsHeader(t *testing.T) {
	mw := Auth(AuthConfig{DevMode: true, Logger: discardLogger()})
	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("X-WTG-User", "trader01")
	h.ServeHTTP(rr, req)

	if p == nil || p.Usid != "trader01" {
		t.Errorf("Principal: %+v", p)
	}
}

func TestAuthDevModeRequiresHeader(t *testing.T) {
	mw := Auth(AuthConfig{DevMode: true, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러가 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rr.Code)
	}
}

func TestAuthPublicPathBypass(t *testing.T) {
	mw := Auth(AuthConfig{DevMode: false, Logger: discardLogger()})
	for _, path := range []string{"/v1/ping", "/healthz", "/readyz", "/metrics", "/v1/login", "/v1/refresh"} {
		called := false
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if !called {
			t.Errorf("%s 는 인증 우회되어야 함 (status=%d)", path, rr.Code)
		}
	}
}

func TestChainComposesInOuterToInnerOrder(t *testing.T) {
	var trace []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace = append(trace, name+":enter")
				next.ServeHTTP(w, r)
				trace = append(trace, name+":exit")
			})
		}
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
	})
	chain := Chain(inner, mw("a"), mw("b"), mw("c"))
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"c:enter", "b:enter", "a:enter", "handler", "a:exit", "b:exit", "c:exit"}
	if strings.Join(trace, ",") != strings.Join(want, ",") {
		t.Errorf("실행 순서:\n got: %v\nwant: %v", trace, want)
	}
}

// principalRequired 동작 검증 — 인증 미들웨어 통과 후에만 허용.
func TestPrincipalFromEmptyContext(t *testing.T) {
	if p := PrincipalFromContext(context.Background()); p != nil {
		t.Errorf("빈 context 에서 Principal 추출되면 안 됨: %+v", p)
	}
}

// SessionMode — Authorization: Bearer <session_id> 흐름.
func newSessionStoreWith(t *testing.T, sess *auth.Session) auth.Store {
	t.Helper()
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return store
}

func TestAuthSessionModeAcceptsBearer(t *testing.T) {
	store := newSessionStoreWith(t, &auth.Session{
		ID:        "sess-abc",
		Usid:      "trader01",
		Channel:   "WEB",
		Cookie:    &mymq.Cookie{Clid: 0xCAFE},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	mw := Auth(AuthConfig{SessionStore: store, Logger: discardLogger()})

	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer sess-abc")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if p == nil {
		t.Fatal("Principal 미주입")
	}
	if p.Usid != "trader01" || p.SessionID != "sess-abc" {
		t.Errorf("Principal: %+v", p)
	}
	if p.Cookie == nil || p.Cookie.Clid != 0xCAFE {
		t.Errorf("Cookie 복원 실패: %+v", p.Cookie)
	}
}

func TestAuthSessionModeRejectsMissingHeader(t *testing.T) {
	store := newSessionStoreWith(t, &auth.Session{
		ID: "sess-abc", Usid: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	mw := Auth(AuthConfig{SessionStore: store, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러가 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/orders", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rr.Code)
	}
}

func TestAuthSessionModeRejectsBadScheme(t *testing.T) {
	store := newSessionStoreWith(t, &auth.Session{
		ID: "sess-abc", Usid: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	mw := Auth(AuthConfig{SessionStore: store, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러가 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rr.Code)
	}
}

func TestAuthSessionModeRejectsUnknownToken(t *testing.T) {
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })

	mw := Auth(AuthConfig{SessionStore: store, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러가 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer ghost")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "재로그인") {
		t.Errorf("재로그인 메시지 없음: %s", rr.Body.String())
	}
}

func TestAuthSessionModeRejectsExpired(t *testing.T) {
	store := newSessionStoreWith(t, &auth.Session{
		ID:        "sess-old",
		Usid:      "u",
		ExpiresAt: time.Now().Add(-time.Minute), // 이미 만료
	})
	mw := Auth(AuthConfig{SessionStore: store, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러가 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer sess-old")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rr.Code)
	}
}

// JWTVerifier 가 SessionStore 보다 우선 — 둘 다 있을 때 JWT 경로.
func TestAuthJWTModeAcceptsValidToken(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	iss, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})

	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })
	store.Put(context.Background(), &auth.Session{
		ID: "sess-jwt", Usid: "trader01", Channel: "WEB",
		Cookie: nil, ExpiresAt: time.Now().Add(time.Hour),
	})

	tok, err := iss.Sign(auth.Claims{
		SID:  "sess-jwt",
		Usid: "trader01",
		EXP:  time.Now().Add(15 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	mw := Auth(AuthConfig{
		SessionStore: store,
		JWTVerifier:  ver,
		Logger:       discardLogger(),
	})
	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if p == nil || p.SessionID != "sess-jwt" || p.Usid != "trader01" {
		t.Errorf("Principal: %+v", p)
	}
}

func TestAuthJWTModeRejectsBadToken(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })

	mw := Auth(AuthConfig{
		SessionStore: store,
		JWTVerifier:  ver,
		Logger:       discardLogger(),
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("핸들러 호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer invalid.jwt.token")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// JWT-only 모드 (DMZ edge) — SessionStore 가 nil 이어도 claim 만으로 Principal.
func TestAuthJWTModeWithoutSessionStore(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	iss, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k", PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	tok, _ := iss.Sign(auth.Claims{
		SID: "sess-edge", Usid: "trader01", Chan: "WEB",
		EXP: time.Now().Add(time.Hour).Unix(),
	})

	mw := Auth(AuthConfig{JWTVerifier: ver /* SessionStore: nil */, Logger: discardLogger()})
	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	if p == nil || p.Usid != "trader01" || p.SessionID != "sess-edge" || p.Channel != "WEB" {
		t.Errorf("Principal: %+v", p)
	}
	if p.Cookie != nil {
		t.Errorf("edge 모드인데 cookie 채워짐: %+v", p.Cookie)
	}
}

// TrustEdgeHeaders — X-WTG-SID 헤더 → SessionStore 조회.
func TestAuthTrustEdgeHeaders(t *testing.T) {
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })
	store.Put(context.Background(), &auth.Session{
		ID: "sess-edge-trust", Usid: "trader01", Channel: "WEB",
		Cookie: nil, ExpiresAt: time.Now().Add(time.Hour),
	})

	mw := Auth(AuthConfig{
		SessionStore:     store,
		TrustEdgeHeaders: true,
		Logger:           discardLogger(),
	})
	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set(HeaderEdgeSID, "sess-edge-trust")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if p == nil || p.SessionID != "sess-edge-trust" || p.Usid != "trader01" {
		t.Errorf("Principal: %+v", p)
	}
}

// TrustEdgeHeaders 인데 SID 헤더 없음 → 401.
func TestAuthTrustEdgeHeadersMissing(t *testing.T) {
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })

	mw := Auth(AuthConfig{
		SessionStore:     store,
		TrustEdgeHeaders: true,
		Logger:           discardLogger(),
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/x", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// JWT 가 유효해도 SID 가 SessionStore 에 없으면 401 (logout 후 stale JWT).
func TestAuthJWTModeRejectsMissingSession(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	iss, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k", PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{SweepInterval: time.Hour})
	t.Cleanup(func() { store.Close() })

	tok, _ := iss.Sign(auth.Claims{SID: "ghost-sess", EXP: time.Now().Add(time.Hour).Unix()})
	mw := Auth(AuthConfig{SessionStore: store, JWTVerifier: ver, Logger: discardLogger()})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("호출되면 안 됨")
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", rr.Code)
	}
}

// DevMode 가 SessionStore 보다 우선 — 둘 다 줘도 헤더 모드.
func TestAuthDevModeOverridesSessionStore(t *testing.T) {
	store := newSessionStoreWith(t, &auth.Session{
		ID: "sess-abc", Usid: "from-session", ExpiresAt: time.Now().Add(time.Hour),
	})
	mw := Auth(AuthConfig{DevMode: true, SessionStore: store, Logger: discardLogger()})

	var p *Principal
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = PrincipalFromContext(r.Context())
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("X-WTG-User", "from-header")
	req.Header.Set("Authorization", "Bearer sess-abc") // 무시되어야 함
	h.ServeHTTP(rr, req)

	if p == nil || p.Usid != "from-header" {
		t.Errorf("DevMode 우선순위 실패: %+v", p)
	}
}
