package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/routing"
)

func newRouteDeps() (*RoutingDeps, routing.Registry) {
	reg := routing.NewInMemoryRegistry(nil)
	return &RoutingDeps{Registry: reg, Logger: quietLogger()}, reg
}

// 인증된 admin 으로 가장한 요청 — UpdatedBy 가 채워지는지도 확인.
func authReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.ContentLength = int64(len(body))
	}
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:    "admin01",
		Channel: "ADMIN",
	}))
	return req
}

func decodeRule(t *testing.T, rr *httptest.ResponseRecorder) *routing.Rule {
	t.Helper()
	var rule routing.Rule
	if err := json.NewDecoder(rr.Body).Decode(&rule); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	return &rule
}

// PUT — 생성 흐름 + 응답에 UpdatedAt/By 포함.
func TestPutRouteCreates(t *testing.T) {
	deps, reg := newRouteDeps()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/admin/routes/{alias}", PutRoute(deps))

	rr := httptest.NewRecorder()
	req := authReq(http.MethodPut, "/v1/admin/routes/ORDER_NEW",
		`{"exchange":"ORDER","routing_key":"NEW","active":true,"comment":"v1"}`)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rule := decodeRule(t, rr)
	if rule.Alias != "ORDER_NEW" || rule.Exchange != "ORDER" || rule.RoutingKey != "NEW" {
		t.Errorf("rule: %+v", rule)
	}
	if !rule.Active || rule.Comment != "v1" {
		t.Errorf("active/comment: %+v", rule)
	}
	if rule.UpdatedBy != "admin01" {
		t.Errorf("UpdatedBy: %q", rule.UpdatedBy)
	}
	if rule.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 누락")
	}

	// 저장소에 실제 반영 확인.
	got, err := reg.Get("ORDER_NEW")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RoutingKey != "NEW" {
		t.Errorf("저장소: %+v", got)
	}
}

// PUT — 잘못된 본문 거부.
func TestPutRouteValidation(t *testing.T) {
	deps, _ := newRouteDeps()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/admin/routes/{alias}", PutRoute(deps))

	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"빈 routing_key", "/v1/admin/routes/A", `{"exchange":"X"}`, http.StatusBadRequest},
		{"bad json", "/v1/admin/routes/A", `{not json`, http.StatusBadRequest},
		{"alias 형식 위반", "/v1/admin/routes/A%20B", `{"routing_key":"K"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, authReq(http.MethodPut, tc.path, tc.body))
			if rr.Code != tc.want {
				t.Errorf("status=%d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// GET — 단일 조회 + 미존재.
func TestGetRoute(t *testing.T) {
	deps, reg := newRouteDeps()
	reg.Put(&routing.Rule{Alias: "A", RoutingKey: "K", Active: true}, "u")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/routes/{alias}", GetRoute(deps))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authReq(http.MethodGet, "/v1/admin/routes/A", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if r := decodeRule(t, rr); r.RoutingKey != "K" {
		t.Errorf("rule: %+v", r)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, authReq(http.MethodGet, "/v1/admin/routes/MISSING", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

// GET 목록 — 정렬 결과 검증.
func TestListRoutes(t *testing.T) {
	deps, reg := newRouteDeps()
	for _, a := range []string{"C", "A", "B"} {
		reg.Put(&routing.Rule{Alias: a, RoutingKey: "K"}, "u")
	}
	rr := httptest.NewRecorder()
	ListRoutes(deps)(rr, authReq(http.MethodGet, "/v1/admin/routes", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out struct {
		Rules []routing.Rule `json:"rules"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Rules) != 3 {
		t.Fatalf("len=%d", len(out.Rules))
	}
	if out.Rules[0].Alias != "A" || out.Rules[2].Alias != "C" {
		t.Errorf("정렬: %v", out.Rules)
	}
}

// DELETE — 정상 + 미존재.
func TestDeleteRoute(t *testing.T) {
	deps, reg := newRouteDeps()
	reg.Put(&routing.Rule{Alias: "X", RoutingKey: "K"}, "u")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/admin/routes/{alias}", DeleteRoute(deps))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authReq(http.MethodDelete, "/v1/admin/routes/X", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if _, err := reg.Get("X"); err == nil {
		t.Error("삭제되지 않음")
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, authReq(http.MethodDelete, "/v1/admin/routes/X", ""))
	if rr.Code != http.StatusNotFound {
		t.Errorf("재삭제 status=%d, want 404", rr.Code)
	}
}

// POST /active — 토글 + 갱신된 룰 반환.
func TestSetRouteActive(t *testing.T) {
	deps, reg := newRouteDeps()
	reg.Put(&routing.Rule{Alias: "A", RoutingKey: "K", Active: true}, "u")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/routes/{alias}/active", SetRouteActive(deps))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authReq(http.MethodPost, "/v1/admin/routes/A/active", `{"active":false}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rule := decodeRule(t, rr)
	if rule.Active {
		t.Error("비활성화 안됨")
	}
	if rule.UpdatedBy != "admin01" {
		t.Errorf("UpdatedBy: %q", rule.UpdatedBy)
	}
}

// 미설정 Registry 면 503.
func TestNoRegistryServiceUnavailable(t *testing.T) {
	deps := &RoutingDeps{Registry: nil, Logger: quietLogger()}
	rr := httptest.NewRecorder()
	ListRoutes(deps)(rr, authReq(http.MethodGet, "/v1/admin/routes", ""))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
}
