package fxsync

import "github.com/winwaysystems/wtg/pkg/session"

// UserProfile — 고객 등급 → 시세 Profile(Site/Tier) 매핑 1건.
//
// 흐름: 고객 DB 등급 → fx-sync 미러 → etcd wtg/auth/user-profiles/{usid} →
// mci-api login 의 EtcdUserProfileResolver 가 읽어 JWT.site/tier 에 박음.
// (login 코드 무변경 — resolver 가 이미 그 키를 watch)
//
// DB column 매핑 (향후 Oracle backend — 고객 마스터 테이블):
//
//	Usid   ← 고객 로그인 ID
//	Site   ← 영업점/본점 구분 코드   (GradeMapper.MapSite 로 enum 화)
//	Tier   ← 고객 등급 코드          (GradeMapper.MapTier 로 enum 화)
//	Active ← 사용여부 (Y/N)
//
// 미러가 etcd 에 쓰는 값은 auth.UserProfile{site,tier} (resolver 스키마) —
// Usid 는 key, Active 는 sync diff(삭제 mark)에만 쓰인다.
type UserProfile struct {
	Usid   string       `json:"usid"`
	Site   session.Site `json:"site"`
	Tier   session.Tier `json:"tier"`
	Active bool         `json:"active"`
}

// UserProfiles — 목록.
type UserProfiles []UserProfile

// GradeMapper — 고객 DB 의 원시 등급/조직 코드를 WTG 도메인 enum 으로 변환.
//
// Oracle backend 가 SELECT 한 raw 코드(예 등급 "01", 조직 "B")를 이 mapper 로
// session.Tier/Site 화한다. 매핑표는 운영 등급 체계에 맞춰 설정(파일/etcd)으로
// 주입 — 미등록 코드는 fallback 으로 안전하게 처리(로그인 자체는 막지 않음).
//
// 운영 등급 체계가 확정되면 TierByGrade/SiteByCode 를 채우면 된다.
type GradeMapper struct {
	TierByGrade  map[string]session.Tier // 등급코드 → Tier (예 "01"→VIP)
	SiteByCode   map[string]session.Site // 조직코드 → Site
	TierFallback session.Tier            // 미매칭 등급 기본값 (빈값이면 STD)
	SiteFallback session.Site            // 미매칭 조직 기본값 (빈값이면 BRANCH)
}

// MapTier — 등급코드 → Tier. 미등록이면 fallback(기본 STD).
func (m GradeMapper) MapTier(grade string) session.Tier {
	if t, ok := m.TierByGrade[grade]; ok {
		return t
	}
	if m.TierFallback != "" {
		return m.TierFallback
	}
	return session.TierStandard
}

// MapSite — 조직코드 → Site. 미등록이면 fallback(기본 BRANCH).
func (m GradeMapper) MapSite(code string) session.Site {
	if s, ok := m.SiteByCode[code]; ok {
		return s
	}
	if m.SiteFallback != "" {
		return m.SiteFallback
	}
	return session.SiteBranch
}
