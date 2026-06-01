//go:build integration

package pricing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/test/etcdtest"
)

func newCli(t *testing.T, e *etcdtest.Embedded) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints: []string{e.ClientURL}, DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func putCurrency(t *testing.T, cli *clientv3.Client, c Currency) {
	t.Helper()
	body, _ := json.Marshal(c)
	if _, err := cli.Put(context.Background(), "wtg/currency/"+c.Code, string(body)); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdCurrencyWatcher_InitialLoad(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newCli(t, e)

	// 사전 시드.
	putCurrency(t, cli, Currency{Code: "USD", Name: "달러", DecimalPlaces: 4, Active: true})
	putCurrency(t, cli, Currency{Code: "KRW", Name: "원", DecimalPlaces: 2, Active: true})

	m := NewCurrencyMaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewEtcdCurrencyWatcher(ctx, EtcdCurrencyWatcherOptions{
		Client: cli, M: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if m.Size() != 2 {
		t.Errorf("초기 로드 size = %d, want 2", m.Size())
	}
	if c, ok := m.Get("USD"); !ok || c.Name != "달러" {
		t.Errorf("Get USD = %+v ok=%v", c, ok)
	}
}

func TestEtcdCurrencyWatcher_PutTriggersUpdate(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newCli(t, e)
	m := NewCurrencyMaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, _ := NewEtcdCurrencyWatcher(ctx, EtcdCurrencyWatcherOptions{Client: cli, M: m})
	defer w.Close()

	if m.Size() != 0 {
		t.Fatalf("초기 size = %d", m.Size())
	}
	putCurrency(t, cli, Currency{Code: "JPY", Name: "엔", DecimalPlaces: 2, Active: true})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Size() != 1 {
		t.Errorf("PUT 후 size = %d, want 1", m.Size())
	}
	if c, _ := m.Get("JPY"); c.Name != "엔" {
		t.Errorf("JPY name = %q", c.Name)
	}
}

func TestEtcdCurrencyWatcher_DeleteTriggersRemoval(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newCli(t, e)
	putCurrency(t, cli, Currency{Code: "USD", Active: true})
	putCurrency(t, cli, Currency{Code: "KRW", Active: true})

	m := NewCurrencyMaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, _ := NewEtcdCurrencyWatcher(ctx, EtcdCurrencyWatcherOptions{Client: cli, M: m})
	defer w.Close()

	if m.Size() != 2 {
		t.Fatalf("초기 size = %d", m.Size())
	}
	_, _ = cli.Delete(context.Background(), "wtg/currency/KRW")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Size() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m.Size() != 1 {
		t.Errorf("DELETE 후 size = %d, want 1", m.Size())
	}
	if _, ok := m.Get("KRW"); ok {
		t.Error("KRW 삭제 안 됨")
	}
}
