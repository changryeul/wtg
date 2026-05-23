//go:build integration

package price

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

func TestEtcdProfileSource_InitialLoadAndUpdate(t *testing.T) {
	cli := newEtcdClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prefix := "test/price/profiles/"

	// 사전 PUT.
	_, err := cli.Put(ctx, prefix+"WEB.BRANCH.VIP", `{"channel":"WEB","site":"BRANCH","tier":"VIP"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli.Put(ctx, prefix+"MOB.HQ.STD", `{"channel":"MOB","site":"HQ","tier":"STD"}`)
	if err != nil {
		t.Fatal(err)
	}

	src, err := NewEtcdProfileSource(ctx, EtcdProfileSourceOptions{
		Client: cli, Prefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	got := src.ActiveProfiles()
	if len(got) != 2 {
		t.Fatalf("초기 로드 len = %d, want 2", len(got))
	}

	// 라이브 추가.
	_, err = cli.Put(ctx, prefix+"WEB.BRANCH.STD", `{"channel":"WEB","site":"BRANCH","tier":"STD"}`)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(src.ActiveProfiles()) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(src.ActiveProfiles()) != 3 {
		t.Errorf("라이브 추가 반영 실패: %d", len(src.ActiveProfiles()))
	}

	// 라이브 삭제.
	_, err = cli.Delete(ctx, prefix+"MOB.HQ.STD")
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(src.ActiveProfiles()) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(src.ActiveProfiles()) != 2 {
		t.Errorf("라이브 삭제 반영 실패: %d", len(src.ActiveProfiles()))
	}
	for _, p := range src.ActiveProfiles() {
		if p.Channel == session.ChannelMobile {
			t.Error("삭제된 MOB profile 이 잔존")
		}
	}
}
