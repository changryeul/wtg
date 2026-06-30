package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// admin_fix_counterparties.go — FIX 카운터파티 등록의 etcd CRUD.
//
// 운영자가 SenderCompID 임의 부여 + Password + Profile (Channel/Site/Tier) +
// Usid 를 명시 → mci-edge-fix 가 etcd watch 로 즉시 반영.
//
// etcd schema:
//
//	<prefix><SenderCompID> = JSON {password, channel, site, tier, usid}
//
// mci-edge-fix 의 fixApp 가 Logon 시 prefix lookup → password 검증 + Principal
// 주입. 새 SenderCompID 추가는 mci-edge-fix 재시작 필요 (quickfix 의 settings
// 가 startup 시 fix) — Phase C 작업.

// FixCounterpartiesDeps — 핸들러 의존성.
type FixCounterpartiesDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/fix/counterparties/"
	Logger *slog.Logger
	Audit  *AuditRing
}

func (d *FixCounterpartiesDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/fix/counterparties/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *FixCounterpartiesDeps) keyFor(cid string) string {
	return d.normalize() + cid
}

// fixCpEntry — 응답 모양.
type fixCpEntry struct {
	SenderCompID string `json:"sender_comp_id"`
	Password     string `json:"password,omitempty"` // 운영에서 노출 주의 — 가능하면 마스킹
	Channel      string `json:"channel"`
	Site         string `json:"site"`
	Tier         string `json:"tier"`
	Usid         string `json:"usid"`
	// OrderAlias — Phase B Layer 2. 카운터파티별 매매 alias. 빈값이면 mci-edge-fix
	// 가 default "FIX_NEW_ORDER" 사용 (Phase A 호환).
	OrderAlias string `json:"order_alias,omitempty"`
}

type fixCpPutRequest struct {
	Password   string `json:"password"`
	Channel    string `json:"channel"`
	Site       string `json:"site"`
	Tier       string `json:"tier"`
	Usid       string `json:"usid"`
	OrderAlias string `json:"order_alias"`
}

// validateSenderCompID — FIX SenderCompID. ASCII, 공백/슬래시/제어문자 금지,
// 64 chars max.
func validateSenderCompID(cid string) error {
	cid = strings.TrimSpace(cid)
	if cid == "" {
		return errors.New("sender_comp_id 필요")
	}
	if len(cid) > 64 {
		return errors.New("sender_comp_id 64자 초과")
	}
	for _, c := range cid {
		if c < 0x20 || c == 0x7f || c == '/' || c == ' ' {
			return errors.New("sender_comp_id 에 공백/슬래시/제어문자 금지")
		}
	}
	return nil
}

func normalizeFixCp(req fixCpPutRequest) fixCpPutRequest {
	req.Password = strings.TrimSpace(req.Password)
	req.Channel = strings.TrimSpace(req.Channel)
	if req.Channel == "" {
		req.Channel = "FIX"
	}
	req.Site = strings.TrimSpace(req.Site)
	req.Tier = strings.TrimSpace(req.Tier)
	req.Usid = strings.TrimSpace(req.Usid)
	req.OrderAlias = strings.TrimSpace(req.OrderAlias)
	return req
}

// ListFixCounterparties — GET /v1/admin/fix-counterparties
func ListFixCounterparties(deps *FixCounterpartiesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		prefix := deps.normalize()
		resp, err := deps.Cli.Get(ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		out := make([]fixCpEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			cid := strings.TrimPrefix(string(kv.Key), prefix)
			if cid == "" {
				continue
			}
			var cp fixCpPutRequest
			if err := json.Unmarshal(kv.Value, &cp); err != nil {
				deps.Logger.Warn("fix counterparty 파싱 실패 (skip)",
					slog.String("cid", cid), slog.Any("error", err))
				continue
			}
			out = append(out, fixCpEntry{
				SenderCompID: cid,
				Password:     "***", // list 응답엔 마스킹
				Channel:      cp.Channel,
				Site:         cp.Site,
				Tier:         cp.Tier,
				Usid:         cp.Usid,
				OrderAlias:   cp.OrderAlias,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].SenderCompID < out[j].SenderCompID })
		writeJSON(w, http.StatusOK, map[string]any{
			"counterparties": out,
			"count":          len(out),
		})
	}
}

// GetFixCounterparty — GET /v1/admin/fix-counterparties/{cid}
func GetFixCounterparty(deps *FixCounterpartiesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("cid"))
		if err := validateSenderCompID(cid); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(cid))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "counterparty 미등록")
			return
		}
		var cp fixCpPutRequest
		if err := json.Unmarshal(resp.Kvs[0].Value, &cp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		// 단건 조회엔 password 전체 노출 (admin UI 편집용). 운영 audit 의 대상.
		writeJSON(w, http.StatusOK, fixCpEntry{
			SenderCompID: cid,
			Password:     cp.Password,
			Channel:      cp.Channel,
			Site:         cp.Site,
			Tier:         cp.Tier,
			Usid:         cp.Usid,
			OrderAlias:   cp.OrderAlias,
		})
	}
}

// PutFixCounterparty — PUT /v1/admin/fix-counterparties/{cid}
//
// body: { "password":"...", "channel":"FIX", "site":"HQ", "tier":"VIP", "usid":"ECN_X" }
func PutFixCounterparty(deps *FixCounterpartiesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("cid"))
		if err := validateSenderCompID(cid); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		var req fixCpPutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "decode", err.Error())
			return
		}
		req = normalizeFixCp(req)
		body, err := json.Marshal(req)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.keyFor(cid), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if deps.Audit != nil {
			// password 는 audit attrs 에 포함 X — 민감.
			deps.Audit.Push(AuditEntry{
				At:       time.Now(),
				Action:   "PUT_FIX_COUNTERPARTY",
				Resource: "fix_counterparty",
				Usid:     principalUsid(r),
				Attrs: map[string]any{
					"sender_comp_id": cid,
					"channel":        req.Channel,
					"site":           req.Site,
					"tier":           req.Tier,
					"usid":           req.Usid,
				},
			})
		}
		deps.Logger.Info("fix counterparty PUT",
			slog.String("cid", cid),
			slog.String("profile", req.Channel+"."+req.Site+"."+req.Tier))
		writeJSON(w, http.StatusOK, fixCpEntry{
			SenderCompID: cid,
			Password:     "***", // 응답엔 마스킹
			Channel:      req.Channel,
			Site:         req.Site,
			Tier:         req.Tier,
			Usid:         req.Usid,
			OrderAlias:   req.OrderAlias,
		})
	}
}

// DeleteFixCounterparty — DELETE /v1/admin/fix-counterparties/{cid}
func DeleteFixCounterparty(deps *FixCounterpartiesDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("cid"))
		if err := validateSenderCompID(cid); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(cid))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if deps.Audit != nil {
			deps.Audit.Push(AuditEntry{
				At:       time.Now(),
				Action:   "DELETE_FIX_COUNTERPARTY",
				Resource: "fix_counterparty",
				Usid:     principalUsid(r),
				Attrs: map[string]any{
					"sender_comp_id": cid,
				},
			})
		}
		deps.Logger.Info("fix counterparty DELETE",
			slog.String("cid", cid),
			slog.Int64("deleted", resp.Deleted))
		writeJSON(w, http.StatusOK, map[string]any{
			"sender_comp_id": cid,
			"deleted":        resp.Deleted,
		})
	}
}
