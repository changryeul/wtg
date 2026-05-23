package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JWT 는 auth.md §6 의 access JWT.
//
// 알고리즘: RS256 (auth.md 권장 — edge 가 public key 만 보유하면 검증 가능,
// 시크릿이 DMZ 에 노출되지 않음).
//
// 1차 prototype 은 외부 라이브러리 없이 표준 crypto/rsa + sha256 으로 직접
// 구현한다. JWT 표준 (RFC 7519) 의 header.payload.signature 형태:
//
//	BASE64URL(header) + "." + BASE64URL(payload) + "." + BASE64URL(sig)
//
// 운영팀이 합의 후 키 로테이션, JWKS 노출 등은 별도 작업.

// Claims 는 access JWT 의 payload (auth.md §6).
//
// Site/Tier 는 시세 fan-out 의 Profile 결정에 사용된다 (mci-edge-price 가
// ws upgrade 시점에 Channel.Site.Tier 로 routing key 를 구성). 기존 토큰은
// 이 필드 없이 발급되었을 수 있으므로 omitempty.
type Claims struct {
	SID  string `json:"sid"`            // session ID — Store 의 키
	Usid string `json:"usid"`           // 사용자 ID (디버깅/감사용)
	Chan string `json:"chan,omitempty"` // 채널 코드 ("WEB" / "ADMIN" 등)
	Site string `json:"site,omitempty"` // 거래 주체 ("BRANCH" / "HQ")
	Tier string `json:"tier,omitempty"` // 고객 등급 ("VIP" / "GOLD" / "STD")
	IAT  int64  `json:"iat"`            // 발급 시각 (Unix sec)
	EXP  int64  `json:"exp"`            // 만료 시각 (Unix sec)
	JTI  string `json:"jti,omitempty"`  // 단일 사용 검증용
}

// ProfileKey 는 Chan/Site/Tier 가 모두 채워졌을 때 시세 fan-out routing key 를
// 반환한다 (예: "WEB.BRANCH.VIP"). 하나라도 비어있으면 빈 문자열 반환.
func (c Claims) ProfileKey() string {
	if c.Chan == "" || c.Site == "" || c.Tier == "" {
		return ""
	}
	return c.Chan + "." + c.Site + "." + c.Tier
}

// jwtHeader 는 JOSE header. RS256 고정.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// JWT 검증 에러 sentinel.
var (
	ErrJWTMalformed       = errors.New("auth: JWT 형식 오류")
	ErrJWTUnsupportedAlg  = errors.New("auth: JWT alg 가 RS256 이 아님")
	ErrJWTBadSignature    = errors.New("auth: JWT 서명 검증 실패")
	ErrJWTExpired         = errors.New("auth: JWT 만료")
	ErrJWTNotYetValid     = errors.New("auth: JWT 가 아직 유효하지 않음 (iat 미래)")
	ErrJWTKeyNotFound     = errors.New("auth: kid 에 해당하는 키 없음")
)

// KeyID 는 키 회전을 위한 식별자. 동일 키로 발급된 모든 JWT 는 동일 kid 를 갖는다.
type KeyID string

// Issuer 는 JWT 발급자. 단일 RSA private key + kid 보유.
//
// 운영에서는 KMS / HSM 로 차환 가능 — Sign 시그니처가 동일하면 호출자 변경 불필요.
type Issuer struct {
	now func() time.Time
	kid KeyID
	key *rsa.PrivateKey
}

// IssuerOptions 는 Issuer 생성 옵션.
type IssuerOptions struct {
	KeyID      KeyID            // kid claim 값. 빈 값이면 빈 문자열 (단일 키 환경).
	PrivateKey *rsa.PrivateKey  // 필수
	Now        func() time.Time // 0 이면 time.Now (테스트용)
}

