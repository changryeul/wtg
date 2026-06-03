package price

import "github.com/winwaysystems/wtg/pkg/ratelimit"

// DefaultRateLimitRules — mci-edge-price 의 path-aware 기본 룰셋.
//
// push 와 동일하게 ws 가 핵심. admin path 는 운영자 한 명 기준 strict.
func DefaultRateLimitRules() []ratelimit.Rule {
	return []ratelimit.Rule{
		// 관리 API — 운영자 한 명 기준 brute force 방지.
		{Pattern: "POST /v1/admin/*", Rate: 10, Burst: 20},
		// ws handshake — 잦은 재연결 차단.
		{Pattern: "GET /v1/subscribe", Rate: 5, Burst: 10},
		// 운영 진단.
		{Pattern: "GET /v1/edge-stats", Rate: 10, Burst: 20},
		// health check — 거의 무제한.
		{Pattern: "GET /v1/ping", Rate: 1000, Burst: 2000},
	}
}
