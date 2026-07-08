package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// PEM 키 파일 → Issuer / Verifier 헬퍼.
//
// 서비스 배선 표준:
//   - mci-api: --jwt-key (RSA private PEM) → IssuerFromPrivateKeyFile → SetJWT
//   - edge 서비스: --jwt-pub (RSA public PEM) → VerifierFromPublicKeyFile
//
// 키 생성 (운영 1회):
//   openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out jwt-private.pem
//   openssl pkey -in jwt-private.pem -pubout -out jwt-public.pem

// IssuerFromPrivateKeyFile 은 RSA private key PEM (PKCS#1/PKCS#8) 파일로
// Issuer 를 만든다. Verifier 도 같은 키의 public 부로 함께 반환한다
// (동일 프로세스 내 검증용 — mci-api 가 자기 발급 토큰을 검증).
func IssuerFromPrivateKeyFile(path string, kid KeyID) (*Issuer, *Verifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: private key 읽기: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, nil, errors.New("auth: private key 가 PEM 형식이 아님")
	}
	var priv *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("auth: private key 가 RSA 가 아님")
		}
		priv = rk
	} else if rk, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
		priv = rk
	} else {
		return nil, nil, fmt.Errorf("auth: private key 파싱 (PKCS8: %v / PKCS1: %v)", err, err2)
	}

	iss, err := NewIssuer(IssuerOptions{KeyID: kid, PrivateKey: priv})
	if err != nil {
		return nil, nil, err
	}
	ver, err := NewVerifier(VerifierOptions{Keys: SingleKey{Key: &priv.PublicKey}})
	if err != nil {
		return nil, nil, err
	}
	return iss, ver, nil
}

// VerifierFromPublicKeyFile 은 RSA public key PEM (PKIX/PKCS#1) 파일로
// Verifier 를 만든다 — DMZ edge 서비스용 (private key 미보유).
func VerifierFromPublicKeyFile(path string) (*Verifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: public key 읽기: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("auth: public key 가 PEM 형식이 아님")
	}
	var pub *rsa.PublicKey
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("auth: public key 가 RSA 가 아님")
		}
		pub = rk
	} else if rk, err2 := x509.ParsePKCS1PublicKey(block.Bytes); err2 == nil {
		pub = rk
	} else {
		return nil, fmt.Errorf("auth: public key 파싱 (PKIX: %v / PKCS1: %v)", err, err2)
	}
	return NewVerifier(VerifierOptions{Keys: SingleKey{Key: pub}})
}
