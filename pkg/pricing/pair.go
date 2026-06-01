package pricing

import (
	"sort"
	"sync/atomic"

	"github.com/winwaysystems/wtg/pkg/session"
)

// Pair — 통화쌍 마스터 (DB TB_FXB_CMG004M + CMG006M 통합).
//
// fxsync.Pair 의 mci-price 측 mirror — 같은 JSON 형식 (etcd 키 wtg/pair/{id}).
// CrossFormula 와 매핑되는 Cross 필드는 cross 산식을 그대로 보존.
type Pair struct {
	ID            string  `json:"id"`              // "USDKRW"
	Base          string  `json:"base"`            // "USD"
	Quote         string  `json:"quote"`           // "KRW"
	Kind          string  `json:"kind"`            // "direct" | "cross"
	Symbol        string  `json:"symbol,omitempty"`
	Cross         *Cross  `json:"cross,omitempty"`
	SpotDays      int     `json:"spot_days"`
	ScaleUnit     int     `json:"scale_unit,omitempty"`
	QuoteDecimals int     `json:"quote_decimals"`
	EmpDecimals   int     `json:"emp_decimals,omitempty"`
	PLPair        string  `json:"pl_pair,omitempty"`
	SortOrder     int     `json:"sort_order,omitempty"`
	Active        bool    `json:"active"`
}

// Cross — Pair 의 cross 산식. CrossFormula 와 1:1 매핑 (운영 wire vs 내부 산식).
type Cross struct {
	LegA  string  `json:"leg_a"`
	OpA   string  `json:"op_a"`
	LegB  string  `json:"leg_b"`
	OpB   string  `json:"op_b"`
	Scale float64 `json:"scale,omitempty"`
}

// ToFormula — Cross 를 ComputeCross 가 받는 CrossFormula 로 변환.
// leg pair 문자열 → session.Pair, op 문자열 → CrossOp.
func (c *Cross) ToFormula() CrossFormula {
	if c == nil {
		return CrossFormula{}
	}
	return CrossFormula{
		LegA:  session.Pair(c.LegA),
		OpA:   CrossOp(c.OpA),
		LegB:  session.Pair(c.LegB),
		OpB:   CrossOp(c.OpB),
		Scale: c.Scale,
	}
}

// PairMaster — read-mostly atomic 캐시 (CurrencyMaster 와 동일 패턴).
type PairMaster struct {
	snap atomic.Pointer[pairSnap]
}

type pairSnap struct {
	byID   map[string]Pair
	sorted []Pair
}

// NewPairMaster — 빈 마스터.
func NewPairMaster() *PairMaster {
	m := &PairMaster{}
	m.Replace(nil)
	return m
}

// Replace — 통째 교체.
func (m *PairMaster) Replace(pairs []Pair) {
	snap := &pairSnap{
		byID:   make(map[string]Pair, len(pairs)),
		sorted: make([]Pair, 0, len(pairs)),
	}
	for _, p := range pairs {
		if p.ID == "" {
			continue
		}
		snap.byID[p.ID] = p
		snap.sorted = append(snap.sorted, p)
	}
	sort.SliceStable(snap.sorted, func(i, j int) bool {
		if snap.sorted[i].SortOrder != snap.sorted[j].SortOrder {
			return snap.sorted[i].SortOrder < snap.sorted[j].SortOrder
		}
		return snap.sorted[i].ID < snap.sorted[j].ID
	})
	m.snap.Store(snap)
}

// Get — ID 로 단일 Pair.
func (m *PairMaster) Get(id string) (Pair, bool) {
	s := m.snap.Load()
	if s == nil {
		return Pair{}, false
	}
	p, ok := s.byID[id]
	return p, ok
}

// List — 정렬된 전체 목록 (snapshot 복사본).
func (m *PairMaster) List() []Pair {
	s := m.snap.Load()
	if s == nil {
		return nil
	}
	out := make([]Pair, len(s.sorted))
	copy(out, s.sorted)
	return out
}

// CrossFormulas — kind=cross 인 active pair 들의 산식 매핑.
// CrossRateConsumer.ReplaceFormulas 에 그대로 주입 가능.
// key 는 결과 cross pair (예: "EUR/KRW") — Base/Quote 로 빌드.
func (m *PairMaster) CrossFormulas() map[session.Pair]CrossFormula {
	s := m.snap.Load()
	if s == nil {
		return nil
	}
	out := make(map[session.Pair]CrossFormula, len(s.sorted))
	for _, p := range s.sorted {
		if !p.Active || p.Kind != "cross" || p.Cross == nil {
			continue
		}
		key := session.Pair(p.Base + "/" + p.Quote)
		out[key] = p.Cross.ToFormula()
	}
	return out
}

// Size — 등록된 pair 수.
func (m *PairMaster) Size() int {
	s := m.snap.Load()
	if s == nil {
		return 0
	}
	return len(s.byID)
}
