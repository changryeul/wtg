package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PEM 키 파일 왕복 — 발급(Issuer) 토큰을 파일 로드된 Verifier 가 검증.
func TestKeyfileIssuerVerifierRoundTrip(t *testing.T) {
	dir := t.TempDir()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPath := filepath.Join(dir, "jwt-private.pem")
	pubPath := filepath.Join(dir, "jwt-public.pem")
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0600); err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0644); err != nil {
		t.Fatal(err)
	}

	iss, selfVer, err := IssuerFromPrivateKeyFile(privPath, "wtg-test")
	if err != nil {
		t.Fatalf("IssuerFromPrivateKeyFile: %v", err)
	}
	tok, err := iss.Sign(Claims{SID: "s1", Usid: "tester01", Chan: "WEB", EXP: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selfVer.Verify(tok); err != nil {
		t.Errorf("자기 검증 실패: %v", err)
	}

	fileVer, err := VerifierFromPublicKeyFile(pubPath)
	if err != nil {
		t.Fatalf("VerifierFromPublicKeyFile: %v", err)
	}
	claims, err := fileVer.Verify(tok)
	if err != nil {
		t.Fatalf("파일 public key 검증 실패: %v", err)
	}
	if claims.Usid != "tester01" {
		t.Errorf("usid = %q", claims.Usid)
	}
}
