package chart

import "github.com/winwaysystems/wtg/pkg/ratelimit"

// DefaultRateLimitRules — mci-edge-chart 의 path-aware 기본 룰셋.
//
// 운영자가 cfg.RateLimitRules 를 비워두면 본 함수 결과가 사용됨. UserOrIPKey
// 와 결합해 한 user/IP 당 별도 버킷.
//
// chart 는 REST proxy + ws (라이브 봉) 양쪽 서비스 — historical 조회는
// 빈번하지만 가볍고, ws 는 handshake 자체가 비용 (인증 + Registry 등록).
func DefaultRateLimitRules() []ratelimit.Rule {
	return []ratelimit.Rule{
		// ws handshake — 잦은 재연결 차단.
		{Pattern: "GET /v1/chart/stream", Rate: 5, Burst: 10},
		// historical 조회 — 차트 페이지 진입 시 다량 가능.
		{Pattern: "GET /v1/chart", Rate: 200, Burst: 400},
		{Pattern: "GET /v1/chart/*", Rate: 200, Burst: 400},
		// health check — 거의 무제한.
		{Pattern: "GET /healthz", Rate: 1000, Burst: 2000},
	}
}
