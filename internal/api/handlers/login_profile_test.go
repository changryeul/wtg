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

// 로그인 시 Site/Tier 가 Session.Profile 에 저장되는지.
func TestLogin_SavesProfileToSession(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)

	body := `{"data":{},"channel":"WEB","site":"BRANCH","tier":"VIP"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var resp LoginResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
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
	if sess.LogonID == "" {
		t.Error("LogonID 가 비어있음")
	}
}

// 로그인 발급된 JWT 가 site/tier 를 claim 으로 포함하는지.
func TestLogin_JWT_IncludesSiteTier(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)

	// JWTIssuer/Verifier 구성.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: key})
	deps.JWTIssuer = issuer
	deps.AccessTokenTTL = time.Hour
	verifier, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &key.PublicKey}})

	body := `{"data":{},"channel":"MOB","site":"HQ","tier":"GOLD"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(body))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var resp LoginResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.AccessToken == "" {
		t.Fatal("access_token 미발급")
	}
	claims, err := verifier.Verify(resp.AccessToken)
	if err != nil {
		t.Fatalf("JWT verify: %v", err)
	}
	if claims.Chan != "MOB" || claims.Site != "HQ" || claims.Tier != "GOLD" {
		t.Errorf("claims chan/site/tier = %q/%q/%q", claims.Chan, claims.Site, claims.Tier)
	}
	if claims.ProfileKey() != "MOB.HQ.GOLD" {
		t.Errorf("ProfileKey = %q", claims.ProfileKey())
	}
}

// Site/Tier 미지정 시 빈 값으로 저장되어 ProfileKey 가 빈 문자열.
func TestLogin_NoSiteTier_LeavesProfileEmpty(t *testing.T) {
	store := newStoreForTest(t)
	deps := depsWithStore(mkTestCaller("CRLEE"), store)

	req := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"data":{}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}

	var resp LoginResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	sess, _ := store.Get(context.Background(), resp.SessionID)

	if sess.Profile.Site != "" || sess.Profile.Tier != "" {
		t.Errorf("Site/Tier 가 채워짐: %+v", sess.Profile)
	}
}
