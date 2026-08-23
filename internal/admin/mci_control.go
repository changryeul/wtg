package admin

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// mci_control.go — MCI 패널 프로세스 제어 (POST /v1/admin/mci/{name}/{action}).
//
// start/stop/restart 를 `sudo systemctl <action> wtg-<unit>` 로 실행한다. 파괴적
// 액션이므로 다중 가드:
//   - cfg.EnableProcessControl (기본 off) 로 기능 자체를 게이트.
//   - **코드 내 고정 allowlist** (controllableUnits) — path param 은 테이블 키
//     조회에만 사용, unit 문자열을 입력으로 조립하지 않는다 (명령 인젝션 차단).
//   - etcd(SoT)·mci-admin(self) 은 allowlist 에서 제외 (내리면 전 시스템/콘솔 붕괴).
//   - 모든 액션을 audit ring 에 기록 (usid/rid 포함).
//   - 접근 IP 제한(AllowCIDRs)·인증은 상위 미들웨어가 담당.
//
// 권한: mci-admin 은 winway(비-root)로 돌므로 scoped sudoers 선행 필수
// (deploy/ec2/wtg-admin.sudoers). 와일드카드 sudo 금지.

// controllableUnits — 제어 허용 서비스의 name→systemd unit 고정 매핑.
// etcd/mci-admin 은 의도적으로 없음 (제어 금지). 신규 서비스는 여기 명시 등록해야
// 패널에서 제어 가능 — 문자열 조합("wtg-"+name)을 쓰지 않는 이유(안전).
var controllableUnits = map[string]string{
	"mci-api":          "wtg-mci-api.service",
	"mci-price":        "wtg-mci-price.service",
	"mci-price-krx":    "wtg-mci-price-krx.service",
	"mci-push":         "wtg-mci-push.service",
	"quote-forwarder":  "wtg-quote-forwarder.service",
	"mci-edge-api":     "wtg-mci-edge-api.service",
	"mci-edge-tcp":     "wtg-mci-edge-tcp.service",
	"mci-edge-fix-ord": "wtg-mci-edge-fix-ord.service",
	"mci-edge-fix-md":  "wtg-mci-edge-fix-md.service",
	"mci-edge-price":   "wtg-mci-edge-price.service",
	"mci-edge-push":    "wtg-mci-edge-push.service",
}

// controllableActions — 허용 액션. reload/SIGHUP 은 미핸들러 프로세스를 죽일 수
// 있어 v1 제외 (설정반영은 restart 로; etcd 기반 설정은 이미 hot-reload 자동).
var controllableActions = map[string]struct{}{
	"start":   {},
	"stop":    {},
	"restart": {},
}

// ControllableUnit — name 이 제어 가능하면 unit 과 true 반환.
func ControllableUnit(name string) (string, bool) {
	u, ok := controllableUnits[name]
	return u, ok
}

// ControllableNames — 제어 허용 서비스 name 집합 (정렬). UI/health 주석용.
func ControllableNames() []string {
	out := make([]string, 0, len(controllableUnits))
	for n := range controllableUnits {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// MciControlDeps — 제어 핸들러 의존성.
type MciControlDeps struct {
	Logger  *slog.Logger
	Audit   *AuditRing
	Enabled bool // cfg.EnableProcessControl
	// runner — systemctl 실행기 (테스트 주입용). nil 이면 실제 sudo systemctl.
	runner func(ctx context.Context, action, unit string) (string, error)
}

// systemctlTimeout — 단일 제어 명령 상한. restart 가 늦어도 되도록 넉넉히.
const systemctlTimeout = 25 * time.Second

// defaultSystemctlRunner — 실제 `sudo systemctl <action> <unit>` 실행.
func defaultSystemctlRunner(ctx context.Context, action, unit string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	// unit/action 은 호출부에서 이미 allowlist 검증됨 — 여기서 재조합/입력 사용 없음.
	cmd := exec.CommandContext(cctx, "sudo", "-n", "systemctl", action, unit)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// MciControl — POST /v1/admin/mci/{name}/{action}.
func MciControl(deps *MciControlDeps) http.HandlerFunc {
	run := deps.runner
	if run == nil {
		run = defaultSystemctlRunner
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.Enabled {
			writeJSONError(w, http.StatusForbidden, "disabled",
				"프로세스 제어 비활성 (--enable-process-control 필요)")
			return
		}
		name := strings.TrimSpace(r.PathValue("name"))
		action := strings.TrimSpace(r.PathValue("action"))

		if _, ok := controllableActions[action]; !ok {
			writeJSONError(w, http.StatusBadRequest, "bad_action",
				"허용되지 않은 액션: "+action+" (start|stop|restart)")
			return
		}
		unit, ok := ControllableUnit(name)
		if !ok {
			// etcd/mci-admin/미등록 — 제어 금지.
			writeJSONError(w, http.StatusForbidden, "not_controllable",
				"제어 대상 아님: "+name+" (etcd/mci-admin 및 미등록 서비스는 금지)")
			return
		}

		out, err := run(r.Context(), action, unit)
		if err != nil {
			// audit 는 실패도 기록 (감사 완결성).
			emitAudit(deps.Logger, deps.Audit, r, "mci_process", "MCI_"+strings.ToUpper(action)+"_FAIL",
				slog.String("unit", unit), slog.String("error", err.Error()), slog.String("output", out))
			writeJSONError(w, http.StatusInternalServerError, "systemctl_failed",
				"systemctl "+action+" "+unit+" 실패: "+err.Error()+" — "+out)
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "mci_process", "MCI_"+strings.ToUpper(action),
			slog.String("unit", unit))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"name":   name,
			"unit":   unit,
			"action": action,
			"output": out,
		})
	}
}
