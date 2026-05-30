package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/session"
)

// ProfilesDeps 는 활성 Profile 카탈로그 CRUD 핸들러 의존성.
//
// etcd watch 패턴 — mci-price 의 EtcdProfileSource 가 변경 즉시 ActiveProfiles snapshot 갱신.
type ProfilesDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/price/profiles/"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

func (d *ProfilesDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/price/profiles/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *ProfilesDeps) keyFor(key string) string {
	return d.normalize() + key
}

// profileEntry — admin endpoint 의 JSON 응답 모양 (lowercase 키, UI 기대 형식).
// session.Profile 자체에 json tag 를 추가하면 다른 wire format (Redis 세션 /
// pricing config / quoteid Record / gRPC) 도 함께 변경되므로 admin response
// 만 별도 wrapper 로 분리.
type profileEntry struct {
	Channel session.Channel `json:"channel"`
	Site    session.Site    `json:"site"`
	Tier    session.Tier    `json:"tier"`
	Key     string          `json:"key"`
}

func toProfileEntry(p session.Profile) profileEntry {
	return profileEntry{
		Channel: p.Channel,
		Site:    p.Site,
		Tier:    p.Tier,
		Key:     p.Key(),
	}
}

// ListProfiles 는 GET /v1/admin/profiles — 전체 활성 Profile 반환.
func ListProfiles(deps *ProfilesDeps) http.HandlerFunc {
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
		out := make([]profileEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			var p session.Profile
			if err := json.Unmarshal(kv.Value, &p); err != nil {
				deps.Logger.Warn("profile 파싱 실패 (skip)",
					slog.String("key", string(kv.Key)),
					slog.Any("error", err),
				)
				continue
			}
			out = append(out, toProfileEntry(p))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles": out,
			"count":    len(out),
		})
	}
}

// GetProfile 은 GET /v1/admin/profiles/{key} (예: "WEB.BRANCH.VIP").
func GetProfile(deps *ProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		key := strings.TrimSpace(r.PathValue("key"))
		if key == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "key 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(key))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "profile 미등록")
			return
		}
		var p session.Profile
		if err := json.Unmarshal(resp.Kvs[0].Value, &p); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, toProfileEntry(p))
	}
}

// PutProfile 은 PUT /v1/admin/profiles/{key}.
//
// path 의 key 는 본문에서 도출한 Profile.Key() 와 일치해야 한다 (불일치 = 400).
// body 의 channel/site/tier 가 모두 채워졌는지 검증.
func PutProfile(deps *ProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		key := strings.TrimSpace(r.PathValue("key"))
		if key == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "key 필요")
			return
		}
		var p session.Profile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if p.Channel == "" || p.Site == "" || p.Tier == "" {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"channel/site/tier 모두 필수")
			return
		}
		derived := p.Key()
		if derived != key {
			writeJSONError(w, http.StatusBadRequest, "validation",
				"path key 와 body 도출 key 불일치: path="+key+", body="+derived)
			return
		}
		body, err := json.Marshal(p)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.keyFor(key), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "profile", "PUT_PROFILE",
			slog.String("key", key),
			slog.String("channel", string(p.Channel)),
			slog.String("site", string(p.Site)),
			slog.String("tier", string(p.Tier)),
		)
		entry := toProfileEntry(p)
		if deps.Hub != nil {
			deps.Hub.Broadcast("profile", map[string]any{"action": "put", "profile": entry})
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

// DeleteProfile 은 DELETE /v1/admin/profiles/{key}.
func DeleteProfile(deps *ProfilesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		key := strings.TrimSpace(r.PathValue("key"))
		if key == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "key 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(key))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if resp.Deleted == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "profile 미존재")
			return
		}
		emitAudit(deps.Logger, deps.Audit, r, "profile", "DELETE_PROFILE", slog.String("key", key))
		if deps.Hub != nil {
			deps.Hub.Broadcast("profile", map[string]any{"action": "delete", "key": key})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

