//go:build integration

package pricing

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/test/etcdtest"
)

func putPair(t *testing.T, cli *clientv3.Client, p Pair) {
	t.Helper()
	body, _ := json.Marshal(p)
	if _, err := cli.Put(context.Background(), "wtg/pair/"+p.ID, string(body)); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdPairWatcher_InitialLoad(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newCli(t, e)
	putPair(t, cli, Pair{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Symbol: "USDKRW", Active: true})
	putPair(t, cli, Pair{ID: "EURKRW", Base: "EUR", Quote: "KRW", Kind: "cross", Active: true,
		Cross: &Cross{LegA: "EUR/USD", OpA: "mul", LegB: "USD/KRW", OpB: "mul", Scale: 1}})

	m := NewPairMaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewEtcdPairWatcher(ctx, EtcdPairWatcherOptions{Client: cli, M: m})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if m.Size() != 2 {
		t.Errorf("initial size = %d", m.Size())
	}
	if f := m.CrossFormulas(); len(f) != 1 {
		t.Errorf("cross formulas = %d, want 1", len(f))
	}
}

func TestEtcdPairWatcher_OnChange(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newCli(t, e)
	m := NewPairMaster()
	var changes atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, _ := NewEtcdPairWatcher(ctx, EtcdPairWatcherOptions{
		Client: cli, M: m,
		OnChange: func(_ *PairMaster) { changes.Add(1) },
	})
	defer w.Close()
	// initial load 도 OnChange 호출.
	if changes.Load() < 1 {
		t.Errorf("initial OnChange 호출 안 됨: %d", changes.Load())
	}
	before := changes.Load()
	putPair(t, cli, Pair{ID: "USDKRW", Base: "USD", Quote: "KRW", Kind: "direct", Active: true})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if changes.Load() > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if changes.Load() <= before {
		t.Errorf("PUT 후 OnChange 호출 안 됨")
	}
	if _, ok := m.Get("USDKRW"); !ok {
		t.Error("PUT 후 master 에 없음")
	}
}
