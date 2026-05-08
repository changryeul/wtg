package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/winwaysystems/wtg/pkg/policy"
)

// PolicyDeps — admin 측 정책 핸들러 의존성.
type PolicyDeps struct {
	Engine *policy.Engine
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub // ws 브로드캐스트 (옵셔널)
}

// GetPolicy — GET /v1/admin/policy : 현재 정책 상태 노출.
func GetPolicy(deps *PolicyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Engine == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no_engine"})
			return
		}
		writeJSON(w, http.StatusOK, deps.Engine.State())
	}
}

// killSwitchRequest — POST /v1/admin/policy/kill-switch
//
// channels 가 비어있으면 전체 차단 (legacy). 비어있지 않으면 그 채널만 차단
// — 예: 사고 시 ["WEB","MOB","HTS"] 로 고객만 막고 직원 거래 유지.
type killSwitchRequest struct {
	Active   bool     `json:"active"`
	Channels []string `json:"channels,omitempty"`
}

// SetKillSwitch — POST /v1/admin/policy/kill-switch
func SetKillSwitch(deps *PolicyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Engine == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_engine", "정책 엔진 없음")
			return
		}
		var req killSwitchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		usid := principalUsid(r)
		deps.Engine.SetKillSwitchScoped(req.Active, req.Channels, usid)
		policyAudit(deps, r, "POLICY_KILL_SWITCH",
			slog.Bool("active", req.Active),
			slog.Any("channels", req.Channels),
		)
		writeJSON(w, http.StatusOK, deps.Engine.State())
	}
}

// maintenanceRequest — POST /v1/admin/policy/maintenance
type maintenanceRequest struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Message string    `json:"message,omitempty"`
}

// SetMaintenance — POST /v1/admin/policy/maintenance.
// start/end 가 모두 zero 면 정비 창 비활성화.
func SetMaintenance(deps *PolicyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Engine == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_engine", "정책 엔진 없음")
			return
		}
		var req maintenanceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		usid := principalUsid(r)
		err := deps.Engine.SetMaintenance(policy.MaintenanceWindow{
			Start: req.Start, End: req.End, Message: req.Message,
		}, usid)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		policyAudit(deps, r, "POLICY_MAINTENANCE",
			slog.Time("start", req.Start),
			slog.Time("end", req.End),
		)
		writeJSON(w, http.StatusOK, deps.Engine.State())
	}
}

// blockedSymbolsRequest — POST /v1/admin/policy/blocked-symbols
type listRequest struct {
	Items []string `json:"items"`
}

// SetBlockedSymbols / SetBlockedRoutingKeys 는 전체 리스트 교체 — 단건 add/remove 보다 단순.
func SetBlockedSymbols(deps *PolicyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Engine == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_engine", "")
			return
		}
		var req listRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		st := deps.Engine.State()
		st.BlockedSymbols = req.Items
		deps.Engine.SetState(st, principalUsid(r))
		policyAudit(deps, r, "POLICY_BLOCKED_SYMBOLS",
			slog.Int("count", len(req.Items)),
		)
		writeJSON(w, http.StatusOK, deps.Engine.State())
	}
}

func SetBlockedRoutingKeys(deps *PolicyDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Engine == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_engine", "")
			return
		}
		var req listRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		st := deps.Engine.State()
		st.BlockedRoutingKeys = req.Items
		deps.Engine.SetState(st, principalUsid(r))
		policyAudit(deps, r, "POLICY_BLOCKED_RKEYS",
			slog.Int("count", len(req.Items)),
		)
		writeJSON(w, http.StatusOK, deps.Engine.State())
	}
}

// policyAudit — auditLog 와 동일하지만 PolicyDeps 시그니처.
func policyAudit(deps *PolicyDeps, r *http.Request, action string, attrs ...any) {
	if deps == nil {
		return
	}
	rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
	auditLog(rd, r, action, attrs...)
}

// 사용 안 함 (편집기 silence) — handler 패키지 외부 참조 회피.
var _ = errors.New
