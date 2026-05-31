package pricing

import (
	"fmt"
	"time"
)

// Calendar — 영업일 판정 추상화.
//
// 1단계 (weekend-only) 에서는 토/일 만 비영업. 운영에서는 HolidayCalendar 로
// 공휴일 set 까지 반영. 통화 caprilly 별로 다른 캘린더가 필요한 경우 (USD/KRW
// = 한국 + 미국 휴일 union) 후속 단계에서 PerPairCalendar 도입.
type Calendar interface {
	// IsBusinessDay — d 가 영업일이면 true.
	// 호출자는 calendar date (year/month/day) 만 사용 가정 — 시각 부분은 무관.
	IsBusinessDay(d time.Time) bool
}

// WeekendCalendar — 주말 (토/일) 만 비영업. 휴일 미반영.
//
// 기존 IsBusinessDay() 자유 함수의 의미와 동등 — backward compat 의 default.
type WeekendCalendar struct{}

// IsBusinessDay 구현.
func (WeekendCalendar) IsBusinessDay(d time.Time) bool {
	wd := d.Weekday()
	return wd != time.Saturday && wd != time.Sunday
}

// HolidayCalendar — 주말 + 휴일 set. 휴일 키는 UTC date 의 "YYYY-MM-DD" 문자열.
//
// 운영 데이터:
//   - admin UI 의 휴일 페이지에서 추가/삭제 → etcd → PricingTableDoc.Holidays
//   - BuildPricingTable 시점에 본 캘린더 빌드.
//   - hot reload 시 PricingTable.Replace 와 함께 캘린더도 통째 교체 (immutable).
type HolidayCalendar struct {
	holidays map[string]struct{}
}

// NewHolidayCalendar — 휴일 set 으로부터 캘린더 빌드. 입력 t 의 location 무관 —
// calendar date (year/month/day) 만 키로 사용.
func NewHolidayCalendar(holidays []time.Time) *HolidayCalendar {
	c := &HolidayCalendar{holidays: make(map[string]struct{}, len(holidays))}
	for _, h := range holidays {
		c.holidays[holidayKey(h)] = struct{}{}
	}
	return c
}

// IsBusinessDay — 주말 또는 휴일이면 false.
func (c *HolidayCalendar) IsBusinessDay(d time.Time) bool {
	wd := d.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	if _, ok := c.holidays[holidayKey(d)]; ok {
		return false
	}
	return true
}

// Count — 등록된 휴일 수. 운영 가시화용.
func (c *HolidayCalendar) Count() int {
	return len(c.holidays)
}

// holidayKey — date 의 "YYYY-MM-DD" 문자열. location 무시.
func holidayKey(t time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
}

// ParseHolidayDate — "YYYY-MM-DD" 문자열을 UTC 자정 time.Time 으로 파싱.
// 휴일 데이터 source 가 string 형태 (etcd / JSON) 일 때 사용.
func ParseHolidayDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// Cal — PricingTable 의 활성 캘린더. nil 이면 WeekendCalendar 반환.
//
// 호출자는 본 메소드만 사용 권장 — PricingTable.Calendar 필드 직접 접근 시
// nil-check 누락 위험.
func (t *PricingTable) Cal() Calendar {
	if t.Calendar != nil {
		return t.Calendar
	}
	return WeekendCalendar{}
}
