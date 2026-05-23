//go:build integration

package quote

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

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

func TestEtcdSymbolWatcher_InitialLoadAndUpdate(t *testing.T) {
	cli := newEtcdClient(t)
	m := NewSymbolMap()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prefix := "test/quote/symbols/"
	// 사전 PUT.
	_, err := cli.Put(ctx, prefix+"USDKRW", `{"symbol":"USDKRW","pair":"USD/KRW","active":true}`)
	if err != nil {
		t.Fatal(err)
	}

	w, err := NewEtcdSymbolWatcher(ctx, EtcdSymbolWatcherOptions{
		Client: cli, Prefix: prefix, M: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// 초기 로드 확인.
	if pair, active, found := m.Lookup("USDKRW"); !found || !active || pair != "USD/KRW" {
		t.Errorf("초기 로드: pair=%q active=%v found=%v", pair, active, found)
	}

	// 라이브 PUT: EURKRW 추가.
	_, err = cli.Put(ctx, prefix+"EURKRW", `{"symbol":"EURKRW","pair":"EUR/KRW","active":true}`)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, found := m.Lookup("EURKRW"); found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, found := m.Lookup("EURKRW"); !found {
		t.Error("EURKRW 라이브 추가 반영 실패")
	}

	// 라이브 DELETE: USDKRW 삭제.
	_, err = cli.Delete(ctx, prefix+"USDKRW")
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, found := m.Lookup("USDKRW"); !found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, found := m.Lookup("USDKRW"); found {
		t.Error("USDKRW 라이브 삭제 반영 실패")
	}

	// 라이브 inactive 토글.
	_, err = cli.Put(ctx, prefix+"EURKRW", `{"symbol":"EURKRW","pair":"EUR/KRW","active":false}`)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, active, found := m.Lookup("EURKRW"); found && !active {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, active, found := m.Lookup("EURKRW"); !found || active {
		t.Errorf("EURKRW inactive 토글 반영 실패: active=%v found=%v", active, found)
	}
}
