package pricing

import (
	"errors"
	"fmt"
	"strings"
)

// ValidationError 는 PricingTableDoc 의 정책 위반 모음.
// 위반이 있어도 메시지로 모두 모아서 한 번에 반환 → 운영자가 한 번에 수정 가능.
type ValidationError struct {
	Violations []string
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return ""
	}
	if len(e.Violations) == 1 {
		return "pricing: validation 실패 — " + e.Violations[0]
	}
	return fmt.Sprintf("pricing: validation 실패 (%d건) — %s",
		len(e.Violations), strings.Join(e.Violations, "; "))
}

// Validate 는 PricingTableDoc 의 정책 sanity check.
//
// 검사 항목 (hard rejection):
//
//   - HQ margin / Site margin: BidAmount >= 0 AND AskAmount >= 0
//     (마진은 항상 spread 확대 방향 — 음수는 spread 축소 = 고객 유리 = 운영 실수 가능성 높음)
//   - SwapPoint: 음수 허용 (만기/금리차에 따라 부호 반전 가능)
//   - 모든 entry 의 Pair: 비어있으면 안 됨
//
// 검사 안 함 (의도적):
//
//   - Tier / Channel / Site enum 값 — open enum (운영자가 새 등급 추가 가능)
//   - 마진 절대값 상한 — 통화쌍별 가격 단위가 다르므로 단일 상한 부적절
//   - Version 검증 — 운영자 책임 (감사용 단조증가 권장이지만 강제 X)
//
// 모든 위반을 모아 단일 *ValidationError 로 반환 — 부분 실패 안 함.
func (doc PricingTableDoc) Validate() error {
	var v []string

	for i, e := range doc.SwapPoint {
		if e.Pair == "" {
			v = append(v, fmt.Sprintf("swap_point[%d]: pair 비어있음", i))
		}
		if e.Tenor == "" {
			v = append(v, fmt.Sprintf("swap_point[%d] (pair=%s): tenor 비어있음", i, e.Pair))
		}
	}

	for i, e := range doc.HQMargin {
		if e.Pair == "" {
			v = append(v, fmt.Sprintf("hq_margin[%d]: pair 비어있음", i))
		}
		if e.BidAmount < 0 {
			v = append(v, fmt.Sprintf("hq_margin[%d] (pair=%s,tier=%s): bid_amount %g < 0",
				i, e.Pair, e.Tier, e.BidAmount))
		}
		if e.AskAmount < 0 {
			v = append(v, fmt.Sprintf("hq_margin[%d] (pair=%s,tier=%s): ask_amount %g < 0",
				i, e.Pair, e.Tier, e.AskAmount))
		}
	}

	for i, e := range doc.SiteMargin {
		if e.Pair == "" {
			v = append(v, fmt.Sprintf("site_margin[%d]: pair 비어있음", i))
		}
		if e.BidAmount < 0 {
			v = append(v, fmt.Sprintf("site_margin[%d] (pair=%s,channel=%s,site=%s): bid_amount %g < 0",
				i, e.Pair, e.Channel, e.Site, e.BidAmount))
		}
		if e.AskAmount < 0 {
			v = append(v, fmt.Sprintf("site_margin[%d] (pair=%s,channel=%s,site=%s): ask_amount %g < 0",
				i, e.Pair, e.Channel, e.Site, e.AskAmount))
		}
		// Channel="" AND Site="" 동시 와일드카드는 너무 광범위 — reject.
		// (둘 중 하나만 빈 값은 정상적인 fallback rule)
		if e.Channel == "" && e.Site == "" {
			v = append(v, fmt.Sprintf("site_margin[%d] (pair=%s): channel/site 동시 와일드카드 금지 (광범위 fallback)", i, e.Pair))
		}
	}

	if len(v) == 0 {
		return nil
	}
	return &ValidationError{Violations: v}
}

// IsValidationError 는 err 가 ValidationError 인지 확인.
// 운영 핸들러가 400 vs 500 으로 분기할 때 사용.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
