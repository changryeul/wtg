package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"
)

func TestClaims_ProfileKey(t *testing.T) {
	tests := []struct {
		name string
		c    Claims
		want string
	}{
		{"all set", Claims{Chan: "WEB", Site: "BRANCH", Tier: "VIP"}, "WEB.BRANCH.VIP"},
		{"channel only", Claims{Chan: "WEB"}, ""},
		{"missing tier", Claims{Chan: "WEB", Site: "BRANCH"}, ""},
		{"missing chan", Claims{Site: "BRANCH", Tier: "VIP"}, ""},
		{"all empty", Claims{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.ProfileKey(); got != tc.want {
				t.Errorf("ProfileKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// JWT round-trip: Sign/Verify 후 Site/Tier 가 보존되는지.
func TestJWT_RoundTrip_SiteTier(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewIssuer(IssuerOptions{KeyID: "k1", PrivateKey: key})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierOptions{Keys: SingleKey{Key: &key.PublicKey}})
	if err != nil {
		t.Fatal(err)
	}

	want := Claims{
		SID:  "sid-abc",
		Usid: "CRLEE",
		Chan: "WEB",
		Site: "BRANCH",
		Tier: "VIP",
		EXP:  time.Now().Add(time.Hour).Unix(),
	}
	token, err := issuer.Sign(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := verifier.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != "BRANCH" || got.Tier != "VIP" || got.Chan != "WEB" {
		t.Errorf("round-trip mismatch: chan=%q site=%q tier=%q", got.Chan, got.Site, got.Tier)
	}
	if got.ProfileKey() != "WEB.BRANCH.VIP" {
		t.Errorf("ProfileKey = %q", got.ProfileKey())
	}
}

// 기존 토큰 (Site/Tier 미포함) 도 정상 Verify (backward compat).
func TestJWT_BackwardCompat_NoSiteTier(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	issuer, _ := NewIssuer(IssuerOptions{KeyID: "k1", PrivateKey: key})
	verifier, _ := NewVerifier(VerifierOptions{Keys: SingleKey{Key: &key.PublicKey}})

	old := Claims{
		SID:  "sid-old",
		Usid: "OLD",
		Chan: "WEB",
		EXP:  time.Now().Add(time.Hour).Unix(),
		// Site/Tier 비워둠 — 기존 발급 시뮬레이션
	}
	token, _ := issuer.Sign(old)
	got, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("기존 토큰 Verify 실패: %v", err)
	}
	if got.Site != "" || got.Tier != "" {
		t.Errorf("Site/Tier 가 비어있어야 함: %+v", got)
	}
	if got.ProfileKey() != "" {
		t.Errorf("ProfileKey 가 비어있어야 함")
	}
}
