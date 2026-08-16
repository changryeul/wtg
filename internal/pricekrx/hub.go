// Package pricekrx 는 mci-price-krx 의 코어 — KRX 원 TR 을 파싱해 종목 상태를 유지하고
// /dev/shm/mfsise(MFSISE_T)에 적재한다 (C sise 피드 흡수, 트랙2). yuanta trn AP 는
// 기존 libmfsise(l_mfread)로 무수정 read.
package pricekrx

import (
	"sync"

	wire "github.com/winwaysystems/wtg/pkg/krx"
	"github.com/winwaysystems/wtg/pkg/krxshm"
)

type mrec struct {
	short      string
	bprc, yprc float64
}

// Hub — 마스터/시세 상태 + SHM writer. Ingest 는 원 TR 5바이트 코드로 판별.
type Hub struct {
	mu        sync.Mutex
	w         *krxshm.Writer
	masters   map[string]mrec
	quotes    map[string]krxshm.Quote
	settle    map[string]wire.FutSettle
	laidCount int // 마지막 Layout 시점의 마스터 수 (증가하면 re-Layout)
}

// New — SHM writer(mmap 또는 테스트 버퍼)로 Hub 생성.
func New(w *krxshm.Writer) *Hub {
	return &Hub{w: w, masters: map[string]mrec{}, quotes: map[string]krxshm.Quote{}, settle: map[string]wire.FutSettle{}}
}

// Ingest — 원 TR 1건 처리 후 SHM 반영. 반환: (code, 반영여부).
func (h *Hub) Ingest(b []byte) (string, bool, error) {
	if len(b) < 5 {
		return "", false, nil
	}
	switch string(b[0:5]) {
	case "A006F":
		m, err := wire.DecodeA006F(b)
		if err != nil {
			return "", false, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.masters[m.Code] = mrec{short: m.ShortCode, bprc: m.BasePrc, yprc: m.PrevClose}
		h.syncLocked(m.Code)
		return m.Code, true, nil
	case "H306F":
		s, err := wire.DecodeH306F(b)
		if err != nil {
			return "", false, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.settle[s.Code] = *s
		if q, ok := h.quotes[s.Code]; ok {
			q.Settle, q.FinalSettle, q.SettleCd = s.Settle, s.FinalSettle, s.SettleCd
			h.quotes[s.Code] = q
			h.syncLocked(s.Code)
		}
		return s.Code, true, nil
	case "A306F":
		ft, err := wire.DecodeA306F(b)
		if err != nil {
			return "", false, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		q := h.buildQuote(ft)
		h.quotes[ft.Code] = q
		h.syncLocked(ft.Code)
		return ft.Code, true, nil
	}
	return "", false, nil
}

// buildQuote — 체결(A306F) + 캐시된 마스터/정산 → SHM Quote (전일대비 계산).
// 전일대비 기준 yPrc = 전일종가, ≤0 이면 기준가(bprc) — C set_fsise_diff 동형.
func (h *Hub) buildQuote(ft *wire.FutTrade) krxshm.Quote {
	q := krxshm.Quote{
		Code: ft.Code, Last: ft.Last, Open: ft.Open, High: ft.High, Low: ft.Low,
	}
	if m, ok := h.masters[ft.Code]; ok {
		q.ShortCode, q.BasePrc, q.PrevClose = m.short, m.bprc, m.yprc
		ref := m.yprc
		if ref <= 0 {
			ref = m.bprc
		}
		q.Diff, q.Rate, q.Sign = wire.PriceDiff(ft.Last, ref)
	}
	if s, ok := h.settle[ft.Code]; ok {
		q.Settle, q.FinalSettle, q.SettleCd = s.Settle, s.FinalSettle, s.SettleCd
	}
	return q
}

// syncLocked — 필요 시 re-Layout(마스터 증가) 후 해당 종목 SHM Update. mu 보유 상태 호출.
func (h *Hub) syncLocked(code string) {
	if h.w == nil {
		return
	}
	if len(h.masters) != h.laidCount {
		codes := make(map[string]string, len(h.masters))
		for c, m := range h.masters {
			codes[c] = m.short
		}
		if err := h.w.Layout(codes); err != nil {
			return // 초과 등 — skip (상위서 로깅 가능)
		}
		h.laidCount = len(h.masters)
		for c, q := range h.quotes { // slot 재배치 후 전량 재기록
			if h.w.Has(c) {
				_ = h.w.Update(q)
			}
		}
		return
	}
	if q, ok := h.quotes[code]; ok && h.w.Has(code) {
		_ = h.w.Update(q)
	}
}

// Stats — 종목/시세 수 (진단).
func (h *Hub) Stats() (masters, quotes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.masters), len(h.quotes)
}
