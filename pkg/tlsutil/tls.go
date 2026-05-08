// Package tlsutil 은 WTG 의 mTLS 부트스트랩 헬퍼.
//
// 사용처:
//
//   - HTTP 서버 (mci-api, mci-edge-api 등) 의 ListenAndServeTLS
//   - gRPC 서버 (mci-price, mci-push) 의 grpc.Creds(...)
//   - gRPC 클라이언트 (mci-edge-price 등) 의 grpc.WithTransportCredentials(...)
//
// 운영 인증서는 외부 KMS / 사내 CA 가 발급한 PEM 파일을 디스크에서 로딩하고,
// 테스트 / dev 환경에서는 GenerateSelfSigned 로 즉석 인증서를 만든다 (CA 신뢰 없이).
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// ServerOptions 는 *tls.Config (서버 측) 생성 입력.
type ServerOptions struct {
	// CertFile, KeyFile — 서버 인증서/키 PEM. 둘 다 필수.
	CertFile string
	KeyFile  string

	// ClientCAFile — 클라이언트 인증서 검증에 사용할 CA bundle PEM.
	// 비어있으면 클라이언트 인증을 요구하지 않음 (TLS only, mutual 아님).
	ClientCAFile string

	// MinVersion — 0 이면 TLS 1.2 (운영 정책 — TLS 1.0/1.1 거부).
	MinVersion uint16
}

// LoadServer 는 ServerOptions 로 *tls.Config 를 만든다.
//
// ClientCAFile 이 채워져 있으면 ClientAuth=tls.RequireAndVerifyClientCert (mTLS).
func LoadServer(opt ServerOptions) (*tls.Config, error) {
	if opt.CertFile == "" || opt.KeyFile == "" {
		return nil, errors.New("tlsutil: CertFile/KeyFile 필수")
	}
	cert, err := tls.LoadX509KeyPair(opt.CertFile, opt.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: 서버 cert/key 로딩 실패: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   selectMinVersion(opt.MinVersion),
	}
	if opt.ClientCAFile != "" {
		pool, err := loadCABundle(opt.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: client CA 로딩 실패: %w", err)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientOptions 는 *tls.Config (클라이언트 측) 생성 입력.
type ClientOptions struct {
	// CertFile, KeyFile — 클라이언트 인증서/키. 서버가 mTLS 요구 시 필수.
	CertFile string
	KeyFile  string

	// ServerCAFile — 서버 인증서 검증용 CA. 비어있으면 시스템 trust store.
	ServerCAFile string

	// ServerName — TLS SNI / hostname 검증 (인증서 SAN 과 일치해야).
	// 빈 값이면 dial 호스트 그대로 사용 (gRPC 의 경우 :authority).
	ServerName string

	// InsecureSkipVerify — 인증서 체인/호스트 검증 비활성. **운영 금지**.
	// dev 자체발급 환경에서만 사용.
	InsecureSkipVerify bool

	// MinVersion — 0 이면 TLS 1.2.
	MinVersion uint16
}

// LoadClient 는 ClientOptions 로 *tls.Config 를 만든다.
func LoadClient(opt ClientOptions) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         selectMinVersion(opt.MinVersion),
		ServerName:         opt.ServerName,
		InsecureSkipVerify: opt.InsecureSkipVerify,
	}
	if opt.CertFile != "" || opt.KeyFile != "" {
		if opt.CertFile == "" || opt.KeyFile == "" {
			return nil, errors.New("tlsutil: CertFile/KeyFile 둘 다 또는 둘 다 비어야")
		}
		cert, err := tls.LoadX509KeyPair(opt.CertFile, opt.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: 클라이언트 cert/key 로딩 실패: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if opt.ServerCAFile != "" {
		pool, err := loadCABundle(opt.ServerCAFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: server CA 로딩 실패: %w", err)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// loadCABundle — PEM 파일에서 모든 CERTIFICATE 블록을 *x509.CertPool 에 추가.
func loadCABundle(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("CA bundle %q 에 유효한 인증서 없음", path)
	}
	return pool, nil
}

func selectMinVersion(v uint16) uint16 {
	if v == 0 {
		return tls.VersionTLS12
	}
	return v
}

// SelfSigned 는 자체발급 CA + 인증서 페어 (테스트/dev 용).
//
// 단순 self-sign: CA 와 leaf 가 동일 인증서. 운영에서는 사용 금지.
type SelfSigned struct {
	CertPEM []byte // 서버/클라이언트 둘 다에서 사용 가능
	KeyPEM  []byte
}

// SelfSignedOptions — DNSNames + IPs 가 SAN 으로 들어간다.
type SelfSignedOptions struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	NotBefore  time.Time
	NotAfter   time.Time
}

// GenerateSelfSigned 는 ECDSA-P256 자체발급 인증서를 만든다.
// 테스트 / dev 외 사용 금지.
func GenerateSelfSigned(opt SelfSignedOptions) (*SelfSigned, error) {
	if opt.NotBefore.IsZero() {
		opt.NotBefore = time.Now().Add(-1 * time.Minute)
	}
	if opt.NotAfter.IsZero() {
		opt.NotAfter = time.Now().Add(24 * time.Hour)
	}
	if opt.CommonName == "" {
		opt.CommonName = "wtg-dev"
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: opt.CommonName},
		NotBefore:    opt.NotBefore,
		NotAfter:     opt.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              opt.DNSNames,
		IPAddresses:           opt.IPs,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	return &SelfSigned{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// WriteToFiles 는 테스트에서 임시 디렉토리에 cert/key 파일을 쓴다.
// 반환된 두 경로는 LoadServer/LoadClient 의 *File 인자에 그대로 전달.
func (s *SelfSigned) WriteToFiles(dir, prefix string) (certPath, keyPath string, err error) {
	certPath = dir + "/" + prefix + ".crt"
	keyPath = dir + "/" + prefix + ".key"
	if err = os.WriteFile(certPath, s.CertPEM, 0o600); err != nil {
		return "", "", err
	}
	if err = os.WriteFile(keyPath, s.KeyPEM, 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}
