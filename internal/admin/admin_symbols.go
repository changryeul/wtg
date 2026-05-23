package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/quote"
)

// SymbolsDeps 는 SymbolMap CRUD 핸들러 공통 의존성.
//
// etcd watch 패턴:
//
//   - 본 mci-admin 핸들러는 etcd 에 직접 PUT/DELETE 만 수행.
//   - mci-price 의 quote.EtcdSymbolWatcher 가 변경을 받아 SymbolMap.Replace.
//   - 모든 mci-price 인스턴스에 즉시 전파됨 (재배포 X).
type SymbolsDeps struct {
	Cli    *clientv3.Client
	Prefix string // 끝에 "/" 자동 보정. default "wtg/quote/symbols/"
	Logger *slog.Logger
	Audit  *AuditRing
	Hub    *Hub
}

// normalize 는 prefix 끝의 "/" 와 default 보정.
func (d *SymbolsDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/quote/symbols/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// keyFor 는 단일 symbol 의 etcd key.
func (d *SymbolsDeps) keyFor(symbol string) string {
	return d.normalize() + symbol
}

// ListSymbols 는 GET /v1/admin/symbols — 모든 entry 반환.
func ListSymbols(deps *SymbolsDeps) http.HandlerFunc {
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
		entries := make([]quote.SymbolEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			var e quote.SymbolEntry
			if err := json.Unmarshal(kv.Value, &e); err != nil {
				deps.Logger.Warn("symbol entry 파싱 실패 (skip)",
					slog.String("key", string(kv.Key)),
					slog.Any("error", err),
				)
				continue
			}
			entries = append(entries, e)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"symbols": entries,
			"count":   len(entries),
		})
	}
}

// GetSymbol 은 GET /v1/admin/symbols/{symbol}.
func GetSymbol(deps *SymbolsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		sym := strings.TrimSpace(r.PathValue("symbol"))
		if sym == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "symbol 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Get(ctx, deps.keyFor(sym))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if len(resp.Kvs) == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "symbol 미등록")
			return
		}
		var e quote.SymbolEntry
		if err := json.Unmarshal(resp.Kvs[0].Value, &e); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, e)
	}
}

// PutSymbol 은 PUT /v1/admin/symbols/{symbol} — 생성 또는 수정.
//
// 본문은 quote.SymbolEntry JSON. Symbol 필드는 path 와 일치해야 함 (불일치 시 path 우선).
func PutSymbol(deps *SymbolsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		sym := strings.TrimSpace(r.PathValue("symbol"))
		if sym == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "symbol 필요")
			return
		}
		var e quote.SymbolEntry
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		// path 우선 — 본문의 Symbol 이 비어있거나 다르면 path 로 통일.
		e.Symbol = sym
		if e.Pair == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "pair 필수")
			return
		}
		body, err := json.Marshal(e)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "marshal", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := deps.Cli.Put(ctx, deps.keyFor(sym), string(body)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		symbolAudit(deps, r, "PUT_SYMBOL",
			slog.String("symbol", sym),
			slog.String("pair", string(e.Pair)),
			slog.Bool("active", e.Active),
		)
		if deps.Hub != nil {
			deps.Hub.Broadcast("symbol", map[string]any{"action": "put", "entry": e})
		}
		writeJSON(w, http.StatusOK, e)
	}
}

// DeleteSymbol 은 DELETE /v1/admin/symbols/{symbol}.
func DeleteSymbol(deps *SymbolsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		sym := strings.TrimSpace(r.PathValue("symbol"))
		if sym == "" {
			writeJSONError(w, http.StatusBadRequest, "validation", "symbol 필요")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		resp, err := deps.Cli.Delete(ctx, deps.keyFor(sym))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "etcd", err.Error())
			return
		}
		if resp.Deleted == 0 {
			writeJSONError(w, http.StatusNotFound, "not_found", "symbol 미존재")
			return
		}
		symbolAudit(deps, r, "DELETE_SYMBOL", slog.String("symbol", sym))
		if deps.Hub != nil {
			deps.Hub.Broadcast("symbol", map[string]any{"action": "delete", "symbol": sym})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// symbolAudit — RoutingDeps.auditLog 의 SymbolsDeps 버전.
func symbolAudit(deps *SymbolsDeps, r *http.Request, action string, attrs ...any) {
	if deps == nil {
		return
	}
	// 동일한 audit 페이로드 형태를 따른다.
	rd := &RoutingDeps{Logger: deps.Logger, Audit: deps.Audit}
	auditLog(rd, r, action, attrs...)
}

// 컴파일 타임 보장 — 핸들러 빈값 deps 호출 안전성.
var _ = errors.New
