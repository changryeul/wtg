package ratelimit

import (
	"fmt"
	"path"
	"strings"
	"sync/atomic"
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
	Pattern string  `json:"pattern"`
	Rate    float64 `json:"rate"`
	Burst   int     `json:"burst"`
}

// compiledRule — Rule 을 미리 분해해서 매칭 비용 절감.
type compiledRule struct {
	Rule
	method   string // "" = any
	pathGlob string
	limiter  *Limiter
}

// ruleSetData — atomic.Pointer 로 hot-swap 되는 룰셋 snapshot.
// in-flight 요청은 swap 전 snapshot 으로 끝까지 수행 — 정책 변경의 순간 race
// 는 의도된 동작 (어느 한 쪽 정책 그대로 적용).
type ruleSetData struct {
	rules    []compiledRule
	fallback *Limiter
}

// RuleSet 은 path-aware 룰 묶음. middleware 가 요청마다 첫 매칭 룰의 limiter
// 로 위임. 매칭 안 되면 fallback (있으면), 아니면 통과.
//
// hot-swap — Replace(rules, fallbackCfg) 가 atomic 교체. old 의 limiter 는
// 즉시 Stop (in-flight Allow 는 limiter 자체가 thread-safe 라 safe).
type RuleSet struct {
	data atomic.Pointer[ruleSetData]
}

// NewRuleSet — rules 순서대로 컴파일. 각 룰별 별도 Limiter 인스턴스.
// fallbackCfg 가 nil 이 아니면 매칭 안 된 요청에 적용 (nil 이면 통과).
//
// 빈 Pattern 또는 잘못된 glob 은 에러.
func NewRuleSet(rules []Rule, fallbackCfg *Config) (*RuleSet, error) {
	data, err := compile(rules, fallbackCfg)
	if err != nil {
		return nil, err
	}
	rs := &RuleSet{}
	rs.data.Store(data)
	return rs, nil
}

// compile — rules + fallbackCfg 를 ruleSetData 로. atomic swap 단위.
func compile(rules []Rule, fallbackCfg *Config) (*ruleSetData, error) {
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
		if r.Rate < 0 || r.Burst < 0 {
			return nil, fmt.Errorf("rule[%d] %q: rate/burst 음수", i, r.Pattern)
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
	d := &ruleSetData{rules: compiled}
	if fallbackCfg != nil {
		d.fallback = NewLimiter(*fallbackCfg)
	}
	return d, nil
}

// stopAll — ruleSetData 의 모든 limiter GC goroutine 종료.
func (d *ruleSetData) stopAll() {
	for i := range d.rules {
		d.rules[i].limiter.Stop()
	}
	if d.fallback != nil {
		d.fallback.Stop()
	}
}

// Replace — 새 룰셋으로 hot-swap. compile 단계 실패 시 기존 룰 유지 + 에러.
// 성공 시 old data 의 limiter 들 즉시 Stop.
func (rs *RuleSet) Replace(rules []Rule, fallbackCfg *Config) error {
	newData, err := compile(rules, fallbackCfg)
	if err != nil {
		return err
	}
	old := rs.data.Swap(newData)
	if old != nil {
		old.stopAll()
	}
	return nil
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
	d := rs.data.Load()
	if d == nil {
		return nil, "", false
	}
	for i := range d.rules {
		r := &d.rules[i]
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
	if d.fallback != nil {
		return d.fallback, "default", true
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

// Stop — 현재 데이터의 모든 limiter GC goroutine 종료 (서버 셧다운 시).
func (rs *RuleSet) Stop() {
	if d := rs.data.Load(); d != nil {
		d.stopAll()
	}
}

// Rules — 현재 등록된 룰 패턴 + 한도 (모니터링/디버그용).
func (rs *RuleSet) Rules() []Rule {
	d := rs.data.Load()
	if d == nil {
		return nil
	}
	out := make([]Rule, len(d.rules))
	for i, r := range d.rules {
		out[i] = r.Rule
	}
	return out
}
