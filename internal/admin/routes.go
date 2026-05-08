package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/routing"
)

// RoutingDeps 는 라우팅 CRUD 핸들러 공유 의존성.
type RoutingDeps struct {
	Registry routing.Registry
	Logger   *slog.Logger
	// Audit 가 채워져 있으면 변경 액션을 ring buffer 에 push 한다.
	// nil 이어도 무방 — logger 만 출력.
	Audit *AuditRing
	// Hub 가 채워져 있으면 라우팅 변경을 ws stream 에 publish.
	Hub *Hub
}

// putRouteRequest 는 PUT /v1/admin/routes/{alias} 본문.
//
// alias 는 path 에서 받으므로 본문에는 포함하지 않는다.
type putRouteRequest struct {
	Exchange   string `json:"exchange,omitempty"`
	RoutingKey string `json:"routing_key"`
	Active     bool   `json:"active"`
	Comment    string `json:"comment,omitempty"`
}

// setActiveRequest 는 POST /v1/admin/routes/{alias}/active 본문.
type setActiveRequest struct {
	Active bool `json:"active"`
}

// ListRoutes 는 GET /v1/admin/routes — 모든 룰 정렬 반환.
func ListRoutes(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_registry", "라우팅 저장소 미구성")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rules": deps.Registry.List(),
		})
	}
}

// GetRoute 는 GET /v1/admin/routes/{alias}.
func GetRoute(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_registry", "라우팅 저장소 미구성")
			return
		}
		alias := strings.TrimSpace(r.PathValue("alias"))
		if alias == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "alias 필요")
			return
		}
		rule, err := deps.Registry.Get(alias)
		if err != nil {
			if errors.Is(err, routing.ErrRouteNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "registry", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rule)
	}
}

// PutRoute 는 PUT /v1/admin/routes/{alias} — 생성 또는 수정.
func PutRoute(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_registry", "라우팅 저장소 미구성")
			return
		}
		alias := strings.TrimSpace(r.PathValue("alias"))
		if alias == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "alias 필요")
			return
		}
		var req putRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		rule := &routing.Rule{
			Alias:      alias,
			Exchange:   req.Exchange,
			RoutingKey: req.RoutingKey,
			Active:     req.Active,
			Comment:    req.Comment,
		}
		if err := deps.Registry.Put(rule, principalUsid(r)); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		// 갱신된 룰 (UpdatedAt/By 포함) 을 다시 읽어 반환.
		saved, _ := deps.Registry.Get(alias)
		auditLog(deps, r, "PUT_ROUTE",
			slog.String("alias", alias),
			slog.String("exchange", rule.Exchange),
			slog.String("routing_key", rule.RoutingKey),
			slog.Bool("active", rule.Active),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("route", map[string]any{"action": "put", "rule": saved})
		}
		writeJSON(w, http.StatusOK, saved)
	}
}

// DeleteRoute 는 DELETE /v1/admin/routes/{alias}.
func DeleteRoute(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_registry", "라우팅 저장소 미구성")
			return
		}
		alias := strings.TrimSpace(r.PathValue("alias"))
		if alias == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "alias 필요")
			return
		}
		if err := deps.Registry.Delete(alias); err != nil {
			if errors.Is(err, routing.ErrRouteNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "registry", err.Error())
			return
		}
		auditLog(deps, r, "DELETE_ROUTE", slog.String("alias", alias))
		if deps.Hub != nil {
			deps.Hub.Broadcast("route", map[string]any{"action": "delete", "alias": alias})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// AuditList 는 GET /v1/admin/audit — 최근 admin 액션 시간 역순 반환.
//
// 쿼리 파라미터 limit 로 개수 제한 (default 0 = 전체, 최대 ring capacity).
func AuditList(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Audit == nil {
			writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}})
			return
		}
		limit := 0
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := parsePositiveInt(v); err == nil {
				limit = n
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": deps.Audit.List(limit),
			"count":   deps.Audit.Len(),
		})
	}
}

// parsePositiveInt 는 양의 정수 파싱 (최대 1000 제한).
func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
		if n > 1000 {
			n = 1000
			break
		}
	}
	return n, nil
}

// SetRouteActive 는 POST /v1/admin/routes/{alias}/active — 활성/비활성 토글.
func SetRouteActive(deps *RoutingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_registry", "라우팅 저장소 미구성")
			return
		}
		alias := strings.TrimSpace(r.PathValue("alias"))
		if alias == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "alias 필요")
			return
		}
		var req setActiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if err := deps.Registry.SetActive(alias, req.Active, principalUsid(r)); err != nil {
			if errors.Is(err, routing.ErrRouteNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", err.Error())
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "registry", err.Error())
			return
		}
		saved, _ := deps.Registry.Get(alias)
		auditLog(deps, r, "SET_ROUTE_ACTIVE",
			slog.String("alias", alias),
			slog.Bool("active", req.Active),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("route", map[string]any{"action": "active", "rule": saved})
		}
		writeJSON(w, http.StatusOK, saved)
	}
}

// principalUsid 는 인증된 admin 사용자 ID — UpdatedBy 감사 필드용.
func principalUsid(r *http.Request) string {
	if p := middleware.PrincipalFromContext(r.Context()); p != nil {
		return p.Usid
	}
	return ""
}

// auditLog 는 라우팅 변경 이벤트 — auth.md §10 의 ADMIN_ACTION 카테고리.
//
// 운영에서는 별도 immutable audit sink 로 보내야 한다 (현재는 logger + ring).
// ring 이 nil 이면 logger 만 사용 (외부 의존성 격리).
func auditLog(deps *RoutingDeps, r *http.Request, action string, attrs ...any) {
	if deps == nil {
		return
	}
	usid := principalUsid(r)
	rid := middleware.RequestIDFromContext(r.Context())

	if deps.Logger != nil {
		all := []any{
			slog.String("action", action),
			slog.String("usid", usid),
			slog.String("rid", rid),
		}
		all = append(all, attrs...)
		deps.Logger.InfoContext(r.Context(), "admin audit", all...)
	}
	if deps.Audit != nil {
		// attrs 는 slog.Attr-style key/value 페어 (slog.String("k", v) 등) — 평탄화.
		flat := flattenAttrs(attrs)
		deps.Audit.Push(AuditEntry{
			Action: action,
			Usid:   usid,
			RID:    rid,
			Attrs:  flat,
		})
	}
}

// flattenAttrs 는 slog.Attr 패턴(...slog.String/Bool/Int) 을 map[string]any 로 평탄화.
// 이미 map 형태로 들어오면 그대로 반환.
func flattenAttrs(attrs []any) map[string]any {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		if attr, ok := a.(slog.Attr); ok {
			out[attr.Key] = attr.Value.Any()
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
