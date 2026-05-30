package price

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/winwaysystems/wtg/pkg/netutil"
)

// guardAdmin — 모든 /v1/admin/* 핸들러 앞에 적용되는 IP allowlist 가드.
// cfg.AdminAllowCIDRs 가 비어있으면 모든 admin 요청 403 (default secure).
// 일반 ws AllowCIDRs 와 별개로 좁은 운영망 (예: 10.0.0.0/8) 만 허용.
//
// 노트: 일반 chain 의 IPAllowList 미들웨어가 이미 적용된 후라 reset 함수
// 가 아니라 이중 가드. 일반 allowlist 가 admin allowlist 의 superset 이어야
// 정상 운영.
func (s *Server) guardAdmin(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := netutil.RemoteIP(r)
		if !netutil.IPAllowed(ip, s.cfg.AdminAllowCIDRs) {
			s.logger.Warn("admin endpoint 거부",
				slog.String("ip", ip.String()),
				slog.String("path", r.URL.Path),
			)
			writeJSONError(w, http.StatusForbidden, "admin_forbidden",
				"admin endpoint 접근 불가 — admin-allow-cidrs 확인")
			return
		}
		inner(w, r)
	}
}

// adminPairRequest — disallow-pair / allow-pair 의 공통 본문 모양.
type adminPairRequest struct {
	Pair string `json:"pair"`
}

// adminDisallowPair — POST /v1/admin/disallow-pair {"pair":"..."}
//
// 운영자가 특정 pair 의 시세 발행을 즉시 정지시킬 때 사용. 효과:
//  1) validator 에서 pair 제거 → 신규 subscribe 차단
//  2) Registry 의 모든 기존 sub 의 그 pair 필터 제거 + force unsubscribe
//  3) 영향 받은 sub 들에 알림 발송 ({"type":"revoked","pair":"..."})
//
// 이후 quote 가 도착해도 (PricingTable 에서 발행 멈춤 전까지) edge 측에서는
// 모든 sub 가 그 pair 매칭 안 함 → 자연스럽게 정지.
func (s *Server) adminDisallowPair() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req adminPairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Pair == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "pair 필수")
			return
		}

		// 1) validator 에서 제거 (Phase 2 가드 가동 중이면).
		if mv, ok := s.pairValidator.(*MemoryPairValidator); ok && mv != nil {
			mv.Remove(req.Pair)
		}
		// 2) 기존 sub 들의 filter 정리.
		affected := s.registry.RevokePairFromAll(req.Pair)
		// 3) 영향 받은 sub 들에 알림 — pair 매칭 (=그 pair 받던) 모든 sub.
		notice, _ := json.Marshal(map[string]any{
			"type":   "revoked",
			"pair":   req.Pair,
			"reason": "admin disallow-pair",
		})
		s.registry.BroadcastForPair(req.Pair, notice)

		s.logger.Warn("admin disallow-pair",
			slog.String("pair", req.Pair),
			slog.Int("affected_subscribers", affected),
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"pair":     req.Pair,
			"affected": affected,
		})
	}
}

// adminAllowPair — POST /v1/admin/allow-pair {"pair":"..."}
//
// Phase 2 권한 가드가 활성일 때 새 pair 를 허용 set 에 추가. operator 가 신규
// 통화쌍을 즉시 시세 카탈로그에 편입할 때 사용. validator 가 nil 이면 (가드
// 비활성) no-op 으로 200 OK.
func (s *Server) adminAllowPair() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req adminPairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.Pair == "" {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "pair 필수")
			return
		}
		if mv, ok := s.pairValidator.(*MemoryPairValidator); ok && mv != nil {
			mv.Add(req.Pair)
		}
		s.logger.Info("admin allow-pair", slog.String("pair", req.Pair))
		writeJSON(w, http.StatusOK, map[string]any{"pair": req.Pair})
	}
}
