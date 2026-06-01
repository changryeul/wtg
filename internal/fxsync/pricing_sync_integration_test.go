//go:build integration

package fxsync

import (
	"context"
	"encoding/json"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/pricing"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

func TestSyncer_Swap_RoundTrip(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	syncer := NewSyncer(cli, quietLogger())

	res, err := syncer.SyncSwapPoints(context.Background(), SwapPoints{
		{Pair: "USD/KRW", Tenor: "1M", BidAmount: 0.15, AskAmount: 0.25},
		{Pair: "USD/KRW", Tenor: "3M", BidAmount: 0.40, AskAmount: 0.55},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceCount != 2 || res.Active != 2 || res.Put != 1 {
		t.Errorf("result: %+v", res)
	}
	// etcd 의 doc 확인.
	doc := readDoc(t, cli)
	if len(doc.SwapPoint) != 2 || doc.Version != 1 {
		t.Errorf("doc: %+v", doc)
	}
}

func TestSyncer_Swap_PreservesOtherLayers(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)

	// 사전 doc — hq + swap 양쪽 있음.
	pre := pricing.PricingTableDoc{
		Version: 5,
		HQMargin: []pricing.HQEntryDoc{
			{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
		},
		SwapPoint: []pricing.SwapEntryDoc{
			{Pair: "USD/KRW", Tenor: "1M", BidAmount: 0.10, AskAmount: 0.10},
		},
		Holidays: []string{"2026-06-03"},
	}
	body, _ := json.Marshal(pre)
	_, _ = cli.Put(context.Background(), "wtg/pricing/table", string(body))

	syncer := NewSyncer(cli, quietLogger())
	// swap 만 새로 PUT.
	_, _ = syncer.SyncSwapPoints(context.Background(), SwapPoints{
		{Pair: "USD/KRW", Tenor: "3M", BidAmount: 0.50, AskAmount: 0.70},
	})

	doc := readDoc(t, cli)
	// swap 은 교체 (1개).
	if len(doc.SwapPoint) != 1 || doc.SwapPoint[0].Tenor != "3M" {
		t.Errorf("swap 교체 실패: %+v", doc.SwapPoint)
	}
	// HQ 보존.
	if len(doc.HQMargin) != 1 || doc.HQMargin[0].Tier != "VIP" {
		t.Errorf("HQ 누락: %+v", doc.HQMargin)
	}
	// holidays 보존.
	if len(doc.Holidays) != 1 || doc.Holidays[0] != "2026-06-03" {
		t.Errorf("holidays 누락: %+v", doc.Holidays)
	}
	// version 증가.
	if doc.Version != 6 {
		t.Errorf("version = %d, want 6", doc.Version)
	}
}

func TestSyncer_HQMargin_RoundTrip(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	syncer := NewSyncer(cli, quietLogger())
	_, err := syncer.SyncHQMargins(context.Background(), HQMargins{
		{Pair: "USD/KRW", Tier: "VIP", BidAmount: 0.02, AskAmount: 0.02},
		{Pair: "USD/KRW", Tier: "STD", BidAmount: 0.10, AskAmount: 0.10},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, cli)
	if len(doc.HQMargin) != 2 {
		t.Errorf("HQ count: %d", len(doc.HQMargin))
	}
}

func TestSyncer_SiteMargin_RoundTrip(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	syncer := NewSyncer(cli, quietLogger())
	_, err := syncer.SyncSiteMargins(context.Background(), SiteMargins{
		{Pair: "USD/KRW", Channel: "WEB", Site: "BRANCH", BidAmount: 0.05, AskAmount: 0.05},
		{Pair: "USD/KRW", Channel: "MOB", Site: "BRANCH", BidAmount: 0.07, AskAmount: 0.07},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, cli)
	if len(doc.SiteMargin) != 2 {
		t.Errorf("Site count: %d", len(doc.SiteMargin))
	}
}

func readDoc(t *testing.T, cli *clientv3.Client) *pricing.PricingTableDoc {
	t.Helper()
	resp, err := cli.Get(context.Background(), "wtg/pricing/table")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) == 0 {
		return &pricing.PricingTableDoc{}
	}
	var doc pricing.PricingTableDoc
	if err := json.Unmarshal(resp.Kvs[0].Value, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}
