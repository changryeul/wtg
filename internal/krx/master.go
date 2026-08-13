package krx

import (
	"sync"

	wire "github.com/winwaysystems/wtg/pkg/krx"
)

// MasterCache 는 종목 마스터(A006F/A001B)를 code 로 캐시한다. 체결(A306F/A301K)이
// 도착하면 마스터의 전일종가로 등락(diff/rate)을 채우는 enrichment 에 쓰인다.
// 마스터는 저빈도(장 초 1회 + 갱신)라 map + RWMutex 로 충분.
type MasterCache struct {
	mu     sync.RWMutex
	fut    map[string]*wire.FutMaster
	bond   map[string]*wire.BondMaster
	settle map[string]*wire.FutSettle // H306F 정산가 (체결 enrich 용)
}

// NewMasterCache — 빈 캐시.
func NewMasterCache() *MasterCache {
	return &MasterCache{
		fut:    map[string]*wire.FutMaster{},
		bond:   map[string]*wire.BondMaster{},
		settle: map[string]*wire.FutSettle{},
	}
}

// PutFut / GetFut — 파생 마스터.
func (c *MasterCache) PutFut(m *wire.FutMaster) {
	if m == nil || m.Code == "" {
		return
	}
	c.mu.Lock()
	c.fut[m.Code] = m
	c.mu.Unlock()
}
func (c *MasterCache) GetFut(code string) *wire.FutMaster {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fut[code]
}

// PutBond / GetBond — 채권 마스터.
func (c *MasterCache) PutBond(m *wire.BondMaster) {
	if m == nil || m.Code == "" {
		return
	}
	c.mu.Lock()
	c.bond[m.Code] = m
	c.mu.Unlock()
}
func (c *MasterCache) GetBond(code string) *wire.BondMaster {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bond[code]
}

// PutSettle / GetSettle — H306F 정산가.
func (c *MasterCache) PutSettle(s *wire.FutSettle) {
	if s == nil || s.Code == "" {
		return
	}
	c.mu.Lock()
	c.settle[s.Code] = s
	c.mu.Unlock()
}
func (c *MasterCache) GetSettle(code string) *wire.FutSettle {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.settle[code]
}

// enrichFutTrade 는 체결(A306F 등)에 마스터의 전일종가/기준가로 등락(전일대비)을 채운다.
// 정답지는 C 피드 fut_real.c 의 set_fsise_diff — 그 수식/가드를 그대로 옮긴다:
//   - 기준값 yPrc = 전일종가(yprc). yprc<=0 이면 기준가(bprc)로 대체.
//   - yPrc<=0 || ePrc(체결가)<=0 || 보합(yPrc==ePrc) → diff=0, rate=0, 부호 보합(' ').
//   - 그 외 diff = ePrc-yPrc, rate = diff/yPrc*100, 부호는 rate 방향.
//
// 전송하는 prevClose 필드는 (C 와 동일하게) 대체 전 원 전일종가를 그대로 싣는다.
// 마스터 미도착(nil)이면 그대로 (diff/rate 0) — 마스터 도착 후 후속 체결부터 채워짐.
func enrichFutTrade(ft *wire.FutTrade, m *wire.FutMaster) {
	if ft == nil || m == nil {
		return
	}
	ft.PrevClose = m.PrevClose
	if ft.BasePrc == 0 {
		ft.BasePrc = m.BasePrc
	}

	yPrc := m.PrevClose
	if yPrc <= 0 { // 전일종가 없으면 기준가로 대체 (신규상장/구분코드 등)
		yPrc = m.BasePrc
	}
	ePrc := ft.Last
	if yPrc <= 0 || ePrc <= 0 || yPrc == ePrc {
		ft.Diff = 0
		ft.Rate = 0
		ft.Sign = " "
		return
	}
	ft.Diff = ePrc - yPrc
	ft.Rate = ft.Diff / yPrc * 100.0
	ft.Sign = dirSign(ft.Diff)
}

// applyFutSettle 는 체결에 캐시된 정산가(H306F)를 실어준다 — C 가 매 KA push 에
// fsise.sPrc/lsPr/sPcd 를 싣는 것과 동일. 정산가 미수신(nil)이면 0 유지.
func applyFutSettle(ft *wire.FutTrade, s *wire.FutSettle) {
	if ft == nil || s == nil {
		return
	}
	ft.Settle = s.Settle
	ft.FinalSettle = s.FinalSettle
	ft.SettleCd = s.SettleCd
}

// dirSign — 등락 방향부호 (+ 상승 / - 하락 / ' ' 보합).
func dirSign(diff float64) string {
	switch {
	case diff > 0:
		return "+"
	case diff < 0:
		return "-"
	default:
		return " "
	}
}
