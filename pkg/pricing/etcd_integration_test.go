//go:build integration

package pricing

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

func newEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	srv := etcdtest.Start(t)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{srv.ClientURL},
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestEtcdTableWatcher_InitialLoadEmpty(t *testing.T) {
	cli := newEtcdClient(t)
	store := NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w, err := NewEtcdTableWatcher(ctx, EtcdTableWatcherOptions{
		Client: cli,
		Key:    "test/pricing/table",
		Store:  store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	tbl := store.Load()
	if tbl == nil || tbl.Version != 0 {
		t.Errorf("초기 빈 PricingTable 기대: %+v", tbl)
	}
}

func TestEtcdTableWatcher_InitialLoadExisting(t *testing.T) {
	cli := newEtcdClient(t)
	store := NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 사전 PUT.
	_, err := cli.Put(ctx, "test/pricing/table", `{
		"version": 7,
		"hq_margin": [{"pair":"USD/KRW","tier":"VIP","bid_amount":0.02,"ask_amount":0.02}]
	}`)
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdTableWatcher(ctx, EtcdTableWatcherOptions{
		Client: cli, Key: "test/pricing/table", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	tbl := store.Load()
	if tbl.Version != 7 {
		t.Errorf("Version = %d, want 7", tbl.Version)
	}
	if m := tbl.lookupHQ("USD/KRW", session.TierVIP, nil); m.BidAmount != 0.02 {
		t.Errorf("HQ VIP bid = %v, want 0.02", m.BidAmount)
	}
}

func TestEtcdTableWatcher_LiveUpdate(t *testing.T) {
	cli := newEtcdClient(t)
	store := NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, err := NewEtcdTableWatcher(ctx, EtcdTableWatcherOptions{
		Client: cli, Key: "test/pricing/table", Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 워처 시작 이후 PUT — 즉시 반영되어야.
	_, err = cli.Put(ctx, "test/pricing/table", `{"version": 99}`)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Load().Version == 99 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Watch 갱신 실패: version=%d, want 99", store.Load().Version)
}
