package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync/atomic"

	"github.com/winwaysystems/wtg/pkg/session"
)

// UserProfile 은 사용자별 정적 메타. 시세 fan-out 의 Profile 결정에 사용된다.
//
// **권위 출처 원칙**: 클라이언트가 로그인 요청 body 에 Site/Tier 를 보내는 것을
// 신뢰하지 않는다. 운영자가 mci-admin (또는 별도 사용자 카탈로그) 에 등록한
// 값만 신뢰 — Resolver 가 Login 시점에 usid 로 조회해 Session.Profile 에 채움.
//
// 향후 매매엔진 cookie_t.Coki 페이로드 스펙이 합의되면 BrokerResolver 가 추가될
// 수 있다 (Resolver 인터페이스 동일).
type UserProfile struct {
	Site session.Site `json:"site"`
	Tier session.Tier `json:"tier"`
}

// ErrUserProfileNotFound — 등록되지 않은 사용자.
// 호출자는 nil 반환과 함께 빈 Profile 로 fallback 하거나, 로그인 거부할 수 있다.
var ErrUserProfileNotFound = errors.New("auth: user profile 미등록")

// UserProfileResolver 는 usid → UserProfile 조회 추상화.
//
// 구현체:
//   - StaticResolver — in-memory map (file 로드 또는 직접 채움). dev / 단일 인스턴스.
//   - EtcdResolver   — etcd watch 기반 (mci-admin 으로 운영 관리). 운영 표준.
//   - (future) BrokerResolver — broker LOGON 응답 / USRPROF 트랜잭션 위임.
type UserProfileResolver interface {
	Resolve(ctx context.Context, usid string) (UserProfile, error)
}

// ─── StaticResolver ────────────────────────────────────────────────────────

// StaticResolver 는 in-memory map 기반 Resolver. dev / 테스트 / 단일 인스턴스 운영.
// 운영에선 EtcdResolver 권장 (다중 인스턴스 동기화 + admin UI).
//
// 동시성: atomic.Pointer 스냅샷 — Replace 와 Resolve 가 race 없음.
type StaticResolver struct {
	p atomic.Pointer[map[string]UserProfile]
}

// NewStaticResolver 는 빈 StaticResolver 를 만든다.
func NewStaticResolver() *StaticResolver {
	r := &StaticResolver{}
	m := map[string]UserProfile{}
	r.p.Store(&m)
	return r
}

// Replace 는 전체 map 을 atomic 으로 교체한다 (호출자 소유 → 이후 수정 금지).
func (r *StaticResolver) Replace(m map[string]UserProfile) {
	copy := make(map[string]UserProfile, len(m))
	for k, v := range m {
		copy[k] = v
	}
	r.p.Store(&copy)
}

// Set 은 단일 entry 를 갱신한다 (copy-on-write).
func (r *StaticResolver) Set(usid string, p UserProfile) {
	curr := r.p.Load()
	next := make(map[string]UserProfile, len(*curr)+1)
	for k, v := range *curr {
		next[k] = v
	}
	next[usid] = p
	r.p.Store(&next)
}

// Resolve — 미등록 사용자는 ErrUserProfileNotFound.
func (r *StaticResolver) Resolve(_ context.Context, usid string) (UserProfile, error) {
	m := r.p.Load()
	if m == nil {
		return UserProfile{}, ErrUserProfileNotFound
	}
	if v, ok := (*m)[usid]; ok {
		return v, nil
	}
	return UserProfile{}, ErrUserProfileNotFound
}

// Size 는 등록된 사용자 수.
func (r *StaticResolver) Size() int {
	m := r.p.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// ─── 파일 로더 ─────────────────────────────────────────────────────────────

// UserProfilesFileEntry 는 파일 JSON 의 한 엔트리.
//
// 파일 포맷 (예: etc/user-profiles.json):
//
//	[
//	  {"usid": "trader01", "site": "BRANCH", "tier": "VIP"},
//	  {"usid": "trader02", "site": "HQ",     "tier": "STD"}
//	]
type UserProfilesFileEntry struct {
	Usid string       `json:"usid"`
	Site session.Site `json:"site"`
	Tier session.Tier `json:"tier"`
}

// LoadStaticResolverFromFile 은 JSON 파일을 읽어 StaticResolver 를 채운다.
// path 비어있으면 빈 resolver 반환 (에러 아님).
func LoadStaticResolverFromFile(path string) (*StaticResolver, error) {
	r := NewStaticResolver()
	if path == "" {
		return r, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var entries []UserProfilesFileEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}
	m := make(map[string]UserProfile, len(entries))
	for _, e := range entries {
		m[e.Usid] = UserProfile{Site: e.Site, Tier: e.Tier}
	}
	r.Replace(m)
	return r, nil
}

// 컴파일 보장.
var _ UserProfileResolver = (*StaticResolver)(nil)
