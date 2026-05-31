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

// HQKey 는 (통화쌍, 고객등급) — 본점 마진은 등급에 의존.
type HQKey struct {
	Pair session.Pair
	Tier session.Tier
}

// SiteKey 는 (통화쌍, 채널, 거래주체) — 영업점/채널 마진은 채널·site 에 의존.
type SiteKey struct {
	Pair    session.Pair
	Channel session.Channel
	Site    session.Site
}

// lookupSwap 은 (pair, tenor) 스왑포인트를 반환. 없으면 zero.
func (t *PricingTable) lookupSwap(pair session.Pair, tenor Tenor) Margin {
	if m, ok := t.SwapPoint[SwapKey{pair, tenor}]; ok {
		return m
	}
	return Margin{}
}

// lookupHQ 는 본점 마진을 반환. 정확매치 → tier="" 와일드카드 → zero.
func (t *PricingTable) lookupHQ(pair session.Pair, tier session.Tier) Margin {
	if m, ok := t.HQMargin[HQKey{pair, tier}]; ok {
		return m
	}
	if m, ok := t.HQMargin[HQKey{pair, ""}]; ok {
		return m
	}
	return Margin{}
}

// lookupSite 는 영업점/채널 마진을 반환.
// fallback 순서: 정확매치 → channel="" → site="" → zero.
// 우선순위는 "site 가 더 강한 식별자"라는 가정에 따른다 (영업점이 채널보다 정책상 명시적).
func (t *PricingTable) lookupSite(pair session.Pair, channel session.Channel, site session.Site) Margin {
	if m, ok := t.SiteMargin[SiteKey{pair, channel, site}]; ok {
		return m
	}
	if m, ok := t.SiteMargin[SiteKey{pair, "", site}]; ok {
		return m
	}
	if m, ok := t.SiteMargin[SiteKey{pair, channel, ""}]; ok {
		return m
	}
	return Margin{}
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
