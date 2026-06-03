package ratelimit

import (
	"fmt"
	"path"
	"strings"
)

// Rule 은 path-aware rate limit 의 단일 정책 entry.
//
// Pattern 은 "<METHOD> <path-glob>" 또는 "<path-glob>" 형태.
//   - "POST /v1/tx"          — POST 만, 정확 매칭
//   - "GET /v1/chart/*"      — GET 만, glob (path.Match)
//   - "/v1/admin/*"          — 모든 method, glob
//   - "*"                    — 모든 method + 모든 path
//
// glob 은 path/filepath 의 path.Match — `*` 은 `/` 를 cross 하지 않음.
// `/v1/chart/*` 는 `/v1/chart/foo` 매칭, `/v1/chart/foo/bar` 는 안 됨.
type Rule struct {
	Pattern string
	Rate    float64
	Burst   int
}

// compiledRule — Rule 을 미리 분해해서 매칭 비용 절감.
type compiledRule struct {
	Rule
	method   string // "" = any
	pathGlob string
	limiter  *Limiter
}

// RuleSet 은 path-aware 룰 묶음. middleware 가 요청마다 첫 매칭 룰의 limiter
// 로 위임. 매칭 안 되면 fallback (있으면), 아니면 통과.
type RuleSet struct {
	rules    []compiledRule
	fallback *Limiter
}

// NewRuleSet — rules 순서대로 컴파일. 각 룰별 별도 Limiter 인스턴스.
// fallbackCfg 가 nil 이 아니면 매칭 안 된 요청에 적용 (nil 이면 통과).
//
// 빈 Pattern 또는 잘못된 glob 은 에러.
func NewRuleSet(rules []Rule, fallbackCfg *Config) (*RuleSet, error) {
	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		method, glob, err := parsePattern(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("rule[%d] %q: %w", i, r.Pattern, err)
		}
		// glob 유효성 사전 검증 — bad glob 은 path.Match 에서 ErrBadPattern.
		if _, err := path.Match(glob, "/probe"); err != nil {
			return nil, fmt.Errorf("rule[%d] %q glob 오류: %w", i, r.Pattern, err)
		}
		compiled = append(compiled, compiledRule{
			Rule:     r,
			method:   method,
			pathGlob: glob,
			limiter: NewLimiter(Config{
				RatePerSec: r.Rate,
				Burst:      r.Burst,
			}),
		})
	}
	rs := &RuleSet{rules: compiled}
	if fallbackCfg != nil {
		rs.fallback = NewLimiter(*fallbackCfg)
	}
	return rs, nil
}

// parsePattern — "POST /path" / "/path" / "*" 분해.
func parsePattern(pat string) (method, glob string, err error) {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return "", "", fmt.Errorf("빈 패턴")
	}
	if pat == "*" {
		return "", "*", nil
	}
	parts := strings.SplitN(pat, " ", 2)
	if len(parts) == 2 && isHTTPMethod(parts[0]) {
		return strings.ToUpper(parts[0]), strings.TrimSpace(parts[1]), nil
	}
	// method 없음 — 전체 pattern 을 path 로.
	return "", pat, nil
}

func isHTTPMethod(s string) bool {
	switch strings.ToUpper(s) {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// Match — 첫 매칭 룰. 매칭 안 되고 fallback 도 없으면 (nil, "", false).
// fallback 매칭 시 룰명은 "default".
func (rs *RuleSet) Match(method, urlPath string) (*Limiter, string, bool) {
	for i := range rs.rules {
		r := &rs.rules[i]
		if r.method != "" && r.method != method {
			continue
		}
		// "*" path 는 모든 path 매칭.
		if r.pathGlob == "*" {
			return r.limiter, r.Pattern, true
		}
		matched, err := path.Match(r.pathGlob, urlPath)
		if err != nil || !matched {
			continue
		}
		return r.limiter, r.Pattern, true
	}
	if rs.fallback != nil {
		return rs.fallback, "default", true
	}
	return nil, "", false
}

// Allow — 매칭된 룰의 limiter 로 토큰 소비. 매칭 안 되면 (default 도 없으면)
// 통과. 반환은 (rule 명, allowed). rule 이 빈 문자열이면 매칭 X (통과).
func (rs *RuleSet) Allow(method, urlPath, key string) (string, bool) {
	lim, name, matched := rs.Match(method, urlPath)
	if !matched {
		return "", true
	}
	return name, lim.Allow(key)
}

// Stop — 모든 룰 limiter + fallback 의 GC goroutine 종료.
func (rs *RuleSet) Stop() {
	for i := range rs.rules {
		rs.rules[i].limiter.Stop()
	}
	if rs.fallback != nil {
		rs.fallback.Stop()
	}
}

// Rules — 등록된 룰 패턴 + 한도 (모니터링/디버그용).
func (rs *RuleSet) Rules() []Rule {
	out := make([]Rule, len(rs.rules))
	for i, r := range rs.rules {
		out[i] = r.Rule
	}
	return out
}
