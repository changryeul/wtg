package ratelimit

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMiniRedisLimiter(t *testing.T, rate float64, burst int) (*RedisLimiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lim := NewRedisLimiter(RedisLimiterOptions{
		Client: rdb, RatePerSec: rate, Burst: burst, Prefix: "test:",
	})
	return lim, mr
}

// 단일 인스턴스 burst 동작 — in-memory 와 동등.
func TestRedisLimiter_BurstThenDeny(t *testing.T) {
	lim, _ := newMiniRedisLimiter(t, 1, 2)
	// 첫 2건 burst 통과.
	if !lim.Allow("k") {
		t.Error("첫 토큰 거부")
	}
	if !lim.Allow("k") {
		t.Error("두번째 토큰 거부")
	}
	// 3번째는 burst 초과 → 거부.
	if lim.Allow("k") {
		t.Error("burst 초과인데 통과")
	}
}

// 키 별 독립 버킷.
func TestRedisLimiter_PerKeyIndependent(t *testing.T) {
	lim, _ := newMiniRedisLimiter(t, 1, 1)
	if !lim.Allow("k1") {
		t.Error("k1 첫 토큰 거부")
	}
	if !lim.Allow("k2") {
		t.Error("k2 첫 토큰 거부")
	}
	if lim.Allow("k1") {
		t.Error("k1 두번째: burst 초과인데 통과")
	}
}

// rate refill — Lua 가 클라이언트 시각 (ARGV[1]) 기준이라 future time 주입.
func TestRedisLimiter_RefillOverTime(t *testing.T) {
	lim, _ := newMiniRedisLimiter(t, 2, 1)
	t0 := time.Now()
	if !lim.allowAt("k", t0) {
		t.Error("첫 토큰 거부")
	}
	if lim.allowAt("k", t0) {
		t.Error("burst 1 인데 두번째 통과")
	}
	// 1초 뒤 → rate 2 / sec 이므로 2 토큰 보충 (burst cap 1).
	if !lim.allowAt("k", t0.Add(time.Second)) {
		t.Error("refill 후에도 거부")
	}
}

// 다중 인스턴스 — 같은 Redis 공유하는 두 RedisLimiter 가 단일 버킷처럼 동작.
// 한 인스턴스가 burst 다 소진하면 다른 인스턴스도 거부 — 분산 일관성.
func TestRedisLimiter_MultiInstance_SharedBudget(t *testing.T) {
	mr := miniredis.RunT(t)
	mkInstance := func() *RedisLimiter {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		return NewRedisLimiter(RedisLimiterOptions{
			Client: rdb, RatePerSec: 1, Burst: 2, Prefix: "shared:",
		})
	}
	insA := mkInstance()
	insB := mkInstance()

	// 인스턴스 A 가 burst (2) 다 소비.
	if !insA.Allow("ip") {
		t.Error("A 첫 거부")
	}
	if !insA.Allow("ip") {
		t.Error("A 두번째 거부")
	}
	// 인스턴스 B 가 같은 키로 호출 → 거부 (단일 버킷).
	if insB.Allow("ip") {
		t.Error("B 가 통과 — 인스턴스 간 격리 = 분산 의미 위반")
	}
}

// Redis 끊김 시 fail-open + failCount 증가 + OnFail callback 호출.
func TestRedisLimiter_FailOpenOnRedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	var onFailCalled atomic.Int32
	lim := NewRedisLimiter(RedisLimiterOptions{
		Client: rdb, RatePerSec: 1, Burst: 1, Prefix: "test:",
		Logger: quietLogger(),
		OnFail: func() { onFailCalled.Add(1) },
	})

	// Redis 닫음 → Eval 실패.
	mr.Close()

	if !lim.Allow("k") {
		t.Error("Redis 끊김인데 통과 안 됨 — fail-open 정책 위반")
	}
	if lim.FailCount() != 1 {
		t.Errorf("FailCount = %d, want 1", lim.FailCount())
	}
	if onFailCalled.Load() != 1 {
		t.Errorf("OnFail callback 호출 수 = %d, want 1", onFailCalled.Load())
	}
}

// 동시성 race — 단일 키 1000 동시 호출 시 정확히 burst 개만 통과.
func TestRedisLimiter_ConcurrentSingleKey(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	lim := NewRedisLimiter(RedisLimiterOptions{
		Client: rdb, RatePerSec: 0, Burst: 10, Prefix: "test:",
	})

	var allowed atomic.Int32
	done := make(chan struct{}, 100)
	for i := 0; i < 100; i++ {
		go func() {
			if lim.Allow("k") {
				allowed.Add(1)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
	// rate=0, burst=10 → 정확히 10 통과.
	if allowed.Load() != 10 {
		t.Errorf("동시 호출: allowed=%d, want 10 (burst)", allowed.Load())
	}
}

// RuleSet + Redis factory — 다중 인스턴스가 같은 Redis 로 정책 적용.
// 두 인스턴스가 동일 룰 (same prefix) 사용 + 같은 키 부하 → 단일 카운터.
func TestRuleSetWithRedisFactory_MultiInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	mkRS := func() *RuleSet {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		rules := []Rule{{Pattern: "POST /v1/tx", Rate: 0, Burst: 5}}
		rs, err := NewRuleSetWithFactory(rules, nil,
			func(r Rule) AllowLimiter {
				return NewRedisLimiter(RedisLimiterOptions{
					Client: rdb, RatePerSec: r.Rate, Burst: r.Burst,
					Prefix: "shared:" + r.Pattern + ":",
				})
			}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	rsA := mkRS()
	rsB := mkRS()
	defer rsA.Stop()
	defer rsB.Stop()

	// 인스턴스 A 가 5건 통과 (burst 소진).
	for i := 0; i < 5; i++ {
		if _, ok := rsA.Allow("POST", "/v1/tx", "ip-1"); !ok {
			t.Fatalf("A %d 번째 거부", i+1)
		}
	}
	// 인스턴스 B 가 같은 키 → 단일 카운터라 거부.
	if _, ok := rsB.Allow("POST", "/v1/tx", "ip-1"); ok {
		t.Error("B 통과 — 분산 카운터 위반 (인스턴스 간 공유 안 됨)")
	}
}
