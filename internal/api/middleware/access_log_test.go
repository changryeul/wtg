package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AccessLog 미들웨어가 status code, path, method, duration, request id 를
// 모두 구조화 로그에 담는지 검증.

func TestAccessLogCapturesStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	mw := AccessLog(logger)

	// 명시적으로 WriteHeader 호출하는 핸들러.
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("User-Agent", "wtg-test/1.0")
	h.ServeHTTP(rr, req)

	out := buf.String()
	for _, want := range []string{
		"method=POST",
		"path=/v1/orders",
		"status=418",
		"ua=wtg-test/1.0",
		"msg=http",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("로그에 %q 없음, got:\n%s", want, out)
		}
	}
}

func TestAccessLogDefaultStatusOK(t *testing.T) {
	// WriteHeader 안 부르고 곧장 Write 만 하면 status 200 으로 기록되어야 함.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	mw := AccessLog(logger)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	h.ServeHTTP(rr, req)

	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("기본 status 200 누락:\n%s", buf.String())
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Errorf("body passthrough: %q", rr.Body.String())
	}
}

func TestAccessLogIncludesRequestID(t *testing.T) {
	// AccessLog 미들웨어가 request id 를 함께 기록한다 (RequestID 미들웨어와
	// 함께 체인 구성 시).
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	chain := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
		AccessLog(logger),
		RequestID(),
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("X-Request-ID", "rid-abc-123")
	chain.ServeHTTP(rr, req)

	if !strings.Contains(buf.String(), "rid=rid-abc-123") {
		t.Errorf("request id 누락:\n%s", buf.String())
	}
}

// statusRecorder 의 WriteHeader 가 두 번 호출돼도 첫 status 만 기록.
func TestStatusRecorderIdempotentWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &statusRecorder{ResponseWriter: rec, status: 200}
	rw.WriteHeader(http.StatusBadRequest)
	rw.WriteHeader(http.StatusInternalServerError) // 무시되어야 함
	if rw.status != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rw.status)
	}
}

// ContextWithPrincipal 라운드트립.
func TestContextWithPrincipal(t *testing.T) {
	p := &Principal{Usid: "trader07", Channel: "WEB"}
	ctx := ContextWithPrincipal(req(t).Context(), p)
	got := PrincipalFromContext(ctx)
	if got == nil || got.Usid != "trader07" {
		t.Errorf("Principal 추출 실패: %+v", got)
	}
}

func req(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/", nil)
}

// 운영 모드 (DevMode=false) 에서는 항상 미구현 에러 → 401.
func TestAuthProductionNotImplemented(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	mw := Auth(AuthConfig{DevMode: false, Logger: logger})

	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", nil)
	req.Header.Set("X-WTG-User", "trader01") // 헤더 있어도 운영 모드라 무시.
	h.ServeHTTP(rr, req)

	if called {
		t.Error("운영 모드에서 핸들러가 호출되면 안 됨 (Phase 3 까지 미구현)")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", rr.Code)
	}
	if !strings.Contains(buf.String(), "인증 실패") {
		t.Errorf("실패 로그 누락:\n%s", buf.String())
	}
}

// healthz / readyz 도 인증 우회.
func TestAuthHealthCheckPathsBypass(t *testing.T) {
	mw := Auth(AuthConfig{DevMode: false})
	for _, path := range []string{"/healthz", "/readyz", "/v1/ping"} {
		called := false
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rr, req)
		if !called {
			t.Errorf("%s 인증 우회 안 됨", path)
		}
	}
}
