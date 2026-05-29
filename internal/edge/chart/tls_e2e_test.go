package chart

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// HTTPS server + mTLS — 외부 client ↔ edge-chart 종단 검증.
//
// chart server.go 의 Start() 가 ListenAndServeTLS + Reloader 로 wiring 하는데,
// 본 테스트는 동일 cert/key/CA 로 httptest.NewUnstartedServer + StartTLS 를
// 띄워 wire 수준에서 mTLS handshake + 정상 응답이 가능한지 확인.

// helper — 자체발급 cert 를 disk 에 쓰고 paths 반환.
func writeSelfSignedTLS(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	ss, err := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		CommonName: "wtg-edge-chart-test",
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cp, kp, err := ss.WriteToFiles(dir, "tls")
	if err != nil {
		t.Fatal(err)
	}
	return cp, kp
}

func TestEdgeChart_TLS_mTLS_EndToEnd(t *testing.T) {
	certPath, keyPath := writeSelfSignedTLS(t)

	// server 측 TLS — chart 의 tlsutil.NewReloader 와 동일 옵션.
	srvTLS, err := tlsutil.LoadServer(tlsutil.ServerOptions{
		CertFile:     certPath,
		KeyFile:      keyPath,
		ClientCAFile: certPath, // self-signed 가 자기 자신을 client CA 로 신뢰
	})
	if err != nil {
		t.Fatal(err)
	}

	up := newFakeUpstream(t, nil)
	cfg := DefaultConfig()
	cfg.UpstreamURL = up.URL
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := s.BuildHandler()

	// HTTPS server 띄우고 chart handler 그대로 사용.
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = srvTLS
	ts.StartTLS()
	defer ts.Close()

	// client mTLS — 동일 cert/key + CA.
	caBytes, _ := os.ReadFile(certPath)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caBytes)
	cliCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS := &tls.Config{
		Certificates: []tls.Certificate{cliCert},
		RootCAs:      caPool,
		ServerName:   "localhost",
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliTLS},
	}

	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("mTLS GET 실패: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status=%d, want 200", resp.StatusCode)
	}
}

// 서버가 mTLS 요구하는데 client cert 없이 dial → handshake 실패.
func TestEdgeChart_TLS_mTLS_RejectsNoClientCert(t *testing.T) {
	certPath, keyPath := writeSelfSignedTLS(t)

	srvTLS, err := tlsutil.LoadServer(tlsutil.ServerOptions{
		CertFile: certPath, KeyFile: keyPath, ClientCAFile: certPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	up := newFakeUpstream(t, nil)
	cfg := DefaultConfig()
	cfg.UpstreamURL = up.URL
	cfg.DevMode = true
	cfg.IPRatePerSec = 0
	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h, _ := s.BuildHandler()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = srvTLS
	ts.StartTLS()
	defer ts.Close()

	// client cert 없이 RootCAs 만 신뢰
	caBytes, _ := os.ReadFile(certPath)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caBytes)
	cliTLS := &tls.Config{RootCAs: caPool, ServerName: "localhost"}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}}

	if _, err := client.Get(ts.URL + "/healthz"); err == nil {
		t.Error("client cert 없는데 handshake 통과")
	}
}
