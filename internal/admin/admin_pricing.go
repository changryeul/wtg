package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/pricing"
)

// PricingDeps 는 PricingTable CRUD 핸들러 의존성.
//
// 단일 key 패턴 — etcd 의 한 key 에 PricingTableDoc JSON 전체가 들어간다.
// mci-price 의 pricing.EtcdTableWatcher 가 변경 즉시 store.Replace.
type PricingDeps struct {
	Cli    *clientv3.Client
	Key    string // default "wtg/pricing/table"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

func (d *PricingDeps) key() string {
	if d.Key == "" {
		return "wtg/pricing/table"
	}
	return d.Key
}

// GetPricingTable 은 GET /v1/admin/pricing/table.
// 빈 응답 (404) 은 운영자가 아직 publish 하지 않은 상태.
func GetPricingTable(deps *PricingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.key())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "pricing table 미등록")
			return
		}
		// JSON 그대로 응답 (raw passthrough) — UI 가 그대로 편집 가능.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(resp.Kvs[0].Value)
	}
}

// PutPricingTable 는 PUT /v1/admin/pricing/table — 본문은 PricingTableDoc JSON.
//
// 서버 측 검증: ParsePricingTable 로 schema 무결성만 확인 (마진 값 정상범위 등은
// 운영자 책임 — 별도 정책 모듈에서 검증).
func PutPricingTable(deps *PricingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB safety cap
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read", err.Error())
			return
		}
		body = []byte(strings.TrimSpace(string(body)))
		if len(body) == 0 {
			writeJSONError(w, http.StatusBadRequest, "validation", "empty body")
			return
		}
		// 검증 — schema 파싱.
		tbl, err := pricing.ParsePricingTable(body)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.key(), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		pricingAudit(deps, r, "PUT_PRICING_TABLE",
			slog.Int64("version", tbl.Version),
			slog.Int("hq_count", len(tbl.HQMargin)),
			slog.Int("site_count", len(tbl.SiteMargin)),
			slog.Int("swap_count", len(tbl.SwapPoint)),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("pricing", map[string]any{
				"action":  "put",
				"version": tbl.Version,
			})
		}
		// 클라이언트에 정규화된 JSON 회신 (운영자 검증 용이).
		writeJSON(w, http.StatusOK, json.RawMessage(body))
	}
}

func pricingAudit(deps *PricingDeps, r *http.Request, action string, attrs ...any) {
	if deps == nil {
		return
	}
	rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
	auditLog(rd, r, action, attrs...)
}
