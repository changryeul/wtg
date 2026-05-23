package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/session"
)

func mkTestCaller(usid string) *fakeCaller {
	return &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{
				Body:   []byte(`{"ok":true}`),
				Cookie: mkCookie(usid),
			}, nil
		},
	}
}

// 권위 출처 — UserProfileResolver 가 반환한 Site/Tier 가 Session.Profile 에 저장.
func TestLogin_ResolverProfileSavedToSession(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)
	resolver := auth.NewStaticResolver()
	resolver.Set("CRLEE", auth.UserProfile{Site: session.SiteBranch, Tier: session.TierVIP})
	deps.UserProfiles = resolver

	body := `{"data":{},"channel":"WEB"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var resp LoginResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	sess, err := store.Get(context.Background(), resp.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := session.Profile{
		Channel: session.ChannelWeb,
		Site:    session.SiteBranch,
		Tier:    session.TierVIP,
	}
	if sess.Profile != want {
		t.Errorf("Profile = %+v, want %+v", sess.Profile, want)
	}
}

// 클라이언트가 body 로 보낸 site/tier 는 무시 (보안 — resolver 미설정 시 빈 값).
func TestLogin_IgnoresBodySiteTier(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)
	// resolver 미설정 → site/tier 빈 값이어야.

	body := `{"data":{},"channel":"WEB","site":"BRANCH","tier":"VIP"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var resp LoginResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	sess, _ := store.Get(context.Background(), resp.SessionID)

	if sess.Profile.Site != "" || sess.Profile.Tier != "" {
		t.Errorf("body 의 site/tier 가 반영됨 (보안 위반): %+v", sess.Profile)
	}
}

// 발급된 JWT 가 resolver 의 Site/Tier 를 claim 으로 포함.
func TestLogin_JWT_FromResolver(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)
	resolver := auth.NewStaticResolver()
	resolver.Set("CRLEE", auth.UserProfile{Site: session.SiteHQ, Tier: session.TierGold})
	deps.UserProfiles = resolver

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: key})
	deps.JWTIssuer = issuer
	deps.AccessTokenTTL = time.Hour
	verifier, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &key.PublicKey}})

	body := `{"data":{},"channel":"MOB"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var resp LoginResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	claims, err := verifier.Verify(resp.AccessToken)
	if err != nil {
		t.Fatalf("JWT verify: %v", err)
	}
	if claims.Chan != "MOB" || claims.Site != "HQ" || claims.Tier != "GOLD" {
		t.Errorf("claims chan/site/tier = %q/%q/%q", claims.Chan, claims.Site, claims.Tier)
	}
}

// resolver 등록 안 된 usid → 빈 Site/Tier 로 fallback (로그인 자체는 성공).
func TestLogin_UnknownUser_EmptyProfile(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)
	resolver := auth.NewStaticResolver()
	// CRLEE 등록 안 함.
	deps.UserProfiles = resolver

	body := `{"data":{},"channel":"WEB"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d (미등록은 fallback 으로 통과해야)", rr.Code)
	}

	var resp LoginResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	sess, _ := store.Get(context.Background(), resp.SessionID)

	if sess.Profile.Site != "" || sess.Profile.Tier != "" {
		t.Errorf("미등록 usid 인데 Site/Tier 채워짐: %+v", sess.Profile)
	}
}
