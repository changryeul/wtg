//go:build cside

// cside_e2e_test.go — C SDK (cside/wtgpush/sample) 의 wire 호환성 검증.
//
// 실행:
//   make -C cside/wtgpush
//   go test -tags=cside ./pkg/push/... -run CSide -v
//
// build tag 로 분리 — CI 의 기본 test 에선 skip (C 빌드 의존 X).
// sample 바이너리가 cside/wtgpush/sample 에 있어야 함.
package push

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func samplePath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// pkg/push → repo root → cside/wtgpush/sample.
	p := filepath.Join(wd, "..", "..", "cside", "wtgpush", "sample")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("sample 바이너리 없음 (%s) — make -C cside/wtgpush 먼저 실행. err: %v", p, err)
	}
	return p
}

// TestCSide_UserSend — sample 으로 user-targeted push → mock server 가 envelope 검증.
func TestCSide_UserSend(t *testing.T) {
	sample := samplePath(t)

	var received atomic.Value // *Message (envelope)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Push-Secret"); got != "mysecret" {
			http.Error(w, "wrong secret: "+got, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/internal/push" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		var env Message
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received.Store(&env)
		_ = json.NewEncoder(w).Encode(Result{Injected: true, User: env.User})
	}))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "mysecret", "dealer01",
		`{"orderId":123,"price":1.0850}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample exec: %v\nout: %s", err, out)
	}
	t.Logf("sample stderr: %s", out)

	env, _ := received.Load().(*Message)
	if env == nil {
		t.Fatal("mock server 가 envelope 미수신")
	}
	if env.User != "dealer01" {
		t.Errorf("user=%q (기대 dealer01)", env.User)
	}
	var dataObj map[string]interface{}
	if err := json.Unmarshal(env.Data, &dataObj); err != nil {
		t.Errorf("data JSON parse: %v (raw=%s)", err, env.Data)
	}
	if dataObj["orderId"] != float64(123) {
		t.Errorf("data.orderId=%v (기대 123)", dataObj["orderId"])
	}
}

// TestCSide_Broadcast — user 빈 → broadcast envelope.
func TestCSide_Broadcast(t *testing.T) {
	sample := samplePath(t)

	var received atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env Message
		_ = json.NewDecoder(r.Body).Decode(&env)
		received.Store(&env)
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "mysecret", "", `{"market":"HALT"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sample exec: %v\nout: %s", err, out)
	}
	t.Logf("sample stderr: %s", out)

	env, _ := received.Load().(*Message)
	if env == nil {
		t.Fatal("envelope 미수신")
	}
	if env.User != "" {
		t.Errorf("broadcast 인데 user=%q (기대 빈)", env.User)
	}
}

// TestCSide_AuthFail — 잘못된 secret → HTTP 401, sample exit 1.
func TestCSide_AuthFail(t *testing.T) {
	sample := samplePath(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Push-Secret") != "mysecret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(Result{Injected: true})
	}))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)
	cmd := exec.Command(sample, host, port, "wrong-secret", "dealer01", `{"x":1}`)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("auth 실패 시 exit 1 기대 (got 0). out: %s", out)
	}
	if !strings.Contains(string(out), "http=401") {
		t.Errorf("stderr 에 http=401 기대: %s", out)
	}
}

func parseHostPort(t *testing.T, raw string) (string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url parse: %v", err)
	}
	host := u.Hostname()
	port := u.Port()
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("port atoi: %v (port=%s)", err, port)
	}
	return host, port
}
