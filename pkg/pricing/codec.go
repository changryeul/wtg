package pricing

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/pkg/session"
)

// PricingTableDoc 은 PricingTable 의 JSON 직렬화 모양 — Map 키가 struct 라
// 직접 marshal 이 안 되므로 리스트로 평탄화한다.
//
// mci-admin 이 운영 콘솔에서 작성하는 JSON 도 이 형식 — etcd 에 그대로 저장하고
// mci-price 의 watcher 가 ParsePricingTable 으로 PricingTable 을 재구성.
//
// 5-layer margin design:
//  1. HQMargin       — 본점 마진 (pair × tier)
//  2. SiteMargin     — 영업점/채널 마진 (pair × channel × site)
//  3. CustomerMargin — 고객 별도 마진 (customer_id × pair) — P1 신규
//  4. SwapPoint      — forward 스왑 (pair × tenor)
//  5. TimeWindows    — 시간대 매핑 (window 이름 → 시각 범위) — P1 신규
//
// 모든 마진 entry 의 `window` 필드는 optional — 비면 모든 시간대 적용.
type PricingTableDoc struct {
	Version        int64              `json:"version"`
	TimeWindows    []TimeWindowDoc    `json:"time_windows,omitempty"` // P1 신규
	SwapPoint      []SwapEntryDoc     `json:"swap_point,omitempty"`
	HQMargin       []HQEntryDoc       `json:"hq_margin,omitempty"`
	SiteMargin     []SiteEntryDoc     `json:"site_margin,omitempty"`
	CustomerMargin []CustomerEntryDoc `json:"customer_margin,omitempty"` // P1 신규

	// P5 6단계 — 영업일 캘린더의 휴일 set ("YYYY-MM-DD" 문자열).
	// 비어있으면 weekend-only. value_date 보간 / SPOT 결제일 산정에 영향.
	Holidays []string `json:"holidays,omitempty"`
}

