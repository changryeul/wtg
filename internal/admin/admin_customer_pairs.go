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

// admin_customer_pairs.go — 고객별 ws 구독 허용 pair allowlist 의 etcd CRUD.
//
// 운영자가 customer ID 를 임의 부여 + 그 customer 가 구독 가능한 pair 만
// 명시 → mci-edge-price 가 etcd watch 로 즉시 반영.
//
// etcd schema:
//
//	<prefix><customerID> = JSON []string  (예: ["USD/KRW","EUR/USD"])
//
// 결합 정책 (mci-edge-price):
//   - customer 미등록 → 글로벌 정책만 (unrestricted)
//   - customer 등록   → 글로벌 ∩ customer 허용 set
//   - 글로벌 disallow → 항상 우선 (emergency cut)
//   - 빈 list 등록    → "전체 차단" 의도

// CustomerPairsDeps 는 고객별 구독 허용 pair CRUD 핸들러 의존성.
//
// Prefix 의 default 는 "wtg/customers/" — mci-edge-price 의
// EtcdCustomerPairsPrefix 와 일치해야 한다.
type CustomerPairsDeps struct {
	Cli    *clientv3.Client
	Prefix string // default "wtg/customers/"
	Logger *slog.Logger
	Audit  *AuditRing
}

func (d *CustomerPairsDeps) normalize() string {
	p := d.Prefix
	if p == "" {
		p = "wtg/customers/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (d *CustomerPairsDeps) keyFor(customerID string) string {
	return d.normalize() + customerID
}

// customerPairsEntry — admin endpoint 의 JSON 응답 모양.
type customerPairsEntry struct {
	CustomerID string   `json:"customer_id"`
	Pairs      []string `json:"pairs"`
}

// putRequest — PUT 본문.
type customerPairsPutRequest struct {
	Pairs []string `json:"pairs"`
}

// normalizePairs — trim + dedup + sort. 빈 list 은 "전체 차단" 의도라 유지.
func normalizePairs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// validateCustomerID — 운영자 부여 ID. ASCII, slash 금지 (etcd 키 분리자), 64 chars max.
func validateCustomerID(cid string) error {
	cid = strings.TrimSpace(cid)
	if cid == "" {
		return errors.New("customer_id 필요")
	}
	if len(cid) > 64 {
		return errors.New("customer_id 64자 초과")
	}
	for _, c := range cid {
		if c < 0x20 || c == 0x7f || c == '/' || c == ' ' {
			return errors.New("customer_id 에 공백 / slash / 제어문자 금지")
		}
	}
	return nil
}

// ListCustomerPairs — GET /v1/admin/customer-pairs
func ListCustomerPairs(deps *CustomerPairsDeps) http.HandlerFunc {
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
		out := make([]customerPairsEntry, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			cid := strings.TrimPrefix(string(kv.Key), prefix)
			if cid == "" {
				continue
			}
			var pairs []string
			if err := json.Unmarshal(kv.Value, &pairs); err != nil {
				deps.Logger.Warn("customer-pairs 파싱 실패 (skip)",
					slog.String("customer", cid), slog.Any("error", err))
				continue
			}
			sort.Strings(pairs)
			out = append(out, customerPairsEntry{CustomerID: cid, Pairs: pairs})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
		writeJSON(w, http.StatusOK, map[string]any{
			"customers": out,
			"count":     len(out),
		})
	}
}

// GetCustomerPairs — GET /v1/admin/customer-pairs/{customer_id}
func GetCustomerPairs(deps *CustomerPairsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("customer_id"))
		if err := validateCustomerID(cid); err != nil {
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
			writeJSONError(w, http.StatusNotFound, "not_found", "customer 미등록")
			return
		}
		var pairs []string
		if err := json.Unmarshal(resp.Kvs[0].Value, &pairs); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "decode", err.Error())
			return
		}
		sort.Strings(pairs)
		writeJSON(w, http.StatusOK, customerPairsEntry{CustomerID: cid, Pairs: pairs})
	}
}

// PutCustomerPairs — PUT /v1/admin/customer-pairs/{customer_id} {"pairs":[...]}
//
// 빈 list (`{"pairs":[]}`) 도 합법 — "전체 차단" 의도. etcd 에 그대로 저장.
func PutCustomerPairs(deps *CustomerPairsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("customer_id"))
		if err := validateCustomerID(cid); err != nil {
			writeJSONError(w, http.StatusBadRequest, "validation", err.Error())
			return
		}
		var req customerPairsPutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "decode", err.Error())
			return
		}
		if req.Pairs == nil {
			req.Pairs = []string{}
		}
		normalized := normalizePairs(req.Pairs)
		body, err := json.Marshal(normalized)
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
			deps.Audit.Push(AuditEntry{
				At:       time.Now(),
				Action:   "PUT_CUSTOMER_PAIRS",
				Resource: "customer_pairs",
				Usid:     principalUsid(r),
				Attrs: map[string]any{
					"customer_id": cid,
					"pairs":       normalized,
				},
			})
		}
		deps.Logger.Info("customer-pairs PUT",
			slog.String("customer", cid),
			slog.Int("pairs", len(normalized)))
		writeJSON(w, http.StatusOK, customerPairsEntry{CustomerID: cid, Pairs: normalized})
	}
}

// DeleteCustomerPairs — DELETE /v1/admin/customer-pairs/{customer_id}
// 삭제 = customer 가 unrestricted (글로벌 정책만 적용) 으로 복귀.
func DeleteCustomerPairs(deps *CustomerPairsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Cli == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_etcd", "etcd 미구성")
			return
		}
		cid := strings.TrimSpace(r.PathValue("customer_id"))
		if err := validateCustomerID(cid); err != nil {
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
				Action:   "DELETE_CUSTOMER_PAIRS",
				Resource: "customer_pairs",
				Usid:     principalUsid(r),
				Attrs: map[string]any{
					"customer_id": cid,
				},
			})
		}
		deps.Logger.Info("customer-pairs DELETE",
			slog.String("customer", cid),
			slog.Int64("deleted", resp.Deleted))
		writeJSON(w, http.StatusOK, map[string]any{
			"customer_id": cid,
			"deleted":     resp.Deleted,
		})
	}
}
