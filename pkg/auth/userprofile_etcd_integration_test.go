//go:build integration

package auth

import (
	"context"
	"errors"
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

func TestEtcdUserProfileResolver_InitialLoadAndLiveUpdate(t *testing.T) {
	cli := newEtcdClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prefix := "test/auth/user-profiles/"

	// 사전 PUT.
	_, err := cli.Put(ctx, prefix+"trader01", `{"site":"BRANCH","tier":"VIP"}`)
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewEtcdUserProfileResolver(ctx, EtcdUserProfileResolverOptions{
		Client: cli, Prefix: prefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	got, err := r.Resolve(ctx, "trader01")
	if err != nil {
		t.Fatalf("초기 로드 Resolve: %v", err)
	}
	if got.Site != session.SiteBranch || got.Tier != session.TierVIP {
		t.Errorf("초기 로드 mismatch: %+v", got)
	}

	// 라이브 추가.
	_, _ = cli.Put(ctx, prefix+"trader02", `{"site":"HQ","tier":"STD"}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Resolve(ctx, "trader02"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err = r.Resolve(ctx, "trader02")
	if err != nil {
		t.Errorf("라이브 추가 반영 실패: %v", err)
	}
	if got.Site != session.SiteHQ {
		t.Errorf("trader02 site = %s", got.Site)
	}

	// 라이브 변경 (Tier 업그레이드).
	_, _ = cli.Put(ctx, prefix+"trader01", `{"site":"BRANCH","tier":"GOLD"}`)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = r.Resolve(ctx, "trader01")
		if got.Tier == session.TierGold {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Tier != session.TierGold {
		t.Errorf("Tier 업그레이드 반영 실패: %s", got.Tier)
	}

	// 라이브 삭제.
	_, _ = cli.Delete(ctx, prefix+"trader02")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Resolve(ctx, "trader02"); errors.Is(err, ErrUserProfileNotFound) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := r.Resolve(ctx, "trader02"); !errors.Is(err, ErrUserProfileNotFound) {
		t.Error("삭제 반영 실패")
	}
}
