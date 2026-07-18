//go:build integration

package fxsync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/pkg/quote"
	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

// SyncCustomerMargins → etcd pricing doc → BuildPricingTable → ApplyForCustomer.
// 고객 스프레드(override)가 tier 를 무시하고, 미등록 고객은 tier 로 fallback 하는지
// 값까지 e2e 검증. (엔진·런타임 무변경, 데이터 feed 만으로 동작한다는 증명)
func TestSyncer_CustomerMargin_OverrideAndFallback(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	ctx := context.Background()
	syncer := NewSyncer(cli, quietLogger())

	// 1. tier(VIP) HQ margin 시드 — 스프레드 미등록 고객의 fallback.
	if _, err := syncer.SyncHQMargins(ctx, HQMargins{
		{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
	}); err != nil {
		t.Fatal(err)
	}
	// 2. 고객 스프레드 sync — alice 만 override (BEST 로부터 0.15/0.20).
	res, err := syncer.SyncCustomerMargins(ctx, CustomerSpreads{
		{Usid: "alice01", Pair: "USD/KRW", BidDelta: 0.15, AskDelta: 0.20, Active: true},
		{Usid: "gone09", Pair: "USD/KRW", BidDelta: 0.99, AskDelta: 0.99, Active: false}, // 제외
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceCount != 2 || res.Active != 1 {
		t.Errorf("result: %+v (active 1 기대)", res)
	}

	// 3. etcd pricing doc → PricingTable.
	resp, err := cli.Get(ctx, "wtg/pricing/table")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) != 1 {
		t.Fatalf("pricing doc 키 = %d", len(resp.Kvs))
	}
	var doc pricing.PricingTableDoc
	if err := json.Unmarshal(resp.Kvs[0].Value, &doc); err != nil {
		t.Fatal(err)
	}
	tbl := pricing.BuildPricingTable(doc)

	best := quote.Quote{Pair: "USD/KRW", Bid: 1380.10, Ask: 1380.20, TS: time.Now()}
	profile := session.Profile{Channel: session.ChannelWeb, Site: session.SiteBranch, Tier: session.TierVIP}
	now := time.Now()

	// alice — override: bid = BEST - 0.15, ask = BEST + 0.20 (tier VIP HQ 무시).
	a := tbl.ApplyForCustomer(best, profile, pricing.TenorSpot, now, "alice01")
	if !near(a.Bid, 1380.10-0.15) || !near(a.Ask, 1380.20+0.20) {
		t.Errorf("alice override: bid=%v ask=%v, want 1379.95/1380.40", a.Bid, a.Ask)
	}

	// charlie — 스프레드 미등록 → tier(VIP) HQ 0.02 fallback.
	c := tbl.ApplyForCustomer(best, profile, pricing.TenorSpot, now, "charlie99")
	if !near(c.Bid, 1380.10-0.02) || !near(c.Ask, 1380.20+0.02) {
		t.Errorf("charlie tier fallback: bid=%v ask=%v, want 1380.08/1380.22", c.Bid, c.Ask)
	}
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
