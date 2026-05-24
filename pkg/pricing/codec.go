package pricing

import (
	"encoding/json"
	"fmt"

	"github.com/winwaysystems/wtg/pkg/session"
)

// PricingTableDoc 은 PricingTable 의 JSON 직렬화 모양 — Map 키가 struct 라
// 직접 marshal 이 안 되므로 리스트로 평탄화한다.
//
// mci-admin 이 운영 콘솔에서 작성하는 JSON 도 이 형식 — etcd 에 그대로 저장하고
// mci-price 의 watcher 가 ParsePricingTable 으로 PricingTable 을 재구성.
type PricingTableDoc struct {
	Version    int64          `json:"version"`
	SwapPoint  []SwapEntryDoc `json:"swap_point,omitempty"`
	HQMargin   []HQEntryDoc   `json:"hq_margin,omitempty"`
	SiteMargin []SiteEntryDoc `json:"site_margin,omitempty"`
}

// SwapEntryDoc — 한 (pair, tenor) 의 스왑포인트.
type SwapEntryDoc struct {
	Pair      session.Pair `json:"pair"`
	Tenor     Tenor        `json:"tenor"`
	BidAmount float64      `json:"bid_amount"`
	AskAmount float64      `json:"ask_amount"`
}

// HQEntryDoc — 한 (pair, tier) 의 본점 마진. Tier="" 는 와일드카드.
type HQEntryDoc struct {
	Pair      session.Pair `json:"pair"`
	Tier      session.Tier `json:"tier"`
	BidAmount float64      `json:"bid_amount"`
	AskAmount float64      `json:"ask_amount"`
}

// SiteEntryDoc — 한 (pair, channel, site) 의 영업점·채널 마진.
// Channel="" 또는 Site="" 는 와일드카드 fallback.
type SiteEntryDoc struct {
	Pair      session.Pair    `json:"pair"`
	Channel   session.Channel `json:"channel"`
	Site      session.Site    `json:"site"`
	BidAmount float64         `json:"bid_amount"`
	AskAmount float64         `json:"ask_amount"`
}

// ParsePricingTable 은 JSON 바이트를 PricingTable 로 변환한다.
// 동일 키가 중복되면 뒤에 나온 entry 가 이긴다.
func ParsePricingTable(body []byte) (*PricingTable, error) {
	var doc PricingTableDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("pricing: PricingTable JSON 파싱: %w", err)
	}
	return BuildPricingTable(doc), nil
}

// BuildPricingTable 은 DTO 로부터 PricingTable 을 빌드한다.
func BuildPricingTable(doc PricingTableDoc) *PricingTable {
	t := &PricingTable{
		Version:    doc.Version,
		SwapPoint:  make(map[SwapKey]Margin, len(doc.SwapPoint)),
		HQMargin:   make(map[HQKey]Margin, len(doc.HQMargin)),
		SiteMargin: make(map[SiteKey]Margin, len(doc.SiteMargin)),
	}
	for _, e := range doc.SwapPoint {
		t.SwapPoint[SwapKey{Pair: e.Pair, Tenor: e.Tenor}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	for _, e := range doc.HQMargin {
		t.HQMargin[HQKey{Pair: e.Pair, Tier: e.Tier}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	for _, e := range doc.SiteMargin {
		t.SiteMargin[SiteKey{Pair: e.Pair, Channel: e.Channel, Site: e.Site}] =
			Margin{BidAmount: e.BidAmount, AskAmount: e.AskAmount}
	}
	return t
}
