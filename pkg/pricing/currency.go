package pricing

import (
	"sort"
	"sync/atomic"
)

// Currency — 통화 마스터 (DB TB_FXB_CMG005M 의 도메인 매핑).
//
// 운영 SoT 는 DB. fx-sync 가 etcd 로 미러링 → mci-price 가 watch 로 메모리
// 캐시 → 다른 svc 가 REST 로 read.
//
// Currency 와 PricingTable 의 의미:
//   - Currency:    "USD 가 존재한다" — 통화 자체의 카탈로그 (분기/년 단위 변경).
//   - PricingTable: "USD/KRW VIP 마진 0.02" — 마진 정책 (분~시간 단위 변경).
type Currency struct {
	Code          string `json:"code"`            // "USD", "KRW", "JPY"
	Name          string `json:"name"`            // "미국 달러"
	RefCode       string `json:"ref_code,omitempty"` // ISO 4217 numeric 등
	DecimalPlaces int    `json:"decimal_places"`  // 호가 표시 소수자리
	PrecisionKind string `json:"precision_kind,omitempty"`
	SortOrder     int    `json:"sort_order,omitempty"`
	Active        bool   `json:"active"`
}

// CurrencyMaster — read-mostly 메모리 캐시. atomic.Pointer 로 lock-free read.
//
// hot path 호출자 (Get / List) 는 lock 없이 사용. EtcdCurrencyWatcher 가 변경
// 발생 시 새 snapshot 빌드 후 Replace.
type CurrencyMaster struct {
	snap atomic.Pointer[currencySnap]
}

// currencySnap — 빌드된 immutable snapshot.
type currencySnap struct {
	byCode map[string]Currency
	sorted []Currency // SortOrder 오름차순
}

// NewCurrencyMaster — 빈 마스터.
func NewCurrencyMaster() *CurrencyMaster {
	m := &CurrencyMaster{}
	m.Replace(nil)
	return m
}

// Replace — 전체 entry 를 새 snapshot 으로 교체. nil 이면 빈 set.
func (m *CurrencyMaster) Replace(currencies []Currency) {
	snap := &currencySnap{
		byCode: make(map[string]Currency, len(currencies)),
		sorted: make([]Currency, 0, len(currencies)),
	}
	for _, c := range currencies {
		if c.Code == "" {
			continue
		}
		snap.byCode[c.Code] = c
		snap.sorted = append(snap.sorted, c)
	}
	sort.SliceStable(snap.sorted, func(i, j int) bool {
		if snap.sorted[i].SortOrder != snap.sorted[j].SortOrder {
			return snap.sorted[i].SortOrder < snap.sorted[j].SortOrder
		}
		return snap.sorted[i].Code < snap.sorted[j].Code
	})
	m.snap.Store(snap)
}

// Get — code 의 Currency 반환. 없으면 false.
func (m *CurrencyMaster) Get(code string) (Currency, bool) {
	s := m.snap.Load()
	if s == nil {
		return Currency{}, false
	}
	c, ok := s.byCode[code]
	return c, ok
}

// List — SortOrder 정렬된 전체 목록 (immutable snapshot 의 복사본).
func (m *CurrencyMaster) List() []Currency {
	s := m.snap.Load()
	if s == nil {
		return nil
	}
	out := make([]Currency, len(s.sorted))
	copy(out, s.sorted)
	return out
}

// Size — 등록된 통화 수.
func (m *CurrencyMaster) Size() int {
	s := m.snap.Load()
	if s == nil {
		return 0
	}
	return len(s.byCode)
}
