package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/ratelimit"
)

// RateLimitDeps — rate limit 정책 CRUD 핸들러 공통 의존성.
//
// 패턴:
//   - 운영자가 admin UI 또는 REST 로 PolicyDoc PUT
//   - mci-edge-* 의 ratelimit.EtcdWatcher 가 즉시 hot-swap
//   - 모든 edge 인스턴스에 전파 (재배포 X)
//
// Prefix 끝의 "/" 자동 보정. service 별 key 는 "<Prefix><service>".
type RateLimitDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/ratelimit/"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

func (d *RateLimitDeps) prefix() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/ratelimit/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *RateLimitDeps) keyFor(service string) string {
	return d.prefix() + service
}

// ListRateLimitPolicies — GET /v1/admin/ratelimit. 모든 service 의 PolicyDoc
// 반환 (service 별 한 doc).
func ListRateLimitPolicies(deps *RateLimitDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.prefix(), clientv3.WithPrefix())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		prefix := deps.prefix()
		out := make([]map[string]any, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			service := strings.TrimPrefix(string(kv.Key), prefix)
			var doc ratelimit.PolicyDoc
			if err := json.Unmarshal(kv.Value, &doc); err != nil {
				deps.Logger.Warn("PolicyDoc 파싱 실패 (skip)",
					slog.String("key", string(kv.Key)),
					slog.Any("error", err),
				)
				continue
			}
			out = append(out, map[string]any{
				"service": service,
				"doc":     doc,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"policies": out,
			"count":    len(out),
		})
	}
}

// GetRateLimitPolicy — GET /v1/admin/ratelimit/{service}.
func GetRateLimitPolicy(deps *RateLimitDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		service := strings.TrimSpace(r.PathValue("service"))
		if service == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "service 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(service))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "정책 미등록")
			return
		}
		var doc ratelimit.PolicyDoc
		if err := json.Unmarshal(resp.Kvs[0].Value, &doc); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, doc)
	}
}

// PutRateLimitPolicy — PUT /v1/admin/ratelimit/{service}. 본문은 PolicyDoc JSON.
//
// 검증:
//   - 각 룰의 Pattern 이 compile 가능해야 (잘못된 glob 거부)
//   - Rate / Burst 음수 거부
//   - fallback rate/burst 도 동일 검증
func PutRateLimitPolicy(deps *RateLimitDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		service := strings.TrimSpace(r.PathValue("service"))
		if service == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "service 필요")
			return
		}
		var doc ratelimit.PolicyDoc
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if err := validatePolicyDoc(doc); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		// version 자동 증가 — 기존 doc 의 version 보다 +1.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if doc.Version == 0 {
			if prev, err := deps.Cli.Get(ctx, deps.keyFor(service)); err == nil && len(prev.Kvs) > 0 {
				var pdoc ratelimit.PolicyDoc
				if json.Unmarshal(prev.Kvs[0].Value, &pdoc) == nil {
					doc.Version = pdoc.Version + 1
				}
			}
			if doc.Version == 0 {
				doc.Version = 1
			}
		}
		body, err := json.Marshal(doc)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal", err.Error())
			return
		}
		if _, err := deps.Cli.Put(ctx, deps.keyFor(service), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "ratelimit", "PUT_RATELIMIT",
			slog.String("service", service),
			slog.Int64("version", doc.Version),
			slog.Int("rules", len(doc.Rules)),
			slog.Bool("fallback", doc.Fallback != nil),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("ratelimit", map[string]any{
				"action":  "put",
				"service": service,
				"version": doc.Version,
			})
		}
		writeJSON(w, http.StatusOK, doc)
	}
}

// DeleteRateLimitPolicy — DELETE /v1/admin/ratelimit/{service}.
//
// edge 측은 EtcdWatcher 의 DELETE 핸들러에서 defaults 로 fallback.
func DeleteRateLimitPolicy(deps *RateLimitDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		service := strings.TrimSpace(r.PathValue("service"))
		if service == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "service 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(service))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if resp.Deleted == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "정책 미존재")
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "ratelimit", "DELETE_RATELIMIT",
			slog.String("service", service))
		if deps.Hub != nil {
			deps.Hub.Broadcast("ratelimit", map[string]any{
				"action":  "delete",
				"service": service,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// validatePolicyDoc — PolicyDoc 의 정책 검증. 잘못된 룰은 EtcdWatcher 도 동일
// 검증으로 잡지만, 운영자에게 의미 있는 에러 메시지를 위해 PUT 진입 시 한 번 더.
//
// NewRuleSet 은 부수효과 (limiter goroutine) 가 있으므로 검증 후 즉시 Stop.
func validatePolicyDoc(doc ratelimit.PolicyDoc) error {
	if doc.Fallback != nil && (doc.Fallback.Rate < 0 || doc.Fallback.Burst < 0) {
		return fmt.Errorf("fallback rate/burst 음수")
	}
	rs, err := ratelimit.NewRuleSet(doc.Rules, doc.Fallback.ToConfig())
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	rs.Stop()
	return nil
}
