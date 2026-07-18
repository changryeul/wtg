//go:build integration

package fxsync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/session"
	"github.com/winwaysystems/wtg/test/etcdtest"
)

// SyncUserProfiles → etcd → 실제 소비자(EtcdUserProfileResolver)로 되읽어
// login 이 볼 Site/Tier 가 그대로 반영되는지 e2e 검증. (login 코드 무변경 증명)
func TestSyncer_UserProfile_MirrorToResolver(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	ctx := context.Background()

	syncer := NewSyncer(cli, quietLogger())
	res, err := syncer.SyncUserProfiles(ctx, UserProfiles{
		{Usid: "alice01", Site: session.SiteBranch, Tier: session.TierVIP, Active: true},
		{Usid: "bob02", Site: session.SiteHQ, Tier: session.TierStandard, Active: true},
		{Usid: "gone03", Site: session.SiteBranch, Tier: session.TierGold, Active: false}, // 삭제 대상
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceCount != 3 || res.Active != 2 || res.Put != 2 {
		t.Errorf("result: %+v", res)
	}

	// 실제 login 이 쓰는 resolver 로 되읽기 (같은 prefix watch).
	r, err := auth.NewEtcdUserProfileResolver(ctx, auth.EtcdUserProfileResolverOptions{Client: cli})
	if err != nil {
		t.Fatal(err)
	}

	alice, err := r.Resolve(ctx, "alice01")
	if err != nil {
		t.Fatalf("alice01 resolve: %v", err)
	}
	if alice.Site != session.SiteBranch || alice.Tier != session.TierVIP {
		t.Errorf("alice01 = %+v, want BRANCH/VIP", alice)
	}

	bob, err := r.Resolve(ctx, "bob02")
	if err != nil {
		t.Fatalf("bob02 resolve: %v", err)
	}
	if bob.Site != session.SiteHQ || bob.Tier != session.TierStandard {
		t.Errorf("bob02 = %+v, want HQ/STD", bob)
	}

	// inactive 사용자는 미등록 (마진 fallback).
	if _, err := r.Resolve(ctx, "gone03"); err == nil {
		t.Errorf("gone03 은 inactive 라 미등록이어야 (ErrUserProfileNotFound)")
	}
}

// 등급 변경 재-sync 시 반영 + inactive 전환 시 삭제.
func TestSyncer_UserProfile_Regrade(t *testing.T) {
	e := etcdtest.Start(t)
	cli := newEtcdClient(t, e)
	ctx := context.Background()
	syncer := NewSyncer(cli, quietLogger())

	// 최초: alice = STD.
	if _, err := syncer.SyncUserProfiles(ctx, UserProfiles{
		{Usid: "alice01", Site: session.SiteBranch, Tier: session.TierStandard, Active: true},
	}); err != nil {
		t.Fatal(err)
	}
	// 승급: alice = VIP 로 재-sync.
	if _, err := syncer.SyncUserProfiles(ctx, UserProfiles{
		{Usid: "alice01", Site: session.SiteBranch, Tier: session.TierVIP, Active: true},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := cli.Get(ctx, "wtg/auth/user-profiles/alice01")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Kvs) != 1 {
		t.Fatalf("alice01 키 수 = %d", len(resp.Kvs))
	}
	var up auth.UserProfile
	if err := json.Unmarshal(resp.Kvs[0].Value, &up); err != nil {
		t.Fatal(err)
	}
	if up.Tier != session.TierVIP {
		t.Errorf("재-sync 후 tier = %q, want VIP", up.Tier)
	}
}
