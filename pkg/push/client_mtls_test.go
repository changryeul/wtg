package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// mTLS test 전략:
//   tlsutil.GenerateSelfSigned 가 ExtKeyUsage 에 ServerAuth + ClientAuth 둘 다 + IsCA=true
//   이므로 같은 cert 를 (1) CA, (2) 서버 leaf, (3) 클라이언트 leaf 세 역할 모두 사용 가능.
//   운영 PKI 와 다르지만 mTLS handshake 검증에는 충분 (CA pool 에 self cert append).
//   각 test 가 서로 다른 cert 를 만들어 server/client identity 를 구분.

// TestClient_MTLS_Roundtrip — mTLS handshake 통과 + Push 정상 동작.
// 핵심 검증: 서버 핸들러가 r.TLS.PeerCertificates[0].CN 으로 클라이언트 식별.
func TestClient_MTLS_Roundtrip(t *testing.T) {
	srvSS := mustSelfSign(t, "wtg-test-push")
	cliSS := mustSelfSign(t, "wtg-test-svc")

	// 양쪽 trust pool — server 는 client cert 를 검증해야 하므로 cliSS 를 pool 에.
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(cliSS.CertPEM) {
		t.Fatal("client trust pool 구성 실패")
	}

	serverCert, err := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	var observedCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			observedCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		_ = json.NewEncoder(w).Encode(Result{Injected: true, User: "x"})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	// 클라이언트 cert/key/CA 를 temp file 로.
	dir := t.TempDir()
	cliCertPath, cliKeyPath, err := cliSS.WriteToFiles(dir, "client")
	if err != nil {
		t.Fatalf("client PEM write: %v", err)
	}
	caPath := dir + "/ca.crt"
	if err := writeFile(caPath, srvSS.CertPEM); err != nil {
		t.Fatalf("CA write: %v", err)
	}

	cli := MustNewClient(ClientOptions{
		BaseURL:           srv.URL,
		TLSClientCertFile: cliCertPath,
		TLSClientKeyFile:  cliKeyPath,
		TLSServerCAFile:   caPath,
		TLSServerName:     "wtg-test-push", // 서버 cert CN
	})
	defer cli.Close()

	res, err := cli.Push(context.Background(), Message{User: "u1", Data: json.RawMessage(`"hi"`)})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Injected {
		t.Errorf("Injected=true 기대")
	}
	if observedCN != "wtg-test-svc" {
		t.Errorf("server 가 본 client CN=%q (기대 wtg-test-svc)", observedCN)
	}
}

// TestClient_MTLS_MissingClientCert — 서버 mTLS 요구 시 client cert 없으면 handshake 실패.
func TestClient_MTLS_MissingClientCert(t *testing.T) {
	srvSS := mustSelfSign(t, "wtg-test-push")
	cliSS := mustSelfSign(t, "wtg-test-svc")

	clientPool := x509.NewCertPool()
	_ = clientPool.AppendCertsFromPEM(cliSS.CertPEM)
	serverCert, _ := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caPath := dir + "/ca.crt"
	_ = writeFile(caPath, srvSS.CertPEM)

	cli := MustNewClient(ClientOptions{
		BaseURL:         srv.URL,
		TLSServerCAFile: caPath,
		TLSServerName:   "wtg-test-push",
	})
	defer cli.Close()

	_, err := cli.Push(context.Background(), Message{Data: json.RawMessage(`"x"`)})
	if err == nil {
		t.Fatal("client cert 없이 mTLS 서버에 push → error 기대")
	}
	t.Logf("missing client cert err: %v", err)
}

