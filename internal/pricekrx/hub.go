// Package pricekrx 는 mci-price-krx 의 코어 — KRX 원 TR 을 파싱해 종목 상태를 유지하고
// 파생 SHM(/dev/shm/mfsise, MFSISE_T) + 채권 SHM(/dev/shm/mbsise, MBSISE_T)에 적재한다
// (C sise 피드 흡수, 트랙2). yuanta trn AP 는 기존 libmfsise/libmbsise(l_mfread) 무수정 read.
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
type bmrec struct {
	name string
	bprc float64
}

// Hub — 파생/채권 마스터·시세 상태 + SHM writer 2종. Ingest 는 원 TR 5바이트 코드 판별.
type Hub struct {
	mu sync.Mutex
	// 파생 (mfsise)
	w         *krxshm.Writer
	masters   map[string]mrec
	quotes    map[string]krxshm.Quote
	settle    map[string]wire.FutSettle
	laidCount int
	// 채권 (mbsise)
	bw         *krxshm.BondWriter
	bmasters   map[string]bmrec
	bquotes    map[string]krxshm.BondQuote
	bLaidCount int
}

// New — 파생/채권 SHM writer(nil 가능; nil 이면 해당 시장 skip)로 Hub 생성.
func New(w *krxshm.Writer, bw *krxshm.BondWriter) *Hub {
	return &Hub{
		w: w, masters: map[string]mrec{}, quotes: map[string]krxshm.Quote{}, settle: map[string]wire.FutSettle{},
		bw: bw, bmasters: map[string]bmrec{}, bquotes: map[string]krxshm.BondQuote{},
	}
}

