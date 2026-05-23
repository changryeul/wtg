// Package session 은 web 세션의 도메인 타입을 정의한다.
//
// 이 패키지는 의존성 그래프의 가장 안쪽(leaf 에 가까운 곳)에 위치하며,
// pkg/auth (세션 저장/조회) 와 pkg/pricing (마진 적용) 가 공통으로
// 사용하는 enum / value object 를 제공한다.
//
// 설계 의도:
//
//   - Channel/Site/Tier/Profile 같은 도메인 식별자를 한 곳에 모아
//     pkg/auth ↔ pkg/pricing 의 순환 import 를 회피한다.
//   - 모든 타입은 string-based enum — etcd/JSON/routing-key 직렬화 친화적.
//   - Profile 은 로그인 시 확정되는 immutable value object.
//
// 채울 출처 메모:
//
//   - Channel: 사용자가 접속한 edge 게이트웨이가 결정 (mci-edge-{api,push,...}).
//   - Site:    매매엔진 cookie_t 의 사용자 속성에서 도출. **현재 mymq.Cookie 의
//              Coki 페이로드 스펙 합의 필요.** 합의 전까지는 로그인 핸들러가
//              임시값을 채우거나 별도 트랜잭션으로 보강.
//   - Tier:    Site 와 동일 — cookie_t 또는 별도 사용자 메타 트랜잭션에서 도출.
package session

// Channel 은 사용자가 접속한 채널.
// edge 게이트웨이별로 1:1 매핑되므로 위·변조 불가능 (클라이언트가 못 고름).
type Channel string

const (
	ChannelWeb    Channel = "WEB"
	ChannelMobile Channel = "MOB"
	ChannelCS     Channel = "CS"
	ChannelFIX    Channel = "FIX"
	ChannelAdmin  Channel = "ADM" // 운영 콘솔 — 시세 publish 대상 아님
	// 주의: 모든 Channel 값은 ≤3자로 유지한다. Profile.Key() 결과가
	// mymq routing-key 한도 (LRkey=16바이트) 안에 들어가야 한다.
	// 최악: 3(Channel) + 1 + 6(BRANCH) + 1 + 4(GOLD) = 15 bytes.
)

// Site 는 거래 주체 구분 (영업점 / 본점).
// 같은 사용자라도 본점 트레이딩과 영업점 거래는 마진이 다를 수 있다.
type Site string

const (
	SiteBranch Site = "BRANCH"
	SiteHQ     Site = "HQ"
)

// Tier 는 고객 등급. 빈값("") 은 마진 테이블의 와일드카드 fallback 키로 사용.
// 운영자가 등급별 마진을 등록하지 않으면 빈값 entry 로 fallback 된다.
type Tier string

const (
	TierVIP      Tier = "VIP"
	TierGold     Tier = "GOLD"
	TierStandard Tier = "STD"
)

// Profile 은 세션이 어떤 시세를 받을지를 결정하는 3차원 튜플.
// 로그인 시점에 확정되며 거래 중간에 바뀌지 않는다 (immutable value object).
//
// 클라이언트는 Profile 을 주장(claim)할 수 없다 — 서버가 edge + cookie_t 에서
// 도출해 세션에 기록하고, 이후 모든 시세 fan-out 의 routing-key 매칭에 사용한다.
type Profile struct {
	Channel Channel
	Site    Site
	Tier    Tier
}

// Key 는 broker routing-key 의 prefix 로 쓰이는 안정된 문자열 (예: "WEB.BRANCH.VIP").
// publish/subscribe topic 구성에 직접 사용된다.
func (p Profile) Key() string {
	return string(p.Channel) + "." + string(p.Site) + "." + string(p.Tier)
}

// Pair 는 통화쌍 표기 (예: "USD/KRW").
// 마진 테이블 / 구독 상태 / publish routing-key 의 공통 식별자.
type Pair string

// LogonID 는 broker broadcast prefix (≤80B) 매칭에 사용되는 사용자 식별자.
// 빈값이면 전체 broadcast — 시세 같이 전체 배포되는 메시지에 주로 사용한다.
// 사용자별 unsolicited push 의 fan-out target 결정에 쓰임.
type LogonID string
