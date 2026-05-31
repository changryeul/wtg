package pricing

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// PricingTable 은 시세 산출에 필요한 모든 마진/스왑 데이터의 immutable snapshot.
// 한 tick 안에서는 단일 Version 으로 일관되게 적용되어야 하므로,
// 갱신은 항상 새 PricingTable 을 통째로 빌드한 뒤 Store.Replace 로 교체한다.
//
// Lookup 은 specific → general fallback chain 으로 동작한다 — 빈 Tier/Channel/Site
// 키를 와일드카드 entry 로 사용하면 운영자가 일반 규칙 + 예외 규칙을 함께 등록할 수 있다.
//
// P1 신규 — CustomerMargin / TimeWindows (Apply 통합은 Phase 2/3 에서):
//   - CustomerMargin: 특정 고객의 추가/대체 마진. mode=add (default) 면 HQ+Site 에 누적.
//                     mode=override 면 HQ+Site 무시.
//   - TimeWindows: 시간대 이름 → 시각 범위 매핑. 각 마진 entry 의 Window 필드가 참조.
//                  현재 시각 이 매칭되는 window 의 entry 만 Apply 시 사용.
type PricingTable struct {
	Version        int64
	SwapPoint      map[SwapKey]Margin
	HQMargin       map[HQKey]Margin
	SiteMargin     map[SiteKey]Margin
	CustomerMargin []CustomerRule   // P1 — 다중 매칭 + priority 정렬 필요해 슬라이스
	TimeWindows    map[string]TimeWindowRule

	// Calendar — 영업일 캘린더. nil 이면 WeekendCalendar 사용 (Cal() 메소드 통해).
	// 운영자가 admin UI 에서 휴일 등록 → PricingTableDoc.Holidays → BuildPricingTable
	// 시점에 HolidayCalendar 빌드.
	Calendar Calendar
}

// CustomerRule — 빌드된 customer override 규칙. Doc 의 CustomerEntryDoc 의 in-memory 형태.
// 같은 CustomerID 가 여러 entry 가능 (pair × window 별). priority desc 정렬되어
// lookup 시 첫 매칭 사용.
type CustomerRule struct {
	CustomerID string
	Pair       session.Pair // "" = 모든 pair
	BidDelta   float64
	AskDelta   float64
	Mode       string // "add" | "override"
	Priority   int
	Window     string // TimeWindow.Name 참조. "" = 모든 시간.
}

// TimeWindowRule — 빌드된 시간대 규칙. Doc 의 TimeWindowDoc 의 in-memory 형태.
// IsActive(now) 가 hot path 에서 매칭 판정.
type TimeWindowRule struct {
	Name         string
	StartMin     int  // 시작 시각 분 단위 (0=00:00, 1439=23:59). -1 = 미설정 (ComplementOf 인 경우)
	EndMin       int  // 끝 (배타). 24:00 = 1440
	TZ           string
	DaysMask     uint8  // bit 0=Sun, 1=Mon, ..., 6=Sat. 0xFF = 매일.
	ComplementOf string // 비면 None. 채워지면 IsActive 는 다른 window 의 IsActive 반대.
}

// SwapKey 는 (통화쌍, 만기) — 스왑포인트는 만기에만 의존.
type SwapKey struct {
	Pair  session.Pair
	Tenor Tenor
}

// HQKey 는 (통화쌍, 고객등급, 시간대) — 본점 마진은 등급 + 시간대에 의존.
// Window="" 는 모든 시간대 적용 (backward compat).
type HQKey struct {
	Pair   session.Pair
	Tier   session.Tier
	Window string // P2 신규. 비면 모든 시간대. lowercase 정규화 (normalizeName 사용).
}

// SiteKey 는 (통화쌍, 채널, 거래주체, 시간대).
type SiteKey struct {
	Pair    session.Pair
	Channel session.Channel
	Site    session.Site
	Window  string // P2 신규.
}

// lookupSwap 은 (pair, tenor) 스왑포인트를 반환. 없으면 zero.
func (t *PricingTable) lookupSwap(pair session.Pair, tenor Tenor) Margin {
	if m, ok := t.SwapPoint[SwapKey{pair, tenor}]; ok {
		return m
	}
	return Margin{}
}

