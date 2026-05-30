package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/quoteid"
)

// QuoteIDEnginesDeps — QuoteID engine allowlist CRUD 핸들러 의존성.
//
// 변경은 etcd KV 직접 PUT/DELETE — mci-price 의 quoteid.EtcdAllowlistWatcher 가
// 동일 prefix watch 로 hot reload. 다중 mci-price 인스턴스에 즉시 전파.
type QuoteIDEnginesDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/quoteid/engines/"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

func (d *QuoteIDEnginesDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/quoteid/engines/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *QuoteIDEnginesDeps) keyFor(engineID string) string {
	return d.normalize() + engineID
}

// engineEntry — 응답 JSON 모양 (EngineMeta + engine_id path).
//
// 빈 etcd value (= 풀 권한, 무기한) 는 모두 default 필드로 표현.
type engineEntry struct {
	EngineID    string   `json:"engine_id"`
	Permissions []string `json:"permissions,omitempty"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	Contact     string   `json:"contact,omitempty"`
}

// 허용된 permission 토큰 — quoteid 패키지 상수의 thin re-export.
var allowedPermissions = map[string]struct{}{
	quoteid.PermValidate:     {},
	quoteid.PermMarkConsumed: {},
}

// validateMeta — request body 의 EngineMeta 가 정합한지 검사.
// 모르는 permission token / 잘못된 ExpiresAt RFC3339 거부.
func validateMeta(m quoteid.EngineMeta) (string, bool) {
	for _, p := range m.Permissions {
		if _, ok := allowedPermissions[p]; !ok {
			return "unknown permission: " + p, false
		}
	}
	if m.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, m.ExpiresAt); err != nil {
			return "expires_at must be RFC3339 (예: 2026-12-31T00:00:00Z)", false
		}
	}
	return "", true
}

func metaToEntry(engineID string, raw []byte) engineEntry {
	out := engineEntry{EngineID: engineID}
	if len(raw) == 0 {
		return out
	}
	if raw[0] != '{' {
		return out // 비-JSON value 는 v1.12 backward compat — default meta.
	}
	var m quoteid.EngineMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	out.Permissions = m.Permissions
	out.ExpiresAt = m.ExpiresAt
	out.Contact = m.Contact
	return out
}

// ListQuoteIDEngines — GET /v1/admin/quoteid-engines.
func ListQuoteIDEngines(deps *QuoteIDEnginesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.normalize(), clientv3.WithPrefix())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		out := make([]engineEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			engineID := strings.TrimPrefix(string(kv.Key), deps.normalize())
			if engineID == "" {
				continue
			}
			out = append(out, metaToEntry(engineID, kv.Value))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"engines": out,
			"count":   len(out),
		})
	}
}

// GetQuoteIDEngine — GET /v1/admin/quoteid-engines/{engine_id}.
func GetQuoteIDEngine(deps *QuoteIDEnginesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		engineID := strings.TrimSpace(r.PathValue("engine_id"))
		if engineID == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "engine_id 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(engineID))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "engine 미등록")
			return
		}
		writeJSON(w, http.StatusOK, metaToEntry(engineID, resp.Kvs[0].Value))
	}
}

// PutQuoteIDEngine — PUT /v1/admin/quoteid-engines/{engine_id}. body 는 EngineMeta.
//
// 빈 body 도 허용 (풀 권한 / 무기한 — v1.12 동작). body 에 잘못된
// permission token / 잘못된 ExpiresAt RFC3339 는 400.
func PutQuoteIDEngine(deps *QuoteIDEnginesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		engineID := strings.TrimSpace(r.PathValue("engine_id"))
		if engineID == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "engine_id 필요")
			return
		}
		// 빈 body 면 EngineMeta 그대로 0값.
		var m quoteid.EngineMeta
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
				return
			}
		}
		if reason, ok := validateMeta(m); !ok {
			writeJSONError(w, http.StatusBadRequest, "validation", reason)
			return
		}
		// 직렬화 — 빈 meta 면 빈 문자열 value (backward compat).
		var value string
		if len(m.Permissions) > 0 || m.ExpiresAt != "" || m.Contact != "" {
			body, err := json.Marshal(m)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "marshal", err.Error())
				return
			}
			value = string(body)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.keyFor(engineID), value); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "quoteid_engine", "PUT_QUOTEID_ENGINE",
			slog.String("engine_id", engineID),
			slog.Any("permissions", m.Permissions),
			slog.String("expires_at", m.ExpiresAt),
			slog.String("contact", m.Contact),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("quoteid_engine", map[string]any{
				"action": "put", "engine_id": engineID,
				"permissions": m.Permissions, "expires_at": m.ExpiresAt, "contact": m.Contact,
			})
		}
		entry := engineEntry{
			EngineID:    engineID,
			Permissions: m.Permissions,
			ExpiresAt:   m.ExpiresAt,
			Contact:     m.Contact,
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

// DeleteQuoteIDEngine — DELETE /v1/admin/quoteid-engines/{engine_id}.
func DeleteQuoteIDEngine(deps *QuoteIDEnginesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		engineID := strings.TrimSpace(r.PathValue("engine_id"))
		if engineID == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "engine_id 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(engineID))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if resp.Deleted == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "engine 미존재")
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "quoteid_engine", "DELETE_QUOTEID_ENGINE", slog.String("engine_id", engineID))
		if deps.Hub != nil {
			deps.Hub.Broadcast("quoteid_engine", map[string]any{
				"action": "delete", "engine_id": engineID,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

