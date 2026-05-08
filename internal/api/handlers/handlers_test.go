package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/internal/api/transform"
	"github.com/winwaysystems/wtg/pkg/mymq"
)

func TestMapBrokerError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"auth", &mymq.Error{Errn: mymq.ErrAuth}, http.StatusUnauthorized, "auth"},
		{"timeout", &mymq.Error{Errn: mymq.ErrTimeout}, http.StatusGatewayTimeout, "timeout"},
		{"svc timeout", &mymq.Error{Errn: mymq.ErrSvcTimeout}, http.StatusGatewayTimeout, "timeout"},
		{"no svc", &mymq.Error{Errn: mymq.ErrNoSvc}, http.StatusBadRequest, "no_service"},
		{"too big", &mymq.Error{Errn: mymq.ErrTooBig}, http.StatusRequestEntityTooLarge, "too_big"},
		{"bad arg", &mymq.Error{Errn: mymq.ErrBadArg}, http.StatusBadRequest, "bad_argument"},
		{"reconnecting", mymq.ErrReconnecting, http.StatusServiceUnavailable, "reconnecting"},
		{"closed", mymq.ErrClientClosed, http.StatusServiceUnavailable, "broker_unavailable"},
		{"other broker", &mymq.Error{Errn: mymq.ErrBusy}, http.StatusUnprocessableEntity, "broker_error"},
		{"unknown", errors.New("network"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, code, _ := mapBrokerError(c.err)
			if status != c.status {
				t.Errorf("status: got %d, want %d", status, c.status)
			}
			if code != c.code {
				t.Errorf("code: got %q, want %q", code, c.code)
			}
		})
	}
}

// Transaction 핸들러는 mymq.Client 의존성이 필요해서 본격 테스트는
// integration test 에서. 여기서는 입력 검증/Principal 체크만 단위 테스트.

func TestTransactionRejectsUnauthenticated(t *testing.T) {
	deps := &Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := Transaction(deps)
	req := httptest.NewRequest(http.MethodPost, "/v1/tx",
		strings.NewReader(`{"routing_key":"NEW","data":{}}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", rr.Code)
	}
}

func TestTransactionRejectsBadJSON(t *testing.T) {
	deps := &Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := Transaction(deps)
	req := httptest.NewRequest(http.MethodPost, "/v1/tx",
		strings.NewReader(`{not json`))
	req = req.WithContext(withTestPrincipal(req.Context(), "trader01"))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rr.Code)
	}
}

func TestTransactionRejectsMissingRoutingKey(t *testing.T) {
	deps := &Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := Transaction(deps)
	body, _ := json.Marshal(transform.Envelope{Exchange: "ORDER"})
	req := httptest.NewRequest(http.MethodPost, "/v1/tx", strings.NewReader(string(body)))
	req = req.WithContext(withTestPrincipal(req.Context(), "trader01"))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rr.Code)
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["error"] != "validation" {
		t.Errorf("error code: %q", resp["error"])
	}
}

// withTestPrincipal 는 테스트용 Principal 을 context 에 주입한다.
// 인증 미들웨어 통과 후 상태를 시뮬레이션.
func withTestPrincipal(ctx context.Context, usid string) context.Context {
	return middleware.ContextWithPrincipal(ctx, &middleware.Principal{
		Usid:    usid,
		Channel: "WEB",
	})
}

// 위 헬퍼와 별도로, Transaction 핸들러를 미들웨어와 함께 묶어 통합 검증.
func TestTransactionWithDevAuthMiddleware(t *testing.T) {
	deps := &Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	authMW := middleware.Auth(middleware.AuthConfig{DevMode: true, Logger: deps.Logger})
	h := authMW(Transaction(deps))

	body, _ := json.Marshal(transform.Envelope{}) // routing_key 없음 → validation 에러
	req := httptest.NewRequest(http.MethodPost, "/v1/tx", strings.NewReader(string(body)))
	req.Header.Set("X-WTG-User", "trader01")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// 인증은 통과해야 하고 (401 X), validation 단계에서 거부.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400 (인증 통과 후 validation)", rr.Code)
	}
}
