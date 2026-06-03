// Package ratelimit 은 WTG 의 토큰 버킷 rate limiter.
//
// 1차 구현은 in-memory (golang.org/x/time/rate 기반). 키 단위 (IP 또는
// user) 별로 별도 limiter 를 유지하며 sync.Map 으로 lock-free 조회.
//
// 운영 환경에서 분산 rate limit 이 필요해지면 (다중 인스턴스 일관성 또는
// 노드 간 공유 budget) Redis 기반 token bucket 으로 교체. 인터페이스
// (Limiter / KeyFunc) 는 동일하게 유지.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// KeyFunc 는 요청에서 rate limit 키를 추출한다.
type KeyFunc func(*http.Request) string

// IPKey 는 RemoteAddr 의 IP 부분.
//
// 운영 시 reverse proxy 뒤라면 X-Forwarded-For 사용 옵션 추가 필요.
// 단, 신뢰 가능한 proxy 만 통과한 후의 헤더만 신뢰 — 그렇지 않으면 spoof.
func IPKey(r *http.Request) string {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// HeaderKey 는 특정 HTTP 헤더 값으로 키를 만든다 (예: X-WTG-User).
func HeaderKey(name string) KeyFunc {
	return func(r *http.Request) string {
		return r.Header.Get(name)
	}
}

// UserOrIPKey — 사용자 헤더 (예: X-WTG-User) 가 채워져 있으면 "user:<usid>",
// 아니면 IP fallback "ip:<addr>" 키.
//
// metric kind 라벨 (ip / user) 을 prefix 로 운반 → middleware 가 분기. 인증된
// 사용자는 IP 가 NAT 뒤에 있어 다른 사용자와 같은 IP 라도 별도 버킷 → 한 명
// abuse 가 다른 사용자에 영향 없음.
func UserOrIPKey(headerName string) KeyFunc {
	return func(r *http.Request) string {
		if u := r.Header.Get(headerName); u != "" {
			return "user:" + u
		}
		return "ip:" + IPKey(r)
	}
}

// SplitKey — UserOrIPKey 로 만들어진 키를 (kind, raw) 로 분해.
// kind 는 "user" 또는 "ip", raw 는 prefix 제거된 식별자.
// prefix 가 없으면 ("ip", key) 로 보수적 해석.
func SplitKey(key string) (kind, raw string) {
	if i := strings.IndexByte(key, ':'); i > 0 {
		switch key[:i] {
		case "user", "ip":
			return key[:i], key[i+1:]
		}
	}
	return "ip", key
}

// Limiter 는 키 단위 토큰 버킷.
//
// rate: 초당 채워지는 토큰 수 (sustained TPS).
// burst: 버킷의 최대 토큰 수 (순간 폭주 허용 한도).
// idle: 일정 시간 미사용 키 자동 정리 (메모리 누수 방지).
type Limiter struct {
	rate  rate.Limit
	burst int
	idle  time.Duration

	mu   sync.RWMutex
	keys map[string]*entry

	// 가비지 콜렉터.
	stopCh chan struct{}
}

// Config 는 Limiter 옵션.
type Config struct {
	// RatePerSec — sustained TPS 한도.
	RatePerSec float64

	// Burst — 버킷 깊이.
	Burst int

	// IdleEviction — 키가 이 시간 동안 미사용이면 자동 정리 (기본 5분).
	IdleEviction time.Duration

	// EvictionPeriod — GC 주기 (기본 1분).
	EvictionPeriod time.Duration
}

type entry struct {
	limiter  *rate.Limiter
	lastUsed atomic.Int64 // unix nano
}

// NewLimiter 는 토큰 버킷 Limiter 생성 + GC 시작.
//
// EvictionPeriod 가 0 이면 GC 비활성 (테스트용).
func NewLimiter(cfg Config) *Limiter {
	if cfg.IdleEviction <= 0 {
		cfg.IdleEviction = 5 * time.Minute
	}
	l := &Limiter{
		rate:   rate.Limit(cfg.RatePerSec),
		burst:  cfg.Burst,
		idle:   cfg.IdleEviction,
		keys:   make(map[string]*entry),
		stopCh: make(chan struct{}),
	}
	if cfg.EvictionPeriod > 0 {
		go l.evictionLoop(cfg.EvictionPeriod)
	}
	return l
}

// Allow 는 키에 대해 토큰 1개를 소비 시도. true=허용, false=거부.
func (l *Limiter) Allow(key string) bool {
	e := l.entryFor(key)
	e.lastUsed.Store(time.Now().UnixNano())
	return e.limiter.Allow()
}

// entryFor — 신규 키면 limiter 생성, 아니면 기존 반환.
func (l *Limiter) entryFor(key string) *entry {
	l.mu.RLock()
	e, ok := l.keys[key]
	l.mu.RUnlock()
	if ok {
		return e
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// double-check.
	if e, ok := l.keys[key]; ok {
		return e
	}
	ne := &entry{limiter: rate.NewLimiter(l.rate, l.burst)}
	l.keys[key] = ne
	return ne
}

// evictionLoop 은 주기적으로 idle 키를 정리.
func (l *Limiter) evictionLoop(period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case now := <-t.C:
			cutoff := now.Add(-l.idle).UnixNano()
			l.mu.Lock()
			for k, e := range l.keys {
				if e.lastUsed.Load() < cutoff {
					delete(l.keys, k)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Stop 은 GC goroutine 종료 (서버 셧다운 시).
func (l *Limiter) Stop() {
	close(l.stopCh)
}

// KeyCount 는 현재 등록된 키 수 (모니터링용).
func (l *Limiter) KeyCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.keys)
}

// Middleware 는 HTTP 미들웨어로 사용. 키 추출 후 Allow → 거부 시 429.
//
// 사용 예:
//
//	ipLimiter := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 100, Burst: 200})
//	chain := middleware.Chain(mux, ratelimit.Middleware(ipLimiter, ratelimit.IPKey))
func Middleware(l *Limiter, keyFn KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFn(r)
			if key == "" {
				// 키 없음 — 보수적으로 통과 (또는 정책에 따라 거부).
				next.ServeHTTP(w, r)
				return
			}
			if !l.Allow(key) {
				writeRateLimited(w, "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// MetricsHook — Allow / Deny 시점 callback (Prometheus 카운터 등록용).
//
// rule 은 RuleSet.Match 가 반환한 패턴 명 ("POST /v1/tx", "default", "").
// kind 는 "user" 또는 "ip" (UserOrIPKey 의 prefix 에서 추출).
type MetricsHook struct {
	OnAllowed func(rule, kind string)
	OnDenied  func(rule, kind string)
}

// MiddlewareRules — path-aware 룰셋 기반 미들웨어. RuleSet.Allow 의 결과로
// 분기. 거부 시 429 + 룰명 동봉 (운영 디버그 용).
//
// keyFn 이 nil 키 반환 시 보수적 통과 (이전 동작과 동일).
// metrics 는 nil 가능 — 카운터 hook 만 빠짐, 동작은 동일.
func MiddlewareRules(rs *RuleSet, keyFn KeyFunc, metrics MetricsHook) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFn(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			rule, allowed := rs.Allow(r.Method, r.URL.Path, key)
			if rule == "" {
				// 매칭 룰 없고 fallback 도 없음 — 통과.
				next.ServeHTTP(w, r)
				return
			}
			kind, _ := SplitKey(key)
			if !allowed {
				if metrics.OnDenied != nil {
					metrics.OnDenied(rule, kind)
				}
				writeRateLimited(w, rule)
				return
			}
			if metrics.OnAllowed != nil {
				metrics.OnAllowed(rule, kind)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeRateLimited — 429 응답 통일. rule 이 비어있지 않으면 본문에 동봉.
func writeRateLimited(w http.ResponseWriter, rule string) {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	if rule == "" {
		_, _ = w.Write([]byte(`{"error":"rate_limited","message":"요청 한도 초과"}`))
		return
	}
	// rule 동봉 — `"` 이스케이프 (룰 패턴에 따옴표 없을 거지만 안전).
	body := `{"error":"rate_limited","message":"요청 한도 초과","rule":"` +
		strings.ReplaceAll(rule, `"`, `\"`) + `"}`
	_, _ = w.Write([]byte(body))
}
