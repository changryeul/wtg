package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
)

// 정상 흐름: 클라이언트 JWT → edge 검증 → upstream 에 X-WTG-SID/User/Channel 주입.
func TestEdgeJWTVerifyAndInjectHeaders(t *testing.T) {
	priv, err := auth.GenerateRSAKeyPair(2048)
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})

	tok, err := iss.Sign(auth.Claims{
		SID:  "sess-123",
		Usid: "trader01",
		Chan: "WEB",
		EXP:  time.Now().Add(15 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var seenSID, seenUser, seenChan, seenAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSID.Store(r.Header.Get(middleware.HeaderEdgeSID))
		seenUser.Store(r.Header.Get(middleware.HeaderEdgeUser))
		seenChan.Store(r.Header.Get(middleware.HeaderEdgeChannel))
		seenAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	cfg.MaxRequestBody = 0
	srv := NewServer(cfg, quietLogger())
	srv.SetJWTVerifier(ver)
	handler, err := srv.BuildHandler()
	if err != nil {
		t.Fatal(err)
	}
	edgeSrv := httptest.NewServer(handler)
	defer edgeSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, edgeSrv.URL+"/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if got, _ := seenSID.Load().(string); got != "sess-123" {
		t.Errorf("upstream X-WTG-SID=%q, want sess-123", got)
	}
	if got, _ := seenUser.Load().(string); got != "trader01" {
		t.Errorf("upstream X-WTG-User=%q", got)
	}
	if got, _ := seenChan.Load().(string); got != "WEB" {
		t.Errorf("upstream X-WTG-Channel=%q", got)
	}
	if got, _ := seenAuth.Load().(string); got != "" {
		t.Errorf("Authorization 헤더가 그대로 forward 됨 (sensitive 제거 실패): %q", got)
	}
}

// 외부 클라이언트가 X-WTG-SID 헤더를 위조해서 보낸 경우 — 그 헤더는 무조건
// 제거되고, 인증은 별도 (Authorization) 로만 결정된다.
func TestEdgeStripsForgedXWTGHeaders(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	iss, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	tok, _ := iss.Sign(auth.Claims{
		SID: "real-sid", Usid: "real-user",
		EXP: time.Now().Add(time.Hour).Unix(),
	})

	var seenSID, seenUser atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenSID.Store(r.Header.Get(middleware.HeaderEdgeSID))
		seenUser.Store(r.Header.Get(middleware.HeaderEdgeUser))
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	cfg.MaxRequestBody = 0
	srv := NewServer(cfg, quietLogger())
	srv.SetJWTVerifier(ver)
	handler, _ := srv.BuildHandler()
	edgeSrv := httptest.NewServer(handler)
	defer edgeSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, edgeSrv.URL+"/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	// 클라이언트가 자기 마음대로 위조한 신뢰 헤더.
	req.Header.Set(middleware.HeaderEdgeSID, "FORGED-SID")
	req.Header.Set(middleware.HeaderEdgeUser, "FORGED-USER")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// upstream 이 본 헤더는 위조값이 아니라 edge 가 검증해서 채운 값이어야.
	if got, _ := seenSID.Load().(string); got != "real-sid" {
		t.Errorf("위조 SID 가 통과: %q", got)
	}
	if got, _ := seenUser.Load().(string); got != "real-user" {
		t.Errorf("위조 User 가 통과: %q", got)
	}
}

// JWT 가 잘못되면 edge 가 401, upstream 호출 안 됨.
func TestEdgeJWTRejectsBadToken(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})

	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	srv := NewServer(cfg, quietLogger())
	srv.SetJWTVerifier(ver)
	handler, _ := srv.BuildHandler()
	edgeSrv := httptest.NewServer(handler)
	defer edgeSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, edgeSrv.URL+"/v1/x", nil)
	req.Header.Set("Authorization", "Bearer not.a.real.jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
	if upstreamCalled {
		t.Error("invalid JWT 인데 upstream 호출됨")
	}
}

// 보조: upstream 응답 본문이 그대로 전달되는지.
func TestEdgeJWTBodyPassthrough(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	iss, _ := auth.NewIssuer(auth.IssuerOptions{PrivateKey: priv})
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})
	tok, _ := iss.Sign(auth.Claims{SID: "s", Usid: "u", EXP: time.Now().Add(time.Hour).Unix()})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hello":"world"}`))
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	srv := NewServer(cfg, quietLogger())
	srv.SetJWTVerifier(ver)
	handler, _ := srv.BuildHandler()
	edgeSrv := httptest.NewServer(handler)
	defer edgeSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, edgeSrv.URL+"/v1/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io_ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hello":"world"`) {
		t.Errorf("body passthrough 실패: %s", body)
	}
}

// io.ReadAll 호환 — 별도 import 회피.
func io_ReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var out []byte
	buf := make([]byte, 1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return out, nil
			}
			return out, err
		}
	}
}

// /v1/login /v1/refresh 은 인증 bootstrap — Principal 없이도 upstream 으로
// forward 되어야 한다 (그렇지 않으면 edge 로는 로그인 자체가 불가능한 닭-달걀).
// 단 그 외 경로는 여전히 Principal 필수.
func TestEdgeForwardsLoginWithoutAuth(t *testing.T) {
	priv, _ := auth.GenerateRSAKeyPair(2048)
	ver, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &priv.PublicKey}})

	var hits atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Store(r.URL.Path)
		w.Write([]byte(`{"session_id":"s"}`))
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	cfg.MaxRequestBody = 0
	srv := NewServer(cfg, quietLogger())
	srv.SetJWTVerifier(ver)
	handler, _ := srv.BuildHandler()
	edgeSrv := httptest.NewServer(handler)
	defer edgeSrv.Close()

	for _, path := range []string{"/v1/login", "/v1/refresh"} {
		hits.Store("")
		resp, err := http.Post(edgeSrv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status=%d, want 200 (무인증 forward)", path, resp.StatusCode)
		}
		if hits.Load() != path {
			t.Errorf("%s: upstream 도달 실패 (hit=%q)", path, hits.Load())
		}
	}

	// 그 외 경로는 여전히 인증 필수 — upstream 에 도달하면 안 됨.
	hits.Store("")
	resp, _ := http.Post(edgeSrv.URL+"/v1/tx", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/v1/tx 무인증: status=%d, want 401", resp.StatusCode)
	}
	if hits.Load() != "" {
		t.Errorf("/v1/tx 무인증인데 upstream 도달: %q", hits.Load())
	}
}