// Ingest — 원 TR 1건 처리 후 SHM 반영. 반환: (code, 반영여부, err).
func (h *Hub) Ingest(b []byte) (string, bool, error) {
	if len(b) < 5 {
		return "", false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch string(b[0:5]) {
	// ---- 파생 (mfsise) ----
	case "A006F":
		m, err := wire.DecodeA006F(b)
		if err != nil {
			return "", false, err
		}
		h.masters[m.Code] = mrec{short: m.ShortCode, bprc: m.BasePrc, yprc: m.PrevClose}
		h.syncFut(m.Code)
		return m.Code, true, nil
	case "A306F":
		ft, err := wire.DecodeA306F(b)
		if err != nil {
			return "", false, err
		}
		h.quotes[ft.Code] = h.buildFutQuote(ft)
		h.syncFut(ft.Code)
		return ft.Code, true, nil
	case "H306F":
		s, err := wire.DecodeH306F(b)
		if err != nil {
			return "", false, err
		}
		h.settle[s.Code] = *s
		if q, ok := h.quotes[s.Code]; ok {
			q.Settle, q.FinalSettle, q.SettleCd = s.Settle, s.FinalSettle, s.SettleCd
			h.quotes[s.Code] = q
			h.syncFut(s.Code)
		}
		return s.Code, true, nil
	case "B606F":
		fb, err := wire.DecodeB606F(b)
		if err != nil {
			return "", false, err
		}
		if h.w != nil && h.w.Has(fb.Code) {
			_ = h.w.UpdateBook(futBook(fb))
		}
		return fb.Code, true, nil
	// ---- 채권 (mbsise) ----
	case "A001B":
		m, err := wire.DecodeA001B(b)
		if err != nil {
			return "", false, err
		}
		h.bmasters[m.Code] = bmrec{name: m.Name, bprc: m.BasePrc}
		h.syncBond(m.Code)
		return m.Code, true, nil
	case "A301K":
		bt, err := wire.DecodeA301K(b)
		if err != nil {
			return "", false, err
		}
		h.bquotes[bt.Code] = h.buildBondQuote(bt)
		h.syncBond(bt.Code)
		return bt.Code, true, nil
	case "B601K":
		bb, err := wire.DecodeB601K(b)
		if err != nil {
			return "", false, err
		}
		if h.bw != nil && h.bw.Has(bb.Code) {
			_ = h.bw.UpdateBook(bondBook(bb))
		}
		return bb.Code, true, nil
	}
	return "", false, nil
}

// buildFutQuote — 체결(A306F)+마스터+정산 → FSISE_T Quote (전일대비: yPrc, ≤0 시 bPrc).
func (h *Hub) buildFutQuote(ft *wire.FutTrade) krxshm.Quote {
	q := krxshm.Quote{Code: ft.Code, Last: ft.Last, Open: ft.Open, High: ft.High, Low: ft.Low}
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

// buildBondQuote — 채권 체결(A301K)+마스터 → BSISE_T (전일대비: 기준가 bprc 대비).
func (h *Hub) buildBondQuote(bt *wire.BondTrade) krxshm.BondQuote {
	q := krxshm.BondQuote{
		Code: bt.Code, Last: bt.Last, Yield: bt.Yield,
		OYield: bt.OYield, HYield: bt.HYield, LYield: bt.LYield,
	}
	if m, ok := h.bmasters[bt.Code]; ok {
		q.BasePrc, q.PrevClose = m.bprc, m.bprc // 채권 전일종가 TR 없음 → 기준가 사용
		q.Diff, q.Rate, q.Sign = wire.PriceDiff(bt.Last, m.bprc)
	}
	return q
}

// syncFut — 파생 SHM: 마스터 증가 시 re-Layout(전량 재기록) 후 해당 종목 Update.
func (h *Hub) syncFut(code string) {
	if h.w == nil {
		return
	}
	if len(h.masters) != h.laidCount {
		codes := make(map[string]string, len(h.masters))
		for c, m := range h.masters {
			codes[c] = m.short
		}
		if h.w.Layout(codes) != nil {
			return
		}
		h.laidCount = len(h.masters)
		for c, q := range h.quotes {
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

// syncBond — 채권 SHM: 동일 패턴.
func (h *Hub) syncBond(code string) {
	if h.bw == nil {
		return
	}
	if len(h.bmasters) != h.bLaidCount {
		codes := make(map[string]string, len(h.bmasters))
		for c, m := range h.bmasters {
			codes[c] = m.name
		}
		if h.bw.Layout(codes) != nil {
			return
		}
		h.bLaidCount = len(h.bmasters)
		for c, q := range h.bquotes {
			if h.bw.Has(c) {
				_ = h.bw.UpdateTrade(q)
			}
		}
		return
	}
	if q, ok := h.bquotes[code]; ok && h.bw.Has(code) {
		_ = h.bw.UpdateTrade(q)
	}
}

// futBook — wire.FutBook → krxshm.Book.
func futBook(fb *wire.FutBook) krxshm.Book {
	b := krxshm.Book{
		Code: fb.Code, AskTot: u64(fb.AskTot), BidTot: u64(fb.BidTot),
		AskCnt: u64(fb.AskCnt), BidCnt: u64(fb.BidCnt), ExpPrc: fb.ExpPrc, ExpVol: u64(fb.ExpVol),
	}
	for _, l := range fb.Ask {
		b.Ask = append(b.Ask, krxshm.Level{Prc: l.Prc, Vol: u64(l.Vol), Cnt: u64(l.Cnt)})
	}
	for _, l := range fb.Bid {
		b.Bid = append(b.Bid, krxshm.Level{Prc: l.Prc, Vol: u64(l.Vol), Cnt: u64(l.Cnt)})
	}
	return b
}

// bondBook — wire.BondBook → krxshm.BondBook.
func bondBook(bb *wire.BondBook) krxshm.BondBook {
	b := krxshm.BondBook{Code: bb.Code, AskTot: u64(bb.AskTot), BidTot: u64(bb.BidTot)}
	for _, l := range bb.Ask {
		b.Ask = append(b.Ask, krxshm.BondLevel{Prc: l.Prc, Vol: u64(l.Vol), Yld: l.Yld})
	}
	for _, l := range bb.Bid {
		b.Bid = append(b.Bid, krxshm.BondLevel{Prc: l.Prc, Vol: u64(l.Vol), Yld: l.Yld})
	}
	return b
}

func u64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// Stats — 파생/채권 마스터·시세 수.
func (h *Hub) Stats() (fm, fq, bm, bq int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.masters), len(h.quotes), len(h.bmasters), len(h.bquotes)
}
