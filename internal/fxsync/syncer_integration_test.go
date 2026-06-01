//go:build integration

package fxsync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/winwaysystems/wtg/test/etcdtest"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newEtcdClient(t *testing.T, e *etcdtest.Embedded) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{e.ClientURL},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestSyncer_Currency_PutAndRead(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)

	syncer := NewSyncer(cli, quietLogger())
	res, err := syncer.SyncCurrencies(context.Background(), Currencies{
		{Code: "USD", Name: "미국 달러", DecimalPlaces: 4, Active: true},
		{Code: "KRW", Name: "한국 원", DecimalPlaces: 2, Active: true},
		{Code: "HKD", Name: "홍콩 달러", Active: false}, // inactive — PUT 안 됨
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceCount != 3 || res.Active != 2 || res.Put != 2 || res.DeletedStale != 0 {
		t.Errorf("result: %+v", res)
	}

	// etcd 에서 확인.
	resp, err := cli.Get(context.Background(), "wtg/currency/", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) != 2 {
		t.Fatalf("etcd 키 수 = %d, want 2", len(resp.Kvs))
	}
	found := map[string]Currency{}
	for _, kv := range resp.Kvs {
		code := strings.TrimPrefix(string(kv.Key), "wtg/currency/")
		var c Currency
		if err := json.Unmarshal(kv.Value, &c); err != nil {
			t.Fatal(err)
		}
		found[code] = c
	}
	if found["USD"].DecimalPlaces != 4 || found["KRW"].DecimalPlaces != 2 {
		t.Errorf("decoded: %+v", found)
	}
	if _, ok := found["HKD"]; ok {
		t.Errorf("HKD 가 inactive 인데 etcd 에 있음")
	}
}

func TestSyncer_Currency_DeleteStale(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	syncer := NewSyncer(cli, quietLogger())

	// 1차 sync — USD + KRW.
	_, err := syncer.SyncCurrencies(context.Background(), Currencies{
		{Code: "USD", Active: true},
		{Code: "KRW", Active: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2차 sync — KRW 만 (USD 가 DB 에서 사라짐).
	res, err := syncer.SyncCurrencies(context.Background(), Currencies{
		{Code: "KRW", Active: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.DeletedStale != 1 {
		t.Errorf("stale 정리 = %d, want 1", res.DeletedStale)
	}

	// 확인 — KRW 만 남음.
	resp, _ := cli.Get(context.Background(), "wtg/currency/", clientv3.WithPrefix())
	if len(resp.Kvs) != 1 || !strings.HasSuffix(string(resp.Kvs[0].Key), "/KRW") {
		t.Errorf("kvs after stale delete: %+v", resp.Kvs)
	}
}

func TestSyncer_Currency_DeleteStaleDisabled(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	syncer := NewSyncer(cli, quietLogger())
	syncer.DeleteStale = false

	_, _ = syncer.SyncCurrencies(context.Background(), Currencies{
		{Code: "USD", Active: true}, {Code: "KRW", Active: true},
	})
	// USD 없이 다시 sync — DeleteStale=false 이므로 USD 도 살아있음.
	_, _ = syncer.SyncCurrencies(context.Background(), Currencies{
		{Code: "KRW", Active: true},
	})
	resp, _ := cli.Get(context.Background(), "wtg/currency/", clientv3.WithPrefix())
	if len(resp.Kvs) != 2 {
		t.Errorf("DeleteStale=false 인데 stale 정리됨: %d", len(resp.Kvs))
	}
}

func TestFileBackend_EndToEnd_SyncIntoEtcd(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)

	// repo 의 db-mirror sample 사용.
	b := NewFileBackend("../../etc/db-mirror")
	cs, err := b.LoadCurrencies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	syncer := NewSyncer(cli, quietLogger())
	res, err := syncer.SyncCurrencies(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceCount == 0 {
		t.Fatal("sample currency.json 비어있음 또는 미발견")
	}
	if res.Put != res.Active {
		t.Errorf("active != put: %+v", res)
	}
}
