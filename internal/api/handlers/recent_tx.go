package handlers

import (
	"net/http"
	"strconv"
	"strings"
)

// RecentTx — GET /v1/admin/recent-tx. mci-api 의 in-memory TxRing snapshot
// 을 JSON array 로 반환. mci-admin 의 매매 감사 dashboard 가 polling.
//
// query:
//
//	limit   = 0 이면 ring 전체. >0 이면 그 수만큼.
//	usid    = (optional) usid 부분일치 filter
//	alias   = (optional) alias 부분일치 filter
//	errn    = "1" 이면 broker_errn != 0 또는 http_status >= 400 만
//	bulk    = "1" 이면 bulk item 만, "0" 이면 단일 tx 만
//
// 응답:
//
//	{"items":[TxEntry...], "size": N, "cap": C}
func RecentTx(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TxRing == nil {
			writeError(w, http.StatusServiceUnavailable, "no_tx_ring",
				"매매 audit ring 미활성 — --tx-ring N 으로 활성")
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		fUsid := strings.ToLower(strings.TrimSpace(q.Get("usid")))
		fAlias := strings.ToLower(strings.TrimSpace(q.Get("alias")))
		onlyErr := q.Get("errn") == "1"
		bulkFilter := q.Get("bulk") // "1" / "0" / ""

		all := deps.TxRing.Snapshot(limit)
		out := all
		if fUsid != "" || fAlias != "" || onlyErr || bulkFilter != "" {
			out = out[:0]
			for _, e := range all {
				if fUsid != "" && !strings.Contains(strings.ToLower(e.Usid), fUsid) {
					continue
				}
				if fAlias != "" && !strings.Contains(strings.ToLower(e.Alias), fAlias) {
					continue
				}
				if onlyErr && e.BrokerErrn == 0 && e.HTTPStatus < 400 {
					continue
				}
				if bulkFilter == "1" && !e.IsBulk {
					continue
				}
				if bulkFilter == "0" && e.IsBulk {
					continue
				}
				out = append(out, e)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": out,
			"size":  deps.TxRing.Size(),
			"cap":   deps.TxRing.Cap(),
		})
	}
}
