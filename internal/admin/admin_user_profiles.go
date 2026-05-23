package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/session"
)

// UserProfilesDeps — 사용자 프로파일 CRUD 핸들러 의존성.
//
// 변경은 etcd KV 직접 PUT/DELETE — mci-api 의 EtcdUserProfileResolver 가 watch 로
// 받아 hot reload. 다중 mci-api 인스턴스에 즉시 전파.
type UserProfilesDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/auth/user-profiles/"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

func (d *UserProfilesDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/auth/user-profiles/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *UserProfilesDeps) keyFor(usid string) string {
	return d.normalize() + usid
}

// userProfileEntry — 응답 JSON 모양 (auth.UserProfile + usid path 값 포함).
type userProfileEntry struct {
	Usid string       `json:"usid"`
	Site session.Site `json:"site"`
	Tier session.Tier `json:"tier"`
}

// ListUserProfiles — GET /v1/admin/user-profiles.
func ListUserProfiles(deps *UserProfilesDeps) http.HandlerFunc {
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
		out := make([]userProfileEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			var p auth.UserProfile
			if err := json.Unmarshal(kv.Value, &p); err != nil {
				deps.Logger.Warn("UserProfile 파싱 실패 (skip)",
					slog.String("key", string(kv.Key)),
					slog.Any("error", err),
				)
				continue
			}
			usid := strings.TrimPrefix(string(kv.Key), deps.normalize())
			out = append(out, userProfileEntry{Usid: usid, Site: p.Site, Tier: p.Tier})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles": out,
			"count":    len(out),
		})
	}
}

// GetUserProfile — GET /v1/admin/user-profiles/{usid}.
func GetUserProfile(deps *UserProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		usid := strings.TrimSpace(r.PathValue("usid"))
		if usid == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "usid 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(usid))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "user profile 미등록")
			return
		}
		var p auth.UserProfile
		if err := json.Unmarshal(resp.Kvs[0].Value, &p); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, userProfileEntry{Usid: usid, Site: p.Site, Tier: p.Tier})
	}
}

// PutUserProfile — PUT /v1/admin/user-profiles/{usid}. body 는 {site, tier}.
//
// site/tier 모두 채워져야 한다 — 빈 값으로 등록할 거면 Delete 가 의미적으로 명확.
func PutUserProfile(deps *UserProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		usid := strings.TrimSpace(r.PathValue("usid"))
		if usid == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "usid 필요")
			return
		}
		var p auth.UserProfile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if p.Site == "" || p.Tier == "" {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"site/tier 모두 필수 (제거하려면 DELETE 사용)")
			return
		}
		body, err := json.Marshal(p)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.keyFor(usid), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		userProfileAudit(deps, r, "PUT_USER_PROFILE",
			slog.String("usid", usid),
			slog.String("site", string(p.Site)),
			slog.String("tier", string(p.Tier)),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("user_profile", map[string]any{
				"action": "put", "usid": usid, "site": p.Site, "tier": p.Tier,
			})
		}
		writeJSON(w, http.StatusOK, userProfileEntry{Usid: usid, Site: p.Site, Tier: p.Tier})
	}
}

// DeleteUserProfile — DELETE /v1/admin/user-profiles/{usid}.
func DeleteUserProfile(deps *UserProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		usid := strings.TrimSpace(r.PathValue("usid"))
		if usid == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "usid 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(usid))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if resp.Deleted == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "user profile 미존재")
			return
		}
		userProfileAudit(deps, r, "DELETE_USER_PROFILE", slog.String("usid", usid))
		if deps.Hub != nil {
			deps.Hub.Broadcast("user_profile", map[string]any{"action": "delete", "usid": usid})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func userProfileAudit(deps *UserProfilesDeps, r *http.Request, action string, attrs ...any) {
	if deps == nil {
		return
	}
	rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
	auditLog(rd, r, action, attrs...)
}