// TestClient_HTTPS_NoMTLS — 서버 TLS only (mTLS X) 면 client cert 없어도 통과.
func TestClient_HTTPS_NoMTLS(t *testing.T) {
	srvSS := mustSelfSign(t, "wtg-test-push")
	serverCert, _ := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
		// ClientAuth 기본 = NoClientCert.
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	caPath := dir + "/ca.crt"
	_ = writeFile(caPath, srvSS.CertPEM)

	cli := MustNewClient(ClientOptions{
		BaseURL:         srv.URL,
		TLSServerCAFile: caPath,
		TLSServerName:   "wtg-test-push",
	})
	defer cli.Close()

	if _, err := cli.Push(context.Background(), Message{Data: json.RawMessage(`"x"`)}); err != nil {
		t.Fatalf("HTTPS 단방향 TLS push: %v", err)
	}
}

// TestClient_TLSInsecure — InsecureSkipVerify=true 면 ServerCA 없어도 통과 (dev only).
func TestClient_TLSInsecure(t *testing.T) {
	srvSS := mustSelfSign(t, "wtg-test-push")
	serverCert, _ := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	cli := MustNewClient(ClientOptions{
		BaseURL:     srv.URL,
		TLSInsecure: true,
	})
	defer cli.Close()

	if _, err := cli.Push(context.Background(), Message{Data: json.RawMessage(`"x"`)}); err != nil {
		t.Fatalf("Insecure TLS push: %v", err)
	}
}

// TestMultiClient_MTLS — MultiClient 도 TLS 옵션 propagate.
func TestMultiClient_MTLS(t *testing.T) {
	srvSS := mustSelfSign(t, "wtg-test-push")
	cliSS := mustSelfSign(t, "wtg-test-svc")
	serverCert, _ := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)
	clientPool := x509.NewCertPool()
	_ = clientPool.AppendCertsFromPEM(cliSS.CertPEM)

	servers := make([]*httptest.Server, 2)
	var observedCN [2]string
	for i := 0; i < 2; i++ {
		idx := i
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				observedCN[idx] = r.TLS.PeerCertificates[0].Subject.CommonName
			}
			_ = json.NewEncoder(w).Encode(Result{Injected: true})
		}))
		s.TLS = &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientCAs:    clientPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		}
		s.StartTLS()
		servers[i] = s
		defer s.Close()
	}

	dir := t.TempDir()
	cliCertPath, cliKeyPath, _ := cliSS.WriteToFiles(dir, "client")
	caPath := dir + "/ca.crt"
	_ = writeFile(caPath, srvSS.CertPEM)

	mc, err := NewMultiClient(MultiClientOptions{
		Endpoints:         []string{servers[0].URL, servers[1].URL},
		TLSClientCertFile: cliCertPath,
		TLSClientKeyFile:  cliKeyPath,
		TLSServerCAFile:   caPath,
		TLSServerName:     "wtg-test-push",
	})
	if err != nil {
		t.Fatalf("NewMultiClient: %v", err)
	}
	defer mc.Close()

	if _, err := mc.Push(context.Background(), Message{Data: json.RawMessage(`"halt"`)}); err != nil {
		t.Fatalf("broadcast Push: %v", err)
	}
	for i, cn := range observedCN {
		if cn != "wtg-test-svc" {
			t.Errorf("인스턴스 %d CN=%q (기대 wtg-test-svc)", i, cn)
		}
	}
}

// TestMultiClient_MTLS_BadCertFile — 존재 안 하는 cert → NewMultiClient error.
func TestMultiClient_MTLS_BadCertFile(t *testing.T) {
	_, err := NewMultiClient(MultiClientOptions{
		Endpoints:         []string{"https://push"},
		TLSClientCertFile: "/nonexistent/cert.pem",
		TLSClientKeyFile:  "/nonexistent/key.pem",
	})
	if err == nil {
		t.Fatal("존재 안 하는 cert → NewMultiClient error 기대")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Errorf("err 메시지에 'TLS' 포함 기대: %v", err)
	}
}

// --- helpers ---

func mustSelfSign(t *testing.T, cn string) *tlsutil.SelfSigned {
	t.Helper()
	ss, err := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		CommonName: cn,
		DNSNames:   []string{cn, "localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("self-sign %q: %v", cn, err)
	}
	return ss
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
