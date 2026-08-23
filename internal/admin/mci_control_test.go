package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 라우트 패턴으로 서빙해 PathValue({name}/{action})가 채워지게 한다.
func serveCtl(h http.HandlerFunc, method, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/mci/{name}/{action}", h)
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMciControl_DisabledGate(t *testing.T) {
	h := MciControl(&MciControlDeps{Enabled: false})
	rec := serveCtl(h, "POST", "/v1/admin/mci/mci-price/restart")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

func TestMciControl_Allowlist(t *testing.T) {
	called := false
	deps := &MciControlDeps{
		Enabled: true,
		runner: func(ctx context.Context, action, unit string) (string, error) {
			called = true
			return "ok", nil
		},
	}
	h := MciControl(deps)

	// etcd — 제어 금지 (allowlist 제외).
	if rec := serveCtl(h, "POST", "/v1/admin/mci/etcd/stop"); rec.Code != http.StatusForbidden {
		t.Errorf("etcd stop code=%d want 403", rec.Code)
	}
	// mci-admin(self) — 제어 금지.
	if rec := serveCtl(h, "POST", "/v1/admin/mci/mci-admin/restart"); rec.Code != http.StatusForbidden {
		t.Errorf("mci-admin restart code=%d want 403", rec.Code)
	}
	// 미등록 서비스 — 금지.
	if rec := serveCtl(h, "POST", "/v1/admin/mci/nope/start"); rec.Code != http.StatusForbidden {
		t.Errorf("nope start code=%d want 403", rec.Code)
	}
	// 나쁜 액션 — 400.
	if rec := serveCtl(h, "POST", "/v1/admin/mci/mci-price/nuke"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad action code=%d want 400", rec.Code)
	}
	if called {
		t.Error("거부 케이스에서 runner 가 호출됨 (systemctl 실행 누출)")
	}
}

func TestMciControl_SuccessAndAudit(t *testing.T) {
	var gotAction, gotUnit string
	ring := NewAuditRing(10)
	deps := &MciControlDeps{
		Enabled: true,
		Audit:   ring,
		runner: func(ctx context.Context, action, unit string) (string, error) {
			gotAction, gotUnit = action, unit
			return "Job for wtg-mci-price.service ...", nil
		},
	}
	h := MciControl(deps)
	rec := serveCtl(h, "POST", "/v1/admin/mci/mci-price/restart")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// runner 가 정확한 unit/action 으로 호출됐나 (allowlist 매핑 검증).
	if gotAction != "restart" || gotUnit != "wtg-mci-price.service" {
		t.Errorf("runner(action=%q unit=%q) want restart/wtg-mci-price.service", gotAction, gotUnit)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true || resp["unit"] != "wtg-mci-price.service" {
		t.Errorf("resp=%v", resp)
	}
	// audit 기록 확인.
	entries := ring.List(10)
	if len(entries) != 1 || entries[0].Action != "MCI_RESTART" || entries[0].Resource != "mci_process" {
		t.Fatalf("audit=%+v want 1× MCI_RESTART/mci_process", entries)
	}
}

func TestMciControl_FailureAudited(t *testing.T) {
	ring := NewAuditRing(10)
	deps := &MciControlDeps{
		Enabled: true,
		Audit:   ring,
		runner: func(ctx context.Context, action, unit string) (string, error) {
			return "Failed to restart", context.DeadlineExceeded
		},
	}
	h := MciControl(deps)
	rec := serveCtl(h, "POST", "/v1/admin/mci/mci-price/restart")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d want 500", rec.Code)
	}
	entries := ring.List(10)
	if len(entries) != 1 || entries[0].Action != "MCI_RESTART_FAIL" {
		t.Fatalf("audit=%+v want MCI_RESTART_FAIL", entries)
	}
}

// allowlist 에 etcd/mci-admin 이 없어야 (설계 불변식).
func TestControllableUnits_ExcludesInfraAndSelf(t *testing.T) {
	if _, ok := ControllableUnit("etcd"); ok {
		t.Error("etcd 가 제어 allowlist 에 있음 (SoT — 금지여야)")
	}
	if _, ok := ControllableUnit("mci-admin"); ok {
		t.Error("mci-admin(self) 이 제어 allowlist 에 있음 (금지여야)")
	}
	if u, ok := ControllableUnit("mci-edge-price"); !ok || u != "wtg-mci-edge-price.service" {
		t.Errorf("mci-edge-price 매핑 오류: %q %v", u, ok)
	}
}