// NewIssuer 는 새 Issuer 를 만든다. PrivateKey 가 nil 이면 에러.
func NewIssuer(opt IssuerOptions) (*Issuer, error) {
	if opt.PrivateKey == nil {
		return nil, errors.New("auth: PrivateKey 필수")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Issuer{
		now: opt.Now,
		kid: opt.KeyID,
		key: opt.PrivateKey,
	}, nil
}

// Sign 은 Claims 를 RS256 서명한 JWT 문자열로 반환한다.
//
// 호출자는 SID/Usid/Chan/EXP 를 채워 넘긴다. IAT 가 0 이면 자동으로 now 를
// 채운다. JTI 가 비어있으면 자동 생성.
func (i *Issuer) Sign(c Claims) (string, error) {
	if c.IAT == 0 {
		c.IAT = i.now().Unix()
	}
	if c.JTI == "" {
		jti, err := NewSessionID() // 같은 형식의 16진수+ 호환 ID
		if err != nil {
			return "", err
		}
		c.JTI = jti
	}
	header := jwtHeader{Alg: "RS256", Typ: "JWT", Kid: string(i.kid)}
	hdrJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payJSON, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hdrJSON) + "." + b64url(payJSON)
	hash := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

// KeyResolver 는 kid → public key 매핑. 키 회전 환경에서 여러 public key 보유.
type KeyResolver interface {
	PublicKey(kid KeyID) (*rsa.PublicKey, error)
}

// SingleKey 는 kid 무시하고 단일 public key 만 반환하는 KeyResolver — 1차 환경용.
type SingleKey struct {
	Key *rsa.PublicKey
}

func (s SingleKey) PublicKey(_ KeyID) (*rsa.PublicKey, error) {
	if s.Key == nil {
		return nil, ErrJWTKeyNotFound
	}
	return s.Key, nil
}

// KeyMap 은 kid 별 public key 매핑 — JWKS rotation 환경용.
type KeyMap map[KeyID]*rsa.PublicKey

func (m KeyMap) PublicKey(kid KeyID) (*rsa.PublicKey, error) {
	k, ok := m[kid]
	if !ok || k == nil {
		return nil, ErrJWTKeyNotFound
	}
	return k, nil
}

// Verifier 는 JWT 서명/만료 검증.
type Verifier struct {
	now      func() time.Time
	keys     KeyResolver
	skewSec  int64
}

// VerifierOptions 는 Verifier 생성 옵션.
type VerifierOptions struct {
	Keys     KeyResolver
	Now      func() time.Time
	ClockSkew time.Duration // 시계 오차 허용 (default 30s, max 5min)
}

// NewVerifier — Keys 가 nil 이면 에러.
func NewVerifier(opt VerifierOptions) (*Verifier, error) {
	if opt.Keys == nil {
		return nil, errors.New("auth: KeyResolver 필수")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	skew := opt.ClockSkew
	if skew <= 0 {
		skew = 30 * time.Second
	}
	if skew > 5*time.Minute {
		skew = 5 * time.Minute
	}
	return &Verifier{
		now:     opt.Now,
		keys:    opt.Keys,
		skewSec: int64(skew.Seconds()),
	}, nil
}

// Verify 는 token 을 파싱·서명검증·만료검증하고 Claims 를 반환.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrJWTMalformed
	}
	hdrBytes, err := decodeB64url(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header", ErrJWTMalformed)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header json", ErrJWTMalformed)
	}
	if hdr.Alg != "RS256" {
		return nil, ErrJWTUnsupportedAlg
	}

	pubKey, err := v.keys.PublicKey(KeyID(hdr.Kid))
	if err != nil {
		return nil, err
	}

	sig, err := decodeB64url(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: sig", ErrJWTMalformed)
	}
	signingInput := parts[0] + "." + parts[1]
	hash := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sig); err != nil {
		return nil, ErrJWTBadSignature
	}

	payBytes, err := decodeB64url(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload", ErrJWTMalformed)
	}
	var c Claims
	if err := json.Unmarshal(payBytes, &c); err != nil {
		return nil, fmt.Errorf("%w: payload json", ErrJWTMalformed)
	}

	now := v.now().Unix()
	if c.EXP > 0 && now > c.EXP+v.skewSec {
		return nil, ErrJWTExpired
	}
	if c.IAT > 0 && c.IAT > now+v.skewSec {
		return nil, ErrJWTNotYetValid
	}
	return &c, nil
}

// GenerateRSAKeyPair 는 테스트 / dev 환경용 RSA 2048 키 페어 생성.
//
// 운영에서는 외부 KMS / 사전 발급된 PEM 을 사용하므로 이 함수는 호출하지 않는다.
func GenerateRSAKeyPair(bits int) (*rsa.PrivateKey, error) {
	if bits == 0 {
		bits = 2048
	}
	return rsa.GenerateKey(rand.Reader, bits)
}

// b64url 은 RFC 4648 base64url, no-padding (JWT 표준).
func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeB64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
