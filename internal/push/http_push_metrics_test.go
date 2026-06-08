package push

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/winwaysystems/wtg/pkg/tlsutil"
)

// TestHTTPPushHandler_OnInjectHook — Phase 2.5 metrics hook 호출 검증.
//
// 4 시나리오: ok / unauthorized / bad_json / (inject_full 은 dispatcher full 환경 어려워 생략)
func TestHTTPPushHandler_OnInjectHook(t *testing.T) {
	disp := NewDispatcher(DispatcherOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	var mu sync.Mutex
	calls := []string{} // "cn|result" 기록.
	hook := func(cn, result string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, cn+"|"+result)
	}

	srv := httptest.NewServer(HTTPPushHandlerDeps(HTTPPushDeps{
		Dispatcher: disp,
		Secret:     "topsecret",
		Logger:     nil,
		OnInject:   hook,
	}))
	defer srv.Close()

	// 1. ok — secret 일치 + 정상 body.
	postJSON(t, srv.URL, `{"data":"x"}`, http.Header{"X-Push-Secret": []string{"topsecret"}}, http.StatusOK)
	// 2. unauthorized — secret 불일치.
	postJSON(t, srv.URL, `{"data":"x"}`, http.Header{"X-Push-Secret": []string{"wrong"}}, http.StatusUnauthorized)
	// 3. bad_json — body 깨짐.
	postJSON(t, srv.URL, `not-json`, http.Header{"X-Push-Secret": []string{"topsecret"}}, http.StatusBadRequest)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"anonymous|ok", "anonymous|unauthorized", "anonymous|bad_json"}
	if len(calls) != len(want) {
		t.Fatalf("hook 호출 수=%d, 기대 %d. calls=%v", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d]=%q, 기대 %q", i, calls[i], w)
		}
	}
}

// TestHTTPPushHandler_OnInjectHookMTLS — mTLS 시 CN 이 label 로 propagate.
func TestHTTPPushHandler_OnInjectHookMTLS(t *testing.T) {
	srvSS := mustSelfSignPushMetric(t, "wtg-push-metric")
	cliSS := mustSelfSignPushMetric(t, "order-engine-prod")

	clientPool := x509.NewCertPool()
	_ = clientPool.AppendCertsFromPEM(cliSS.CertPEM)
	serverCert, _ := tls.X509KeyPair(srvSS.CertPEM, srvSS.KeyPEM)

	disp := NewDispatcher(DispatcherOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	var mu sync.Mutex
	var observedCN, observedResult string
	hook := func(cn, result string) {
		mu.Lock()
		defer mu.Unlock()
		observedCN, observedResult = cn, result
	}

	srv := httptest.NewUnstartedServer(HTTPPushHandlerDeps(HTTPPushDeps{
		Dispatcher: disp,
		OnInject:   hook,
	}))
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
				ServerName:   "wtg-push-metric",
				MinVersion:   tls.VersionTLS12,
			},
		},
	}
	req, _ := http.NewRequest("POST", srv.URL+"/", strings.NewReader(`{"data":"x"}`))
	resp, err := httpCli.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if observedCN != "order-engine-prod" {
		t.Errorf("hook CN=%q (기대 order-engine-prod)", observedCN)
	}
	if observedResult != "ok" {
		t.Errorf("hook result=%q (기대 ok)", observedResult)
	}
}

func postJSON(t *testing.T, url, body string, header http.Header, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/", strings.NewReader(body))
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		buf, _ := io.ReadAll(resp.Body)
		t.Errorf("status=%d 기대 %d body=%s", resp.StatusCode, wantStatus, buf)
	}
	// drain — keep-alive 위해.
	_, _ = io.Copy(io.Discard, resp.Body)
}

func mustSelfSignPushMetric(t *testing.T, cn string) *tlsutil.SelfSigned {
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

// 단순 verify — json marshal/unmarshal 의존 회피.
var _ = json.Marshal
