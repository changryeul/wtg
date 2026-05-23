package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/winwaysystems/wtg/pkg/session"
)

func TestStaticResolver_Basic(t *testing.T) {
	r := NewStaticResolver()
	r.Replace(map[string]UserProfile{
		"trader01": {Site: session.SiteBranch, Tier: session.TierVIP},
		"trader02": {Site: session.SiteHQ, Tier: session.TierStandard},
	})

	got, err := r.Resolve(context.Background(), "trader01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != session.SiteBranch || got.Tier != session.TierVIP {
		t.Errorf("trader01: %+v", got)
	}

	if _, err := r.Resolve(context.Background(), "unknown"); !errors.Is(err, ErrUserProfileNotFound) {
		t.Errorf("미등록: err = %v, want ErrUserProfileNotFound", err)
	}
}

func TestStaticResolver_Set(t *testing.T) {
	r := NewStaticResolver()
	r.Set("trader01", UserProfile{Site: session.SiteBranch, Tier: session.TierGold})

	got, _ := r.Resolve(context.Background(), "trader01")
	if got.Tier != session.TierGold {
		t.Errorf("Set 후 Resolve: %+v", got)
	}
}

func TestStaticResolver_Replace_CopiesInput(t *testing.T) {
	r := NewStaticResolver()
	in := map[string]UserProfile{"a": {Site: session.SiteBranch, Tier: session.TierVIP}}
	r.Replace(in)
	// 외부 수정 — resolver 영향 없어야.
	in["a"] = UserProfile{Site: session.SiteHQ, Tier: session.TierStandard}
	in["b"] = UserProfile{}

	got, _ := r.Resolve(context.Background(), "a")
	if got.Site != session.SiteBranch || got.Tier != session.TierVIP {
		t.Errorf("외부 수정이 resolver 에 영향: %+v", got)
	}
	if _, err := r.Resolve(context.Background(), "b"); err == nil {
		t.Error("외부 추가 entry 가 resolver 에 반영됨")
	}
}

func TestStaticResolver_ConcurrentReadWrite(t *testing.T) {
	r := NewStaticResolver()
	r.Set("a", UserProfile{Site: session.SiteBranch, Tier: session.TierVIP})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = r.Resolve(context.Background(), "a")
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			r.Set("a", UserProfile{Site: session.SiteHQ, Tier: session.TierStandard})
		}
		close(stop)
	}()
	wg.Wait()
}

func TestLoadStaticResolverFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user-profiles.json")
	body := `[
		{"usid":"trader01","site":"BRANCH","tier":"VIP"},
		{"usid":"trader02","site":"HQ","tier":"STD"}
	]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := LoadStaticResolverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != 2 {
		t.Errorf("Size = %d, want 2", r.Size())
	}
	got, _ := r.Resolve(context.Background(), "trader02")
	if got.Site != session.SiteHQ || got.Tier != session.TierStandard {
		t.Errorf("trader02: %+v", got)
	}
}

func TestLoadStaticResolverFromFile_EmptyPath(t *testing.T) {
	r, err := LoadStaticResolverFromFile("")
	if err != nil {
		t.Errorf("empty path: %v", err)
	}
	if r.Size() != 0 {
		t.Errorf("empty path Size = %d", r.Size())
	}
}