// TimeWindowDoc — 시간대 범위 정의 (예: regular = 평일 09:00~15:30 KST).
//
//   - Name        : 마진 entry 의 window 필드에서 참조하는 라벨 (대소문자 구분 X).
//   - Start / End : "HH:MM" (24h). 운영 권장 — 외부 시스템 사용 시 동일 timezone.
//   - TZ          : IANA timezone (예: "Asia/Seoul"). 비면 "UTC".
//   - Days        : 적용 요일 ("MON-FRI" 또는 "MON,TUE,WED,THU,FRI" 또는 "*" 전체). 비면 매일.
//   - ComplementOf: 다른 window 이름의 보집합 — 예: off_hours = regular 의 반대 시간.
//     이 경우 Start/End/Days/TZ 무시. Name + ComplementOf 만 사용.
type TimeWindowDoc struct {
	Name         string `json:"name"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	TZ           string `json:"tz,omitempty"`
	Days         string `json:"days,omitempty"`
	ComplementOf string `json:"complement_of,omitempty"`
}

// SwapEntryDoc — 한 (pair, tenor) 의 스왑포인트.
type SwapEntryDoc struct {
	Pair      session.Pair `json:"pair"`
	Tenor     Tenor        `json:"tenor"`
	BidAmount float64      `json:"bid_amount"`
	AskAmount float64      `json:"ask_amount"`
}

// HQEntryDoc — 한 (pair, tier) 의 본점 마진. Tier="" 는 와일드카드.
//
// Window 가 비어있으면 모든 시간대 적용 (current behavior). 채우면 해당 window
// 활성 시각 동안만 적용. 같은 (Pair, Tier) 가 여러 window 로 분기 가능.
type HQEntryDoc struct {
	Pair      session.Pair `json:"pair"`
	Tier      session.Tier `json:"tier"`
	BidAmount float64      `json:"bid_amount"`
	AskAmount float64      `json:"ask_amount"`
	Window    string       `json:"window,omitempty"` // P1 신규 — TimeWindow.Name 참조. 비면 모든 시간
}

// SiteEntryDoc — 한 (pair, channel, site) 의 영업점·채널 마진.
// Channel="" 또는 Site="" 는 와일드카드 fallback.
type SiteEntryDoc struct {
	Pair      session.Pair    `json:"pair"`
	Channel   session.Channel `json:"channel"`
	Site      session.Site    `json:"site"`
	BidAmount float64         `json:"bid_amount"`
	AskAmount float64         `json:"ask_amount"`
	Window    string          `json:"window,omitempty"` // P1 신규
}

// CustomerEntryDoc — 특정 customer 의 추가/대체 마진 (P1 신규).
//
//   - CustomerID : 고객 식별자 (usid 또는 별도 customer id). edge-price 가 ws
//     연결 시점에 결정해서 mci-price 에 전달.
//   - Pair       : 통화쌍. ""=모든 pair (와일드카드, 운영 권장 X).
//   - BidDelta / AskDelta : Mode=add 면 delta (HQ+Site 에 누적), Mode=override 면 절대 마진.
//   - Mode       : "add" (default) — 누적. "override" — HQ+Site 무시하고 customer 단독.
//   - Priority   : 여러 customer entry 매칭 시 큰 값 우선. default 0.
//   - Window     : TimeWindow 이름 참조. 비면 모든 시간.
//
// 운영 의미:
//   - VIP 고객 별도 할인 → mode=add, BidDelta/AskDelta 음수
//   - 특별 계약 고객 → mode=override, 별도 마진 단독 적용
//   - 시간대별 차등 → 같은 customer 의 여러 entry × window
type CustomerEntryDoc struct {
	CustomerID string       `json:"customer_id"`
	Pair       session.Pair `json:"pair,omitempty"`
	BidDelta   float64      `json:"bid_delta"`
	AskDelta   float64      `json:"ask_delta"`
	Mode       string       `json:"mode,omitempty"` // "add" (default) | "override"
	Priority   int          `json:"priority,omitempty"`
	Window     string       `json:"window,omitempty"`
}

// ParsePricingTable 은 JSON 바이트를 PricingTable 로 변환한다.
// 동일 키가 중복되면 뒤에 나온 entry 가 이긴다.
func ParsePricingTable(body []byte) (*PricingTable, error) {
	var doc PricingTableDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("pricing: PricingTable JSON 파싱: %w", err)
	}
	return BuildPricingTable(doc), nil
}

// BuildPricingTable 은 DTO 로부터 PricingTable 을 빌드한다.
//
// P1 추가:
//   - TimeWindows 를 map[name]TimeWindowRule 로 빌드 (이름 lowercase).
//   - CustomerMargin 을 priority desc 정렬된 []CustomerRule 로 빌드.
//   - HQ/Site entry 의 Window 필드는 P1 에서 보존만 (Apply 통합은 Phase 2).
//     현 동작 backward compat — Window 비어있으면 기존 lookup 그대로.
func BuildPricingTable(doc PricingTableDoc) *PricingTable {
	t := &PricingTable{
		Version:     doc.Version,
		SwapPoint:   make(map[SwapKey]Margin, len(doc.SwapPoint)),
		HQMargin:    make(map[HQKey]Margin, len(doc.HQMargin)),
		SiteMargin:  make(map[SiteKey]Margin, len(doc.SiteMargin)),
		TimeWindows: make(map[string]TimeWindowRule, len(doc.TimeWindows)),
	}
	for _, e := range doc.SwapPoint {
		t.SwapPoint[SwapKey{Pair: e.Pair, Tenor: e.Tenor}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	for _, e := range doc.HQMargin {
		t.HQMargin[HQKey{Pair: e.Pair, Tier: e.Tier, Window: normalizeName(e.Window)}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	for _, e := range doc.SiteMargin {
		t.SiteMargin[SiteKey{Pair: e.Pair, Channel: e.Channel, Site: e.Site, Window: normalizeName(e.Window)}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	// TimeWindows — 이름 lowercase 정규화.
	for _, w := range doc.TimeWindows {
		rule := TimeWindowRule{
			Name:         w.Name,
			TZ:           w.TZ,
			ComplementOf: w.ComplementOf,
			StartMin:     -1,
			EndMin:       -1,
		}
		rule.StartMin = parseHHMM(w.Start)
		rule.EndMin = parseHHMM(w.End)
		rule.DaysMask = parseDays(w.Days)
		t.TimeWindows[normalizeName(w.Name)] = rule
	}
	// CustomerMargin — priority desc 정렬 (간단 O(N^2), N 작음). mode default "add".
	t.CustomerMargin = make([]CustomerRule, 0, len(doc.CustomerMargin))
	for _, e := range doc.CustomerMargin {
		mode := e.Mode
		if mode == "" {
			mode = "add"
		}
		t.CustomerMargin = append(t.CustomerMargin, CustomerRule{
			CustomerID: e.CustomerID,
			Pair:       e.Pair,
			BidDelta:   e.BidDelta,
			AskDelta:   e.AskDelta,
			Mode:       mode,
			Priority:   e.Priority,
			Window:     normalizeName(e.Window),
		})
	}
	// 삽입 정렬 (priority desc) — 운영 entry 수 작음 가정.
	for i := 1; i < len(t.CustomerMargin); i++ {
		for j := i; j > 0 && t.CustomerMargin[j-1].Priority < t.CustomerMargin[j].Priority; j-- {
			t.CustomerMargin[j-1], t.CustomerMargin[j] = t.CustomerMargin[j], t.CustomerMargin[j-1]
		}
	}
	// P5 6단계 — 휴일 캘린더 빌드. 빈 set 이면 Calendar 는 nil 유지 → Cal() 이
	// WeekendCalendar 반환.
	if len(doc.Holidays) > 0 {
		dates := make([]time.Time, 0, len(doc.Holidays))
		for _, s := range doc.Holidays {
			if d, err := ParseHolidayDate(s); err == nil {
				dates = append(dates, d)
			}
		}
		if len(dates) > 0 {
			t.Calendar = NewHolidayCalendar(dates)
		}
	}
	return t
}

// normalizeName — TimeWindow 이름 lookup 키. lowercase + trim.
func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// parseHHMM — "09:30" → 570 (분). 실패 시 -1.
func parseHHMM(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return -1
	}
	h, errH := strconvAtoi(parts[0])
	m, errM := strconvAtoi(parts[1])
	if errH != nil || errM != nil {
		return -1
	}
	if h < 0 || h > 24 || m < 0 || m >= 60 {
		return -1
	}
	if h == 24 && m == 0 {
		return 1440
	}
	return h*60 + m
}

// parseDays — "MON-FRI" / "MON,TUE,WED" / "*" / "" → DaysMask (bit 0=Sun).
// "" 또는 "*" 는 0xFF (전체).
func parseDays(s string) uint8 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "*" {
		return 0xFF
	}
	dayIdx := map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
	var mask uint8
	// "MON-FRI" 범위.
	if strings.Contains(s, "-") && !strings.Contains(s, ",") {
		parts := strings.SplitN(s, "-", 2)
		fromI, fromOK := dayIdx[parts[0]]
		toI, toOK := dayIdx[parts[1]]
		if fromOK && toOK {
			for d := fromI; d != (toI+1)%7 && (d-fromI+7)%7 < 7; d = (d + 1) % 7 {
				mask |= 1 << uint(d)
				if d == toI {
					break
				}
			}
		}
		return mask
	}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if d, ok := dayIdx[tok]; ok {
			mask |= 1 << uint(d)
		}
	}
	return mask
}

// strconvAtoi — strconv import 회피용 미니. parseHHMM 내부만 사용.
func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("nan")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
