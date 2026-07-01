package md

import (
	"fmt"
	"strings"
	"sync"

	"github.com/quickfixgo/enum"
	"github.com/quickfixgo/fix44/marketdatarequest"
	"github.com/quickfixgo/fix44/marketdatasnapshotfullrefresh"
	"github.com/shopspring/decimal"
)

// ParsedMDR — MDR (35=V) 파싱 결과. Phase A 는 필수 필드만 뽑음.
//
// Phase B 에서 MDEntryType(bid/offer/trade) 리스트 + MarketDepth 도 활용.
type ParsedMDR struct {
	MDReqID string
	// SubReqType — 0=SNAPSHOT / 1=SNAPSHOT+UPDATES / 2=UNSUBSCRIBE (unused Phase A).
	SubReqType enum.SubscriptionRequestType
	// Symbols — NoRelatedSym 그룹의 심볼 리스트 (tag 55).
	Symbols []string
}

// ParseMDR — quickfix.Message → ParsedMDR. 필수 필드 없으면 error.
func ParseMDR(mdr marketdatarequest.MarketDataRequest) (ParsedMDR, error) {
	out := ParsedMDR{}
	id, err := mdr.GetMDReqID()
	if err != nil {
		return out, fmt.Errorf("MDReqID(262) 필요")
	}
	out.MDReqID = id

	srt, err := mdr.GetSubscriptionRequestType()
	if err != nil {
		return out, fmt.Errorf("SubscriptionRequestType(263) 필요")
	}
	out.SubReqType = srt

	// NoRelatedSym(146) → repeating group of Symbol(55).
	grp, err := mdr.GetNoRelatedSym()
	if err != nil {
		return out, fmt.Errorf("NoRelatedSym(146) 필요")
	}
	n := grp.Len()
	if n == 0 {
		return out, fmt.Errorf("NoRelatedSym 그룹 비어있음")
	}
	for i := 0; i < n; i++ {
		row := grp.Get(i)
		sym, err := row.GetSymbol()
		if err != nil || strings.TrimSpace(sym) == "" {
			return out, fmt.Errorf("NoRelatedSym[%d] Symbol(55) 누락", i)
		}
		out.Symbols = append(out.Symbols, sym)
	}
	return out, nil
}

// StaticQuote — 심볼 하나에 대한 정지 quote. Phase A 하드코딩 provider.
// Phase B 에서 mci-price gRPC SubscribeQuote 로 교체.
type StaticQuote struct {
	Bid   float64
	Ask   float64
	Scale int32 // price scale (예: 1378.55 → 2)
	Size  float64
}

// StaticQuoteProvider — 심볼 → StaticQuote. thread-safe read.
type StaticQuoteProvider struct {
	mu sync.RWMutex
	m  map[string]StaticQuote
}

// DefaultStaticProvider — Phase A 데모용 4-symbol seed.
func DefaultStaticProvider() *StaticQuoteProvider {
	return &StaticQuoteProvider{m: map[string]StaticQuote{
		"USD/KRW": {Bid: 1378.55, Ask: 1378.60, Scale: 2, Size: 1_000_000},
		"EUR/USD": {Bid: 1.0850, Ask: 1.0852, Scale: 4, Size: 1_000_000},
		"USD/JPY": {Bid: 155.20, Ask: 155.23, Scale: 2, Size: 1_000_000},
		"GBP/USD": {Bid: 1.2650, Ask: 1.2652, Scale: 4, Size: 1_000_000},
	}}
}

func (p *StaticQuoteProvider) Get(sym string) (StaticQuote, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	q, ok := p.m[sym]
	return q, ok
}

func (p *StaticQuoteProvider) Set(sym string, q StaticQuote) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[sym] = q
}

// BuildSnapshot — 하나의 symbol 에 대한 MarketDataSnapshotFullRefresh (35=W) 조립.
// NoMDEntries 는 bid(269=0) + offer(269=1) 두 개.
func BuildSnapshot(mdReqID, symbol string, q StaticQuote) marketdatasnapshotfullrefresh.MarketDataSnapshotFullRefresh {
	msg := marketdatasnapshotfullrefresh.New()
	msg.SetMDReqID(mdReqID)
	msg.SetSymbol(symbol)

	entries := marketdatasnapshotfullrefresh.NewNoMDEntriesRepeatingGroup()
	bid := entries.Add()
	bid.SetMDEntryType(enum.MDEntryType_BID)
	bid.SetMDEntryPx(decimal.NewFromFloat(q.Bid), q.Scale)
	bid.SetMDEntrySize(decimal.NewFromFloat(q.Size), 0)

	ask := entries.Add()
	ask.SetMDEntryType(enum.MDEntryType_OFFER)
	ask.SetMDEntryPx(decimal.NewFromFloat(q.Ask), q.Scale)
	ask.SetMDEntrySize(decimal.NewFromFloat(q.Size), 0)

	msg.SetNoMDEntries(entries)
	return msg
}
