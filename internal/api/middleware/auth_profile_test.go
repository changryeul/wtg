package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/session"
)

func TestPrincipal_ProfileKey(t *testing.T) {
	tests := []struct {
		name string
		p    *Principal
		want string
	}{
		{"nil", nil, ""},
		{"all set", &Principal{Channel: "WEB", Site: "BRANCH", Tier: "VIP"}, "WEB.BRANCH.VIP"},
		{"missing site", &Principal{Channel: "WEB", Tier: "VIP"}, ""},
		{"missing tier", &Principal{Channel: "WEB", Site: "HQ"}, ""},
		{"missing channel", &Principal{Site: "BRANCH", Tier: "STD"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.ProfileKey(); got != tc.want {
				t.Errorf("ProfileKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// DevMode 에서 X-WTG-Site/Tier 헤더가 Principal 에 반영되는지.
func TestAuth_DevMode_SiteTierHeaders(t *testing.T) {
	mw := Auth(AuthConfig{DevMode: true})
	var captured *Principal
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = PrincipalFromContext(r.Context())
	}))

	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set(HeaderEdgeUser, "CRLEE")
	req.Header.Set(HeaderEdgeChannel, "MOB")
	req.Header.Set(HeaderEdgeSite, "branch")
	req.Header.Set(HeaderEdgeTier, "vip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if captured == nil {
		t.Fatal("Principal 미주입")
	}
	if captured.Channel != "MOB" || captured.Site != "BRANCH" || captured.Tier != "VIP" {
		t.Errorf("Principal = %+v", captured)
	}
	if captured.ProfileKey() != "MOB.BRANCH.VIP" {
		t.Errorf("ProfileKey = %q", captured.ProfileKey())
	}
}

// JWT (no store) 모드: claims.Site/Tier → Principal 복사.
func TestAuth_JWT_NoStore_CopiesSiteTier(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, _ := auth.NewIssuer(auth.IssuerOptions{KeyID: "k1", PrivateKey: key})
	verifier, _ := auth.NewVerifier(auth.VerifierOptions{Keys: auth.SingleKey{Key: &key.PublicKey}})

	token, err := issuer.Sign(auth.Claims{
		SID:  "sid-x",
		Usid: "CRLEE",
		Chan: "WEB",
		Site: "BRANCH",
		Tier: "VIP",
		EXP:  time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	mw := Auth(AuthConfig{JWTVerifier: verifier})
	var captured *Principal
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = PrincipalFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if captured == nil || captured.ProfileKey() != "WEB.BRANCH.VIP" {
		t.Errorf("Principal Profile 누락: %+v", captured)
	}
}

// SessionStore 모드: sess.Profile → Principal 복사 (신규 경로).
func TestAuth_SessionStore_CopiesSiteTier(t *testing.T) {
	store := auth.NewMemoryStore(auth.MemoryStoreOptions{})
	defer store.Close()

	sess := &auth.Session{
		ID:        "sid-store",
		Usid:      "CRLEE",
		Channel:   "WEB", // legacy
		Profile:   session.Profile{Channel: session.ChannelWeb, Site: session.SiteHQ, Tier: session.TierGold},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	mw := Auth(AuthConfig{TrustEdgeHeaders: true, SessionStore: store})
	var captured *Principal
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = PrincipalFromContext(r.Context())
	}))
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set(HeaderEdgeSID, "sid-store")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if captured == nil {
		t.Fatal("Principal 미주입")
	}
	if captured.ProfileKey() != "WEB.HQ.GOLD" {
		t.Errorf("Profile mismatch: %q (full=%+v)", captured.ProfileKey(), captured)
	}
}
