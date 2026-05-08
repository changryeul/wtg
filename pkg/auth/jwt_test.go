package auth

import (
	"crypto/rsa"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mkIssuerVerifier(t *testing.T, kid KeyID, now func() time.Time) (*Issuer, *Verifier, *rsa.PrivateKey) {
	t.Helper()
	priv, err := GenerateRSAKeyPair(2048)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(IssuerOptions{KeyID: kid, PrivateKey: priv, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	ver, err := NewVerifier(VerifierOptions{
		Keys: SingleKey{Key: &priv.PublicKey},
		Now:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return iss, ver, priv
}

func TestJWTRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	iss, ver, _ := mkIssuerVerifier(t, "k1", func() time.Time { return now })

	tok, err := iss.Sign(Claims{
		SID:  "sess-abc",
		Usid: "trader01",
		Chan: "WEB",
		EXP:  now.Add(15 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tok, ".") != 2 {
		t.Fatalf("토큰 형식: %q", tok)
	}

	c, err := ver.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.SID != "sess-abc" || c.Usid != "trader01" || c.Chan != "WEB" {
		t.Errorf("claims: %+v", c)
	}
	if c.JTI == "" {
		t.Error("JTI 자동 생성 안됨")
	}
	if c.IAT == 0 {
		t.Error("IAT 자동 채움 안됨")
	}
}

func TestJWTExpired(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	iss, ver, _ := mkIssuerVerifier(t, "k1", now)
	tok, err := iss.Sign(Claims{SID: "x", EXP: now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}

	// 시계 진행 → 만료 + skew 초과.
	clock.Add(int64(2*time.Minute) + 60)
	if _, err := ver.Verify(tok); !errors.Is(err, ErrJWTExpired) {
		t.Errorf("err=%v, want ErrJWTExpired", err)
	}
}

func TestJWTClockSkewAllowsBoundary(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }

	priv, _ := GenerateRSAKeyPair(2048)
	iss, _ := NewIssuer(IssuerOptions{PrivateKey: priv, Now: now})
	ver, _ := NewVerifier(VerifierOptions{
		Keys:      SingleKey{Key: &priv.PublicKey},
		Now:       now,
		ClockSkew: 60 * time.Second,
	})
	tok, _ := iss.Sign(Claims{EXP: now().Unix()}) // 즉시 만료
	clock.Add(30)                                  // 30초 경과 — skew 60s 안에 있음
	if _, err := ver.Verify(tok); err != nil {
		t.Errorf("skew 안에서 만료로 처리됨: %v", err)
	}
	clock.Add(40) // 70초 — skew 초과
	if _, err := ver.Verify(tok); !errors.Is(err, ErrJWTExpired) {
		t.Errorf("skew 초과: %v", err)
	}
}

// 다른 키로 서명한 토큰은 거부.
func TestJWTBadSignature(t *testing.T) {
	iss, _, _ := mkIssuerVerifier(t, "k1", time.Now)
	tok, _ := iss.Sign(Claims{SID: "x", EXP: time.Now().Add(time.Hour).Unix()})

	// 다른 verifier (다른 키).
	_, otherVer, _ := mkIssuerVerifier(t, "k1", time.Now)
	if _, err := otherVer.Verify(tok); !errors.Is(err, ErrJWTBadSignature) {
		t.Errorf("err=%v, want ErrJWTBadSignature", err)
	}
}

func TestJWTMalformed(t *testing.T) {
	_, ver, _ := mkIssuerVerifier(t, "k1", time.Now)
	cases := []string{
		"",
		"a.b",
		"a.b.c.d",
		"!!!.!!!.!!!", // base64url 디코딩 실패
	}
	for _, c := range cases {
		if _, err := ver.Verify(c); !errors.Is(err, ErrJWTMalformed) {
			t.Errorf("token=%q err=%v", c, err)
		}
	}
}

// alg 가 RS256 이 아니면 거부.
func TestJWTUnsupportedAlg(t *testing.T) {
	_, ver, _ := mkIssuerVerifier(t, "k1", time.Now)
	// HS256 헤더 임의 조립 — 실제 서명 X. 단지 alg 검증 로직 확인.
	hdr := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pay := b64url([]byte(`{"sid":"x"}`))
	tok := hdr + "." + pay + ".sig"
	if _, err := ver.Verify(tok); !errors.Is(err, ErrJWTUnsupportedAlg) {
		t.Errorf("err=%v, want ErrJWTUnsupportedAlg", err)
	}
}

// kid 가 KeyMap 에 없으면 ErrJWTKeyNotFound.
func TestJWTKeyMapResolver(t *testing.T) {
	priv, _ := GenerateRSAKeyPair(2048)
	iss, _ := NewIssuer(IssuerOptions{KeyID: "K1", PrivateKey: priv})
	tok, _ := iss.Sign(Claims{EXP: time.Now().Add(time.Hour).Unix()})

	// 등록된 K1 → 통과.
	v, _ := NewVerifier(VerifierOptions{Keys: KeyMap{"K1": &priv.PublicKey}})
	if _, err := v.Verify(tok); err != nil {
		t.Errorf("등록된 kid: %v", err)
	}

	// 등록 안 된 kid → ErrJWTKeyNotFound.
	v2, _ := NewVerifier(VerifierOptions{Keys: KeyMap{"OTHER": &priv.PublicKey}})
	if _, err := v2.Verify(tok); !errors.Is(err, ErrJWTKeyNotFound) {
		t.Errorf("err=%v, want ErrJWTKeyNotFound", err)
	}
}

// Issuer/Verifier 옵션 검증.
func TestNewIssuerNilKey(t *testing.T) {
	if _, err := NewIssuer(IssuerOptions{PrivateKey: nil}); err == nil {
		t.Error("nil key 인데 통과")
	}
}

func TestNewVerifierNilKeys(t *testing.T) {
	if _, err := NewVerifier(VerifierOptions{Keys: nil}); err == nil {
		t.Error("nil keys 인데 통과")
	}
}
