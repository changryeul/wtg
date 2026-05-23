package quote

import (
	"sync/atomic"

	"github.com/winwaysystems/wtg/pkg/session"
)

// SymbolEntry 는 외부 시세 source 가 사용하는 심볼과 WTG 내부 session.Pair 의
// 매핑 1건. Active 가 false 면 mci-price 는 해당 심볼의 tick 을 drop 한다.
type SymbolEntry struct {
	Symbol string       `json:"symbol"`  // 외부 표기 (예: "USDKRW")
	Pair   session.Pair `json:"pair"`    // WTG 내부 표기 (예: "USD/KRW")
	Active bool         `json:"active"`  // 시세 처리 활성 여부
}

// SymbolMap 은 외부 심볼 ↔ session.Pair 매핑의 immutable snapshot 을 atomic
// 으로 보관한다. hot path (tick 디코딩) 는 Lookup 만 사용 — lock 없음.
//
// 운영 흐름:
//
//   - mci-admin 이 etcd 에 매핑 목록 갱신
//   - mci-price 의 watcher 가 변경 감지 → 새 snapshot 빌드 → Replace 호출
//   - 호출자(Hot path) 는 Lookup 만 → 영향 없음
type SymbolMap struct {
	p atomic.Pointer[symbolMapData]
}

// symbolMapData 는 양방향 색인을 가진 immutable snapshot.
type symbolMapData struct {
	bySymbol map[string]SymbolEntry
	byPair   map[session.Pair]string
}

// NewSymbolMap 은 빈 매핑으로 초기화한 SymbolMap 을 반환한다.
func NewSymbolMap() *SymbolMap {
	m := &SymbolMap{}
	m.Replace(nil)
	return m
}

// Replace 는 전체 매핑을 통째로 교체한다 (atomic).
// 동일 Symbol/Pair 가 중복되면 뒤에 나온 entry 가 이긴다.
func (m *SymbolMap) Replace(entries []SymbolEntry) {
	d := &symbolMapData{
		bySymbol: make(map[string]SymbolEntry, len(entries)),
		byPair:   make(map[session.Pair]string, len(entries)),
	}
	for _, e := range entries {
		d.bySymbol[e.Symbol] = e
		d.byPair[e.Pair] = e.Symbol
	}
	m.p.Store(d)
}

// Lookup 은 외부 symbol 에 대응하는 session.Pair 와 활성 여부를 반환한다.
// found=false 면 등록되지 않은 심볼.
// 호출자는 found && active 인 경우에만 정상 처리하면 된다.
func (m *SymbolMap) Lookup(symbol string) (pair session.Pair, active bool, found bool) {
	d := m.p.Load()
	if d == nil {
		return "", false, false
	}
	e, ok := d.bySymbol[symbol]
	if !ok {
		return "", false, false
	}
	return e.Pair, e.Active, true
}

// Reverse 는 session.Pair 에 대응하는 외부 symbol 을 반환한다.
// publish path 에서 routing-key / 외부 표기 변환에 사용.
func (m *SymbolMap) Reverse(pair session.Pair) (symbol string, found bool) {
	d := m.p.Load()
	if d == nil {
		return "", false
	}
	s, ok := d.byPair[pair]
	return s, ok
}

// All 은 현재 snapshot 의 모든 entry 를 반환한다 (정렬 보장 X).
// 모니터링 / admin 화면용. hot path 에서 호출하지 말 것.
func (m *SymbolMap) All() []SymbolEntry {
	d := m.p.Load()
	if d == nil {
		return nil
	}
	out := make([]SymbolEntry, 0, len(d.bySymbol))
	for _, e := range d.bySymbol {
		out = append(out, e)
	}
	return out
}

// Size 는 등록된 매핑 수.
func (m *SymbolMap) Size() int {
	d := m.p.Load()
	if d == nil {
		return 0
	}
	return len(d.bySymbol)
}