// lookupHQ — 본점 마진. activeWindows 의 active window 매칭 우선, window="" fallback.
//
// Lookup 순서:
//   1. activeWindows 의 각 window 에 대해 (pair, tier, window) 시도
//   2. (pair, tier, "") — 모든 시간대 적용 entry
//   3. activeWindows 의 각 window 에 대해 (pair, "", window) — tier 와일드카드
//   4. (pair, "", "")
//   5. zero
//
// activeWindows 는 nil 가능 (window 매칭 X, 기존 동작).
func (t *PricingTable) lookupHQ(pair session.Pair, tier session.Tier, activeWindows []string) Margin {
	// 1. (pair, tier, active_window)
	for _, w := range activeWindows {
		if m, ok := t.HQMargin[HQKey{pair, tier, w}]; ok {
			return m
		}
	}
	// 2. (pair, tier, "")
	if m, ok := t.HQMargin[HQKey{pair, tier, ""}]; ok {
		return m
	}
	// 3. (pair, "", active_window)
	for _, w := range activeWindows {
		if m, ok := t.HQMargin[HQKey{pair, "", w}]; ok {
			return m
		}
	}
	// 4. (pair, "", "")
	if m, ok := t.HQMargin[HQKey{pair, "", ""}]; ok {
		return m
	}
	return Margin{}
}

// lookupSite — 영업점/채널 마진. window 매칭 + 기존 channel/site fallback chain.
//
// Lookup 순서 (각 단계는 activeWindows 별 window 매칭 → "" fallback):
//   1. (pair, channel, site)
//   2. (pair, "",      site)
//   3. (pair, channel, "")
//   4. zero
//
// 각 단계 내에서 window 매칭 우선 → 빈 window fallback (위 lookupHQ 와 동일 패턴).
func (t *PricingTable) lookupSite(pair session.Pair, channel session.Channel, site session.Site, activeWindows []string) Margin {
	type k struct {
		c session.Channel
		s session.Site
	}
	chain := []k{{channel, site}, {"", site}, {channel, ""}}
	for _, kk := range chain {
		// window 매칭
		for _, w := range activeWindows {
			if m, ok := t.SiteMargin[SiteKey{pair, kk.c, kk.s, w}]; ok {
				return m
			}
		}
		// window="" fallback
		if m, ok := t.SiteMargin[SiteKey{pair, kk.c, kk.s, ""}]; ok {
			return m
		}
	}
	return Margin{}
}

// ActiveWindows — 현재 시각에 활성인 TimeWindow 이름 목록 (lowercase). 가장
// specific 한 매칭이 먼저. window 미정의 환경에선 빈 슬라이스 — lookup 함수가
// fallback path 만 사용.
func (t *PricingTable) ActiveWindows(now time.Time) []string {
	if len(t.TimeWindows) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.TimeWindows))
	for name, w := range t.TimeWindows {
		if w.IsActive(now, t.TimeWindows) {
			out = append(out, name)
		}
	}
	return out
}

// IsActive — 본 TimeWindow 가 now 시점에 활성인지 판정.
// allWindows: ComplementOf 가 있을 때 다른 window 의 활성 여부 lookup.
func (w *TimeWindowRule) IsActive(now time.Time, allWindows map[string]TimeWindowRule) bool {
	if w.ComplementOf != "" {
		if other, ok := allWindows[strings.ToLower(w.ComplementOf)]; ok {
			return !other.IsActive(now, allWindows)
		}
		return false
	}
	// timezone 적용
	loc := time.UTC
	if w.TZ != "" {
		if l, err := time.LoadLocation(w.TZ); err == nil {
			loc = l
		}
	}
	t := now.In(loc)
	// 요일 매칭. DaysMask=0 또는 0xFF 면 모두 통과.
	if w.DaysMask != 0 && w.DaysMask != 0xFF {
		if w.DaysMask&(1<<uint(t.Weekday())) == 0 {
			return false
		}
	}
	if w.StartMin < 0 || w.EndMin <= w.StartMin {
		// 미설정 또는 invalid 면 항상 활성 (보수적)
		return true
	}
	mn := t.Hour()*60 + t.Minute()
	return mn >= w.StartMin && mn < w.EndMin
}

// Store 는 PricingTable snapshot 을 atomic 으로 보관한다.
// hot path (Apply 호출자) 는 Load 만 사용하면 되며 lock 이 필요 없다.
// 갱신자(예: mci-price 의 etcd watcher)는 새 PricingTable 을 빌드한 뒤 Replace 호출.
type Store struct {
	p atomic.Pointer[PricingTable]
}

// NewStore 는 빈 PricingTable 로 초기화한 Store 를 반환한다.
func NewStore() *Store {
	s := &Store{}
	s.Replace(&PricingTable{
		SwapPoint:   map[SwapKey]Margin{},
		HQMargin:    map[HQKey]Margin{},
		SiteMargin:  map[SiteKey]Margin{},
		TimeWindows: map[string]TimeWindowRule{},
	})
	return s
}

// Replace 는 활성 snapshot 을 통째로 교체한다 (lock-free, atomic).
// 호출자는 Replace 이후 해당 PricingTable 을 다시 수정하면 안 된다 (immutable).
func (s *Store) Replace(t *PricingTable) {
	s.p.Store(t)
}

// Load 는 현재 활성 snapshot 을 반환한다. 반환값은 read-only.
func (s *Store) Load() *PricingTable {
	return s.p.Load()
}
