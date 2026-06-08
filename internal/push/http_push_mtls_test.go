package push

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// TestHTTPPushHandler_MTLSAuditLog — mTLS client cert 가 있으면 핸들러가
// peer CN 을 audit log 에 기록.
func TestHTTPPushHandler_MTLSAuditLog(t *testing.T) {
	srvSS := mustSelfSignPush(t, "wtg-push-test")
	cliSS := mustSelfSignPush(t, "wtg-svc-test")

	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(cliSS.CertPEM) {
		t.Fatal("client pool")
	}
	serverCert, err := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}

	disp := NewDispatcher(DispatcherOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Run 없이 Inject 만 호출 — 채널은 buffered 이므로 1건 OK.

	// audit log 캡처 buffer.
	var logBuf bytes.Buffer
	auditLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv := httptest.NewUnstartedServer(HTTPPushHandlerWithLogger(disp, "", auditLogger))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	cliCert, _ := tls.X509KeyPair(cliSS.CertPEM, cliSS.KeyPEM)
	serverPool := x509.NewCertPool()
	_ = serverPool.AppendCertsFromPEM(srvSS.CertPEM)
	httpCli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cliCert},
				RootCAs:      serverPool,
				ServerName:   "wtg-push-test",
				MinVersion:   tls.VersionTLS12,
			},
		},
	}

	req, err := http.NewRequest("POST", srv.URL+"/", strings.NewReader(`{"data":"x"}`))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpCli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var out HTTPPushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Injected {
		t.Errorf("Injected=true 기대")
	}
	logStr := logBuf.String()
	if !strings.Contains(logStr, "wtg-svc-test") {
		t.Errorf("audit log 에 client CN 'wtg-svc-test' 없음: %s", logStr)
	}
	if !strings.Contains(logStr, "push: mTLS client") {
		t.Errorf("audit log 에 'push: mTLS client' 메시지 없음: %s", logStr)
	}
}

// TestHTTPPushHandler_SecretAndNoTLS — 기존 secret 인증 path backward compat.
func TestHTTPPushHandler_SecretAndNoTLS(t *testing.T) {
	disp := NewDispatcher(DispatcherOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(HTTPPushHandlerWithLogger(disp, "topsecret", nil))
	defer srv.Close()

	// secret 일치 — 통과.
	req, _ := http.NewRequest("POST", srv.URL+"/", strings.NewReader(`{"data":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Push-Secret", "topsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("secret OK 시 200 기대, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// secret 불일치 — 401.
	req2, _ := http.NewRequest("POST", srv.URL+"/", strings.NewReader(`{"data":"x"}`))
	req2.Header.Set("X-Push-Secret", "wrong")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do2: %v", err)
	}
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("secret 불일치 시 401 기대, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func mustSelfSignPush(t *testing.T, cn string) *tlsutil.SelfSigned {
	t.Helper()
	ss, err := tlsutil.GenerateSelfSigned(tlsutil.SelfSignedOptions{
		CommonName: cn,
		DNSNames:   []string{cn, "localhost"},
	})
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}
	return ss
}

// dummy — os.WriteFile 가 사용되지 않아도 import 가 비어있지 않게 (test helpers 패턴).
var _ = os.O_RDONLY
