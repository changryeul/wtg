package krx

import (
	"sync"

	wire "github.com/winwaysystems/wtg/pkg/krx"
)

// MasterCache 는 종목 마스터(A006F/A001B)를 code 로 캐시한다. 체결(A306F/A301K)이
// 도착하면 마스터의 전일종가로 등락(diff/rate)을 채우는 enrichment 에 쓰인다.
// 마스터는 저빈도(장 초 1회 + 갱신)라 map + RWMutex 로 충분.
type MasterCache struct {
	mu   sync.RWMutex
	fut  map[string]*wire.FutMaster
	bond map[string]*wire.BondMaster
}

// NewMasterCache — 빈 캐시.
func NewMasterCache() *MasterCache {
	return &MasterCache{
		fut:  map[string]*wire.FutMaster{},
		bond: map[string]*wire.BondMaster{},
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

// enrichFutTrade 는 체결(A306F 등)에 마스터 전일종가로 등락/기준가를 채운다.
// 마스터 미도착(nil)이면 그대로 (diff/rate 0) — 마스터 도착 후 후속 체결부터 채워짐.
func enrichFutTrade(ft *wire.FutTrade, m *wire.FutMaster) {
	if ft == nil || m == nil {
		return
	}
	ft.PrevClose = m.PrevClose
	if ft.BasePrc == 0 {
		ft.BasePrc = m.BasePrc
	}
	if m.PrevClose != 0 {
		ft.Diff = ft.Last - m.PrevClose
		ft.Rate = ft.Diff / m.PrevClose * 100.0
	}
	ft.Sign = dirSign(ft.Diff)
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
