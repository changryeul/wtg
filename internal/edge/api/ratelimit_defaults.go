package api

import "github.com/winwaysystems/wtg/pkg/ratelimit"

// DefaultRateLimitRules 는 mci-edge-api 의 path-aware 기본 룰셋.
//
// 운영자가 cfg.RateLimitRules 를 비워두면 본 함수의 결과가 사용됨. 한 명의
// 사용자/IP 가 cheap path (ping/quote 조회) 를 polling 해도 critical path
// (login/tx/admin) 의 토큰을 소모하지 않도록 별도 버킷.
//
// 각 한도는 "한 user 또는 한 IP" 기준 — 인증된 사용자는 X-WTG-User 헤더로
// 별도 버킷 (UserOrIPKey). 운영 환경에서 측정 후 조정 권장.
//
// 룰 순서대로 첫 매칭 적용 — 좁은 규칙을 위에.
func DefaultRateLimitRules() []ratelimit.Rule {
	return []ratelimit.Rule{
		// 인증 path — brute force 방지 (분당 300 시도).
		{Pattern: "POST /v1/login", Rate: 5, Burst: 10},
		// 토큰 갱신 — 빈번하지 않음.
		{Pattern: "POST /v1/refresh", Rate: 20, Burst: 40},
		// 매매 — 매매 엔진 보호. 정상 거래는 분당 100건 이하.
		{Pattern: "POST /v1/tx", Rate: 50, Burst: 100},
		// 관리 API — 운영자 한 명 기준.
		{Pattern: "POST /v1/admin/*", Rate: 10, Burst: 20},
		{Pattern: "PUT /v1/admin/*", Rate: 10, Burst: 20},
		{Pattern: "DELETE /v1/admin/*", Rate: 10, Burst: 20},
		{Pattern: "GET /v1/admin/*", Rate: 50, Burst: 100},
		// 시세 조회 — 자주 호출되지만 비싸지 않음.
		{Pattern: "GET /v1/quote/*", Rate: 200, Burst: 400},
		// health check — 거의 제한 없음.
		{Pattern: "GET /v1/ping", Rate: 1000, Burst: 2000},
	}
}
