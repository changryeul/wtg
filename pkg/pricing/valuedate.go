package pricing

import (
	"errors"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Package valuedate — broken-date forward 마진 산출 인프라.
//
// 고객이 "결제일 = 2026-06-15" 같은 임의 날짜를 선택하면, 그 날짜가 표준 tenor
// (SPOT/1W/1M/3M/...) 의 결제일과 일치하지 않을 수 있다 — 이를 broken-date 라
// 부른다. 두 인접 standard tenor 의 swap_point 를 일수 비례 (선형) 보간해
// 임의 결제일의 swap 를 산출한다.
//
// 캘린더: P5 5단계 1차는 weekend-only (토/일 비영업). 휴일 데이터는 후속.
//
// 산식 (선형):
//
//	prev_tenor = max(tenor where DefaultTenorDays[tenor] <= offset_days)
//	next_tenor = min(tenor where DefaultTenorDays[tenor] >  offset_days)
//	w = (offset - prev.days) / (next.days - prev.days)
//	swap_bid = prev.bid + (next.bid - prev.bid) * w
//	swap_ask = prev.ask + (next.ask - prev.ask) * w
//
// 양 끝 (offset < 모든 tenor 의 최소 days 또는 > 최대 days) 은 extrapolation
// 거절 — 운영 안전 우선. (실 거래 한도를 1Y 이하로 제한해 정책적으로 cover)

// DefaultTenorDays — SPOT 대비 영업일 offset (일반 시장 컨벤션 + ACT/365).
// 운영 환경의 캘린더가 다르면 PricingTable 에 별도 tenor_calendar 필드 확장
// 또는 admin 정의 후 본 map 을 overwrite 하는 식으로 운영.
//
// 주의: 일부 컨벤션 (특히 USD/KRW) 는 SPOT 이 T+2 가 아닌 T+1 인 경우가 있다.
// SpotDate() 헬퍼 분리.
var DefaultTenorDays = map[Tenor]int{
	TenorTOD:  -2, // SPOT - 2 (= 거래일 T+0)
	TenorTOM:  -1, // SPOT - 1
	TenorSpot: 0,
	Tenor1W:   7,
	Tenor2W:   14,
	Tenor1M:   30,
	Tenor2M:   61,
	Tenor3M:   91,
	Tenor6M:   183,
	Tenor9M:   274,
	Tenor1Y:   365,
}

// ErrOutOfRange — InterpolateSwap 가 양 끝 extrapolation 으로 거절될 때.
var ErrOutOfRange = errors.New("pricing: value date 가 등록된 tenor 범위 밖 (extrapolation 비활성)")

// ErrNoSwap — 본 pair 에 swap_point entry 가 하나도 없을 때.
var ErrNoSwap = errors.New("pricing: 해당 pair 의 swap_point 미등록")

// IsBusinessDay — weekend-only 캘린더. 토/일 false, 평일 true.
// 휴일 (공휴일) 은 미반영 — 후속 단계에서 Calendar interface 도입.
func IsBusinessDay(d time.Time) bool {
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// AddBusinessDays — d 에서 n 영업일 추가/감산 후의 날짜. n=0 이면 d 그대로
// (영업일 여부 무관 — 호출자 책임). n>0 면 미래, n<0 면 과거.
//
// 시각 부분은 보존하지 않고 자정 (UTC 동일 시각대) 으로 truncate 하지 않는다 —
// 호출자가 미리 truncate 권장.
func AddBusinessDays(d time.Time, n int) time.Time {
	if n == 0 {
		return d
	}
	step := 1
	if n < 0 {
		step = -1
		n = -n
	}
	cur := d
	for i := 0; i < n; i++ {
		cur = cur.AddDate(0, 0, step)
		for !IsBusinessDay(cur) {
			cur = cur.AddDate(0, 0, step)
		}
	}
	return cur
}

// BusinessDaysBetween — from 부터 to 까지의 영업일 차이. from < to 면 양수.
// from / to 자체가 영업일 아니어도 카운트는 영업일만.
//
// 정의 (half-open):
//   from 다음 영업일부터 to 까지 (to 포함) 영업일 수.
//   from == to 면 0.
//
// 예:
//   월 → 금 = 4 영업일
//   금 → 다음 월 = 1 영업일 (토/일 skip)
//   토 → 다음 월 = 1 영업일 (토 다음 영업일은 월, 월부터 카운트)
func BusinessDaysBetween(from, to time.Time) int {
	if from.Equal(to) {
		return 0
	}
	sign := 1
	if to.Before(from) {
		from, to = to, from
		sign = -1
	}
	// 자정 정렬 (시각 부분 무시).
	from = startOfDay(from)
	to = startOfDay(to)
	n := 0
	cur := from
	for cur.Before(to) {
		cur = cur.AddDate(0, 0, 1)
		if IsBusinessDay(cur) {
			n++
		}
	}
	return n * sign
}

// startOfDay — t 의 calendar date 의 자정을 UTC 로 정규화.
//
// 두 input 의 location 이 달라도 (예: KST spot vs UTC parsed value_date) 같은
// 기준으로 비교해야 영업일 카운터가 일관됨. weekday 도 calendar date 그대로
// (KST 자정의 weekday == UTC 자정의 weekday) 보존되어 IsBusinessDay 영향 X.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// SpotDate — 거래일 (now) 기준 SPOT 결제일. 일반적으로 T+2 (영업일).
// 일부 통화쌍 (USD/CAD 등) 은 T+1. spotDays 인자로 명시 — 기본 2.
func SpotDate(now time.Time, spotDays int) time.Time {
	if spotDays < 0 {
		spotDays = 0
	}
	return AddBusinessDays(startOfDay(now), spotDays)
}

// SwapInterpolation — 보간 결과 + 어떤 두 점 사이에서 어떤 가중치로 산출됐는지.
// quote_id Record 에 모두 보존되어 분쟁 시 audit 가능.
type SwapInterpolation struct {
	Margin     Margin // 보간된 swap (bid/ask)
	From       Tenor  // 하한 tenor (offsetDays 이하 중 최대)
	To         Tenor  // 상한 tenor (offsetDays 초과 중 최소)
	FromDays   int
	ToDays     int
	OffsetDays int     // SPOT 대비 일수
	Weight     float64 // 0~1 (1 이면 next, 0 이면 prev). offsetDays==fromDays 면 0.

	// Exact — 보간이 아닌 정확 매칭 (offsetDays == fromDays == toDays).
	// 이때 From 만 의미 있고 To/Weight 는 0.
	Exact bool
}

// InterpolateSwap — 본 pair 의 swap_point 들을 offsetDays 기준 선형 보간.
//
// 반환:
//   - Exact 매칭 (offsetDays 가 정확히 어떤 tenor 의 days 와 일치) → 그 entry
//     의 swap 그대로 + result.Exact = true.
//   - 인접 두 tenor 가 있고 그 사이 → 선형 보간 + result.Exact = false.
//   - 양 끝 (모든 tenor 의 최소 days 보다 작거나 최대보다 큼) → ErrOutOfRange.
//   - swap_point 가 본 pair 에 없음 → ErrNoSwap.
//
// 영업일 단위 offset 기대 — calendar day 사용 시 결과는 여전히 정합하지만
// DefaultTenorDays 의 의미와 일관성 위해 호출자가 BusinessDaysBetween 으로
// 산출 권장.
func (t *PricingTable) InterpolateSwap(pair session.Pair, offsetDays int) (SwapInterpolation, error) {
	// 본 pair 의 (tenor, days, margin) 집합 수집.
	type item struct {
		tenor Tenor
		days  int
		m     Margin
	}
	items := make([]item, 0, 8)
	for k, m := range t.SwapPoint {
		if k.Pair != pair {
			continue
		}
		days, ok := DefaultTenorDays[k.Tenor]
		if !ok {
			// DefaultTenorDays 에 없는 tenor 는 보간 무시 — 등록 규칙으로만 추가 가능.
			continue
		}
		items = append(items, item{k.Tenor, days, m})
	}
	if len(items) == 0 {
		return SwapInterpolation{}, ErrNoSwap
	}
	// days 기준 정렬 (간단 삽입 정렬 — N 작음).
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].days > items[j].days; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}

	// Exact 매칭 우선.
	for _, it := range items {
		if it.days == offsetDays {
			return SwapInterpolation{
				Margin: it.m, From: it.tenor, FromDays: it.days,
				To: it.tenor, ToDays: it.days,
				OffsetDays: offsetDays, Weight: 0, Exact: true,
			}, nil
		}
	}

	// prev (offset 이하 중 최대) + next (offset 초과 중 최소) 탐색.
	var prev, next *item
	for i := range items {
		it := &items[i]
		if it.days < offsetDays {
			prev = it
		}
		if it.days > offsetDays {
			next = it
			break
		}
	}
	if prev == nil || next == nil {
		return SwapInterpolation{OffsetDays: offsetDays}, ErrOutOfRange
	}

	w := float64(offsetDays-prev.days) / float64(next.days-prev.days)
	return SwapInterpolation{
		Margin: Margin{
			BidAmount: prev.m.BidAmount + (next.m.BidAmount-prev.m.BidAmount)*w,
			AskAmount: prev.m.AskAmount + (next.m.AskAmount-prev.m.AskAmount)*w,
		},
		From: prev.tenor, FromDays: prev.days,
		To: next.tenor, ToDays: next.days,
		OffsetDays: offsetDays, Weight: w, Exact: false,
	}, nil
}
