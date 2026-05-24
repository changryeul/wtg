package price

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// TestHTTPS_ReloaderServerConfig — Server.startHTTP 의 TLS 분기와 동일 패턴.
// tlsutil.Reloader.ServerConfig 가 http.Server.TLSConfig 에 들어가 정상
// listen / handshake 됨을 검증. broker 의존성 우회.
func TestHTTPS_ReloaderServerConfig(t *testing.T) {
	certPath, keyPath := mkServerCert(t)

	rl, err := tlsutil.NewReloader(tlsutil.ReloaderOptions{
		CertFile: certPath,
		KeyFile:  keyPath,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	port := freePort(t)
	addr := "127.0.0.1:" + port
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		TLSConfig:    rl.ServerConfig(),
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	go func() {
		// "" "" — TLSConfig 의 GetCertificate (Reloader) 사용.
		_ = srv.ListenAndServeTLS("", "")
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	waitDial(t, addr)

	caPEM, _ := os.ReadFile(certPath)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}

	resp, err := client.Get("https://" + addr + "/v1/ping")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status=%d", resp.StatusCode)
	}

	// plain HTTP 시도 — TLS 서버가 400 (Go 의 표준 동작) 반환. 200 면 안 됨.
	plain := &http.Client{Timeout: 1500 * time.Millisecond}
	plainResp, perr := plain.Get("http://" + addr + "/v1/ping")
	if perr == nil {
		defer plainResp.Body.Close()
		if plainResp.StatusCode == 200 {
			t.Error("plain HTTP 가 TLS 서버에 200 — 보안 회귀")
		}
	}
}

// TestHTTPS_mTLS — ClientCAFile 채워지면 클라이언트 인증서 없는 호출 reject.
func TestHTTPS_mTLS(t *testing.T) {
	srvCert, srvKey := mkServerCert(t)
	clientCert, _ := mkServerCert(t)

	rl, err := tlsutil.NewReloader(tlsutil.ReloaderOptions{
		CertFile:     srvCert,
		KeyFile:      srvKey,
		ClientCAFile: clientCert, // 클라이언트 인증을 별도 CA 로 검증
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	})

	port := freePort(t)
	addr := "127.0.0.1:" + port
	srv := &http.Server{Addr: addr, Handler: mux, TLSConfig: rl.ServerConfig()}
	go func() { _ = srv.ListenAndServeTLS("", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	waitDial(t, addr)

	// 클라이언트 인증서 없이 호출 — mTLS reject.
	caPEM, _ := os.ReadFile(srvCert)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	noCertClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
	if _, err := noCertClient.Get("https://" + addr + "/v1/ping"); err == nil {
		t.Error("mTLS 인데 클라이언트 cert 없이 통과")
	}
}

// helpers ---

func mkServerCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	ss, err := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		CommonName: "localhost",
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, ss.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, ss.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return port
}

func waitDial(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitDial %s timeout", addr)
}
