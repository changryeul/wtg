package tlsutil

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

func mkSelfSigned(t *testing.T) *SelfSigned {
	t.Helper()
	ss, err := GenerateSelfSigned(SelfSignedOptions{
		CommonName: "test-ca",
		DNSNames:   []string{"localhost", "wtg-test"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func TestGenerateSelfSignedFields(t *testing.T) {
	ss := mkSelfSigned(t)
	if !strings.Contains(string(ss.CertPEM), "BEGIN CERTIFICATE") {
		t.Error("cert PEM 형식 아님")
	}
	if !strings.Contains(string(ss.KeyPEM), "BEGIN EC PRIVATE KEY") {
		t.Error("key PEM 형식 아님")
	}
}

func TestSelfSignedExpiryDefaults(t *testing.T) {
	ss, err := GenerateSelfSigned(SelfSignedOptions{
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(ss.CertPEM, ss.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("certificate chain 비어있음")
	}
}

func TestLoadServerRequiresFiles(t *testing.T) {
	if _, err := LoadServer(ServerOptions{}); err == nil {
		t.Error("빈 옵션 통과")
	}
}

func TestLoadServerAndClientRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ss := mkSelfSigned(t)
	certPath, keyPath, err := ss.WriteToFiles(dir, "srv")
	if err != nil {
		t.Fatal(err)
	}
	caPath := certPath // self-signed: 자기 자신이 CA

	srvCfg, err := LoadServer(ServerOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: caPath,
	})
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if srvCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("mTLS 요구 안됨")
	}
	if srvCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion=%d, want TLS1.2", srvCfg.MinVersion)
	}

	cliCfg, err := LoadClient(ClientOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ServerCAFile: caPath,
		ServerName:   "localhost",
	})
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cliCfg.ServerName != "localhost" {
		t.Errorf("ServerName: %q", cliCfg.ServerName)
	}
	if cliCfg.RootCAs == nil {
		t.Error("RootCAs 미설정")
	}

	// 실제 TLS 핸드셰이크.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		// 핸드셰이크 강제.
		if tc, ok := conn.(*tls.Conn); ok {
			done <- tc.Handshake()
			return
		}
		done <- nil
	}()

	cliConn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cliConn.Close()

	if err := cliConn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server handshake: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server handshake 타임아웃")
	}
}

// 클라이언트 인증서 없이 mTLS 서버 dial → 실패해야.
func TestMTLSRejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	ss := mkSelfSigned(t)
	certPath, keyPath, _ := ss.WriteToFiles(dir, "srv")

	srvCfg, _ := LoadServer(ServerOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: certPath,
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err == nil {
			if tc, ok := conn.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			conn.Close()
		}
	}()

	cliCfg, _ := LoadClient(ClientOptions{
		ServerCAFile: certPath, // 서버 신뢰 OK, 클라이언트 cert 안 보냄
		ServerName:   "localhost",
	})
	conn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		return // 일부 OS / 버전은 dial 자체에서 실패
	}
	defer conn.Close()
	// Handshake 가 통과해도 첫 Read 에서 서버 alert 가 옴.
	if err := conn.Handshake(); err != nil {
		return
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Error("클라이언트 cert 없이 mTLS 데이터 통신 가능")
	}
}

func TestLoadClientPartialCertKey(t *testing.T) {
	if _, err := LoadClient(ClientOptions{CertFile: "x.crt"}); err == nil {
		t.Error("Cert 만 주고 통과")
	}
	if _, err := LoadClient(ClientOptions{KeyFile: "x.key"}); err == nil {
		t.Error("Key 만 주고 통과")
	}
}

// CA bundle 미존재 / 잘못된 PEM.
func TestLoadServerBadCA(t *testing.T) {
	dir := t.TempDir()
	ss := mkSelfSigned(t)
	certPath, keyPath, _ := ss.WriteToFiles(dir, "srv")

	if _, err := LoadServer(ServerOptions{
		CertFile: certPath, KeyFile: keyPath,
		ClientCAFile: "/nonexistent",
	}); err == nil {
		t.Error("미존재 CA 통과")
	}
}
