package fxsync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/session"
)

// GradeMapper — 고객 DB 원시 등급/조직 코드 → WTG enum 변환 (Oracle backend seam).

func TestGradeMapper_MapTier(t *testing.T) {
	m := GradeMapper{
		TierByGrade: map[string]session.Tier{
			"01": session.TierVIP,
			"02": session.TierGold,
			"03": session.TierStandard,
		},
		TierFallback: session.TierStandard,
	}
	cases := map[string]session.Tier{
		"01": session.TierVIP,
		"02": session.TierGold,
		"03": session.TierStandard,
		"99": session.TierStandard, // 미등록 → fallback
		"":   session.TierStandard,
	}
	for in, want := range cases {
		if got := m.MapTier(in); got != want {
			t.Errorf("MapTier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGradeMapper_MapSite(t *testing.T) {
	m := GradeMapper{
		SiteByCode:   map[string]session.Site{"B": session.SiteBranch, "H": session.SiteHQ},
		SiteFallback: session.SiteBranch,
	}
	if m.MapSite("H") != session.SiteHQ {
		t.Errorf("MapSite(H) != HQ")
	}
	if m.MapSite("X") != session.SiteBranch { // 미등록 → fallback
		t.Errorf("MapSite(X) fallback 아님")
	}
}

// fallback 미지정 시 기본값(STD/BRANCH).
func TestGradeMapper_DefaultFallback(t *testing.T) {
	var m GradeMapper // 전부 nil/빈값
	if m.MapTier("anything") != session.TierStandard {
		t.Errorf("기본 tier fallback 이 STD 아님")
	}
	if m.MapSite("anything") != session.SiteBranch {
		t.Errorf("기본 site fallback 이 BRANCH 아님")
	}
}

// FileBackend.LoadUserProfiles — user_profile.json (enum 형태) 로드.
func TestFileBackend_LoadUserProfiles(t *testing.T) {
	dir := t.TempDir()
	js := `[
	  {"usid":"alice01","site":"BRANCH","tier":"VIP","active":true},
	  {"usid":"bob02","site":"HQ","tier":"STD","active":true},
	  {"usid":"gone03","site":"BRANCH","tier":"GOLD","active":false}
	]`
	if err := os.WriteFile(filepath.Join(dir, "user_profile.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewFileBackend(dir)
	ups, err := b.LoadUserProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 3 {
		t.Fatalf("len = %d, want 3", len(ups))
	}
	if ups[0].Usid != "alice01" || ups[0].Site != session.SiteBranch || ups[0].Tier != session.TierVIP {
		t.Errorf("alice01 매핑: %+v", ups[0])
	}
	if ups[2].Active { // inactive 도 반환 (sync 가 삭제 mark 로 사용)
		t.Errorf("gone03 은 active=false 여야")
	}
}

// 파일 없으면 빈 슬라이스 (sync 가 그 항목 무시).
func TestFileBackend_LoadUserProfiles_Missing(t *testing.T) {
	b := NewFileBackend(t.TempDir())
	ups, err := b.LoadUserProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 0 {
		t.Errorf("누락 파일인데 len = %d", len(ups))
	}
}

// 핵심: 미러가 쓰는 payload 가 EtcdUserProfileResolver 가 읽는 auth.UserProfile
// 스키마와 정확히 호환되어야 한다 (site/tier JSON round-trip).
func TestUserProfile_ResolverSchemaCompat(t *testing.T) {
	up := UserProfile{Usid: "alice01", Site: session.SiteBranch, Tier: session.TierVIP, Active: true}
	// 미러가 etcd 에 쓰는 값 = auth.UserProfile{Site,Tier}.
	payload := auth.UserProfile{Site: up.Site, Tier: up.Tier}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	// EtcdUserProfileResolver 가 Unmarshal 하는 타입으로 되읽기.
	var back auth.UserProfile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("resolver 스키마로 unmarshal 실패: %v (%s)", err, raw)
	}
	if back.Site != session.SiteBranch || back.Tier != session.TierVIP {
		t.Errorf("round-trip 불일치: %+v (%s)", back, raw)
	}
}
