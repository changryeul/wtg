// Package instrument — 통합 심볼 카탈로그. 자산군-무관(asset-neutral) 하게
// symbol → 상류(upstream)/자산분류(asset_class)/시장(market) 을 조회한다.
//
// 범용 MCI 확장의 핵심 신규 추상: FX(pkg/quote.SymbolEntry, symbol↔pair)와
// KRX(시세 스트림 내장 code) 가 각자 심볼을 아는 방식이 달라, 통합 엣지에서
// "이 심볼은 어느 상류로 보내야 하나" 를 판별할 공용 근거가 없었다. 본 카탈로그가
// 그 단일 출처다. FX 도메인 타입을 import 하지 않는다(단방향 DAG, leaf).
//
// docs/unified-quote-edge-design.md §6.
package instrument

import "sync/atomic"

// AssetClass — 자산군. 클라 굵은 분기 + 라우팅 보조.
type AssetClass string

const (
	AssetFX      AssetClass = "FX"
	AssetFuture  AssetClass = "FUTURE"
	AssetOption  AssetClass = "OPTION"
	AssetBond    AssetClass = "BOND"
	AssetEquity  AssetClass = "EQUITY"
	AssetUnknown AssetClass = "UNKNOWN"
)

// Upstream 태그 — 엣지가 fan-in 할 상류 식별. 값은 라우팅에만 쓰이는 불투명 태그.
const (
	UpstreamFX  = "fx"  // mci-price (gRPC SubscribeQuote)
	UpstreamKRX = "krx" // KRX 트랙 (Phase 2b 에서 fan-in 배선)
)

// Instrument — 카탈로그 1건. symbol 은 통합 라우팅/식별 키(FX "USD/KRW",
// KRX "101V6000" 등 불투명 문자열).
type Instrument struct {
	Symbol     string     `json:"symbol"`
	AssetClass AssetClass `json:"asset_class"`
	Market     string     `json:"market"`   // 예: OTC, KRX
	Upstream   string     `json:"upstream"` // 엣지 라우팅 태그 (fx|krx)
	Active     bool       `json:"active"`
}

// Catalog — Instrument 의 immutable snapshot 을 atomic 으로 보관한다.
// hot path(구독 라우팅)는 Lookup/Route 만 사용 — lock 없음. mci-admin 이 etcd
// 갱신 → watcher 가 Replace 로 통째 교체 (SymbolMap 과 동일 패턴).
type Catalog struct {
	p atomic.Pointer[map[string]Instrument]
}

// NewCatalog — 빈 카탈로그.
func NewCatalog() *Catalog {
	c := &Catalog{}
	c.Replace(nil)
	return c
}

// Replace — 전체 카탈로그 통째 교체 (atomic). 동일 Symbol 중복 시 뒤가 이긴다.
func (c *Catalog) Replace(items []Instrument) {
	m := make(map[string]Instrument, len(items))
	for _, it := range items {
		m[it.Symbol] = it
	}
	c.p.Store(&m)
}

// Lookup — symbol 의 Instrument. found=false 면 미등록.
func (c *Catalog) Lookup(symbol string) (Instrument, bool) {
	m := c.p.Load()
	if m == nil {
		return Instrument{}, false
	}
	it, ok := (*m)[symbol]
	return it, ok
}

// Route — symbol 의 상류 태그. 미등록이거나 Active=false 면 ("", false).
// 엣지는 이 결과로 심볼을 어느 상류 adapter 에 등록할지 결정한다.
func (c *Catalog) Route(symbol string) (upstream string, ok bool) {
	it, found := c.Lookup(symbol)
	if !found || !it.Active {
		return "", false
	}
	return it.Upstream, true
}

// RouteAll — 심볼 목록을 상류별로 분류한다. active 미등록/미상은 unknown 으로.
// 통합 엣지가 혼합 구독 요청을 상류별 서브구독으로 쪼갤 때 사용.
func (c *Catalog) RouteAll(symbols []string) (byUpstream map[string][]string, unknown []string) {
	byUpstream = make(map[string][]string)
	for _, s := range symbols {
		up, ok := c.Route(s)
		if !ok {
			unknown = append(unknown, s)
			continue
		}
		byUpstream[up] = append(byUpstream[up], s)
	}
	return byUpstream, unknown
}

// All — 현재 snapshot 전체 (정렬 보장 X). 진단/admin 용, hot path 금지.
func (c *Catalog) All() []Instrument {
	m := c.p.Load()
	if m == nil {
		return nil
	}
	out := make([]Instrument, 0, len(*m))
	for _, it := range *m {
		out = append(out, it)
	}
	return out
}

// Size — 등록 수.
func (c *Catalog) Size() int {
	m := c.p.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}
