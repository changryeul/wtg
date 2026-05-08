package tlsutil

import (
	"crypto/tls"
	"net"
	"os"
	"testing"
	"time"
)

func mkCertFiles(t *testing.T, cn string) (certPath, keyPath string) {
	t.Helper()
	ss, err := GenerateSelfSigned(SelfSignedOptions{
		CommonName: cn,
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath, err = ss.WriteToFiles(dir, "tls")
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestReloaderInitialLoad(t *testing.T) {
	cert, key := mkCertFiles(t, "init")
	rl, err := NewReloader(ReloaderOptions{CertFile: cert, KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Stop()
	if rl.CurrentCertSubject() != "init" {
		t.Errorf("Subject=%q", rl.CurrentCertSubject())
	}
	cfg := rl.ServerConfig()
	c, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || len(c.Certificate) == 0 {
		t.Error("GetCertificate 결과 비어있음")
	}
}

func TestReloaderMissingFile(t *testing.T) {
	if _, err := NewReloader(ReloaderOptions{CertFile: "/nope", KeyFile: "/nope"}); err == nil {
		t.Error("미존재 파일 통과")
	}
}

func TestReloaderRequiresPaths(t *testing.T) {
	if _, err := NewReloader(ReloaderOptions{}); err == nil {
		t.Error("빈 옵션 통과")
	}
}

// 동일 경로에 다른 cert 파일 덮어쓰면 Reload 후 Subject 가 바뀐다.
func TestReloaderSwapsCert(t *testing.T) {
	certPath, keyPath := mkCertFiles(t, "v1")

	rl, err := NewReloader(ReloaderOptions{CertFile: certPath, KeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Stop()
	if rl.CurrentCertSubject() != "v1" {
		t.Fatalf("초기 Subject=%q", rl.CurrentCertSubject())
	}

	// 같은 경로에 새 cert 덮어쓰기.
	ss2, _ := GenerateSelfSigned(SelfSignedOptions{
		CommonName: "v2",
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err := writeOver(certPath, ss2.CertPEM); err != nil {
		t.Fatal(err)
	}
	if err := writeOver(keyPath, ss2.KeyPEM); err != nil {
		t.Fatal(err)
	}

	if err := rl.Reload(); err != nil {
		t.Fatal(err)
	}
	if rl.CurrentCertSubject() != "v2" {
		t.Errorf("Reload 후 Subject=%q, want v2", rl.CurrentCertSubject())
	}
}

// 파일 watch 가 mtime 변경 감지 후 자동 Reload.
func TestReloaderWatchFile(t *testing.T) {
	certPath, keyPath := mkCertFiles(t, "watch-init")
	rl, err := NewReloader(ReloaderOptions{CertFile: certPath, KeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Stop()
	rl.WatchFile(20 * time.Millisecond)

	// mtime 이 명확히 후라고 인식되도록 한 번 sleep.
	time.Sleep(30 * time.Millisecond)

	ss2, _ := GenerateSelfSigned(SelfSignedOptions{
		CommonName: "watch-v2",
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	writeOver(certPath, ss2.CertPEM)
	writeOver(keyPath, ss2.KeyPEM)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rl.CurrentCertSubject() == "watch-v2" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("watch reload 타임아웃 — Subject=%q", rl.CurrentCertSubject())
}

// mTLS round-trip: ClientCA 도 reload 가능 (GetConfigForClient).
func TestReloaderMTLSRoundTrip(t *testing.T) {
	cert, key := mkCertFiles(t, "srv")
	rl, err := NewReloader(ReloaderOptions{
		CertFile: cert, KeyFile: key, ClientCAFile: cert, // self-signed: 자기 CA
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Stop()
	srvCfg := rl.ServerConfig()
	if srvCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("mTLS 미활성")
	}
	if srvCfg.GetConfigForClient == nil {
		t.Error("GetConfigForClient 누락")
	}

	// 클라이언트 cfg.
	cliCfg, _ := LoadClient(ClientOptions{
		CertFile: cert, KeyFile: key,
		ServerCAFile: cert, ServerName: "localhost",
	})

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			tc.Handshake()
		}
		c.Close()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		t.Errorf("handshake: %v", err)
	}
}

// 파일 덮어쓰기 헬퍼.
func writeOver(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
