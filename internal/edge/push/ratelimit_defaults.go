package push

import "github.com/winwaysystems/wtg/pkg/ratelimit"

// DefaultRateLimitRules — mci-edge-push 의 path-aware 기본 룰셋.
//
// push 는 거의 ws 만 — 메시지는 ws 안이라 path-aware 무관이고 핵심은 ws
// handshake 의 frequency limit (재연결 abuse / brute force). UserOrIPKey 와
// 결합되어 한 user/IP 가 분당 N 회 reconnect 만 허용.
func DefaultRateLimitRules() []ratelimit.Rule {
	return []ratelimit.Rule{
		// ws handshake — 잦은 재연결 차단.
		{Pattern: "GET /v1/subscribe", Rate: 5, Burst: 10},
		// 운영 진단 — 자주 호출되지만 가벼움.
		{Pattern: "GET /v1/edge-stats", Rate: 10, Burst: 20},
		// health check — 거의 무제한.
		{Pattern: "GET /v1/ping", Rate: 1000, Burst: 2000},
	}
}
