package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// auditTracer — Redis backend 호출 OTel span 발행. 영구 audit 의 latency tail
// + failure 시 가시화 — fail 시 in-memory fallback 이라 매 호출 trace 가치.
var auditTracer = otel.Tracer("wtg.admin.audit.redis")

// AuditEntry 는 단일 admin 액션 기록 (auth.md §10 ADMIN_ACTION).
//
// 운영에서는 immutable 외부 sink (7년 보관) 가 single source of truth 이고,
// 본 ring 은 UI 의 즉시 표시 + 로컬 디버깅용 sliding window.
type AuditEntry struct {
	Action   string         `json:"action"`             // PUT_ROUTE / DELETE_SYMBOL / 등
	Resource string         `json:"resource,omitempty"` // route / symbol / profile / user_profile / pricing / quoteid_engine / policy / svcio
	Usid     string         `json:"usid,omitempty"`     // 액션 수행 admin
	RID      string         `json:"rid,omitempty"`      // request id
	At       time.Time      `json:"at"`                 // 발생 시각
	Attrs    map[string]any `json:"attrs,omitempty"`    // 액션별 상세 (alias / active / key / 등)
}

// AuditRing 은 audit 항목 저장소. 기본은 in-memory 고정크기 ring buffer.
//
// Redis backend (SetRedisBackend) 활성 시 — Push 는 LPUSH + LTRIM 으로
// Redis 에 영속, List 는 LRANGE 로 Redis fetch. 재시작 시 항목 손실 X.
// Redis 호출 실패 시 fail-open (in-memory ring 만 사용) + failCount 누적.
//
// 동기화: sync.RWMutex. 작은 사이즈 (200) 라 락 경합 무시 가능.
type AuditRing struct {
	mu  sync.RWMutex
	cap int
	buf []AuditEntry
	// next 는 다음 쓰기 위치 (0..cap-1).
	next int
	// full 은 한 바퀴 돌았는지 — true 면 모든 슬롯이 유효.
	full bool
	// onPush — 신규 항목 추가 시 호출 (ws 브로드캐스트 등). nil 가능.
	onPush func(AuditEntry)

	// Redis backend — nil 이면 in-memory only.
	redis       *redis.Client
	redisKey    string
	redisMaxLn  int64 // LTRIM 후 보존할 길이
	logger      *slog.Logger
	redisFails  atomic.Uint64
	onRedisFail func() // Prometheus 카운터 등에 연결. nil 가능.
}

// SetOnPush 는 push 콜백을 설정한다 (등록은 1회).
func (r *AuditRing) SetOnPush(f func(AuditEntry)) {
	r.mu.Lock()
	r.onPush = f
	r.mu.Unlock()
}

// NewAuditRing 은 capacity 크기의 ring 을 만든다.
// capacity <= 0 이면 200.
func NewAuditRing(capacity int) *AuditRing {
	if capacity <= 0 {
		capacity = 200
	}
	return &AuditRing{
		cap: capacity,
		buf: make([]AuditEntry, capacity),
	}
}

// SetRedisBackend — Redis 영속 backend 활성. 활성 후 Push 는 Redis 에 LPUSH +
// LTRIM, List 는 LRANGE 로 Redis fetch. nil client 면 비활성 (in-memory only).
//
// maxLen 은 Redis LIST 보존 길이 (예: 10000). <= 0 면 1000.
// logger nil 가능.
func (r *AuditRing) SetRedisBackend(rdb *redis.Client, key string, maxLen int64, logger *slog.Logger) {
	if rdb == nil {
		return
	}
	if key == "" {
		key = "wtg:audit"
	}
	if maxLen <= 0 {
		maxLen = 1000
	}
	if logger == nil {
		logger = slog.Default()
	}
	r.mu.Lock()
	r.redis = rdb
	r.redisKey = key
	r.redisMaxLn = maxLen
	r.logger = logger
	r.mu.Unlock()
}

// SetRedisFailCallback — Redis 호출 실패 시 호출. Prometheus 카운터 등에 연결.
// SetRedisBackend 이후 호출.
func (r *AuditRing) SetRedisFailCallback(fn func()) {
	r.mu.Lock()
	r.onRedisFail = fn
	r.mu.Unlock()
}

// noteRedisFail — 내부 helper, failCount + onRedisFail 동시 처리.
func (r *AuditRing) noteRedisFail() {
	r.redisFails.Add(1)
	r.mu.RLock()
	cb := r.onRedisFail
	r.mu.RUnlock()
	if cb != nil {
		cb()
	}
}

// RedisFailCount — Redis 호출 실패 누적 (operations 모니터링).
func (r *AuditRing) RedisFailCount() uint64 {
	return r.redisFails.Load()
}

// Push 는 항목을 추가한다 (At 가 0 이면 자동 채움).
func (r *AuditRing) Push(e AuditEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	r.mu.Lock()
	r.buf[r.next] = e
	r.next++
	if r.next >= r.cap {
		r.next = 0
		r.full = true
	}
	cb := r.onPush
	rdb := r.redis
	key := r.redisKey
	maxLen := r.redisMaxLn
	logger := r.logger
	r.mu.Unlock()
	if rdb != nil {
		r.pushRedis(rdb, key, maxLen, logger, e)
	}
	if cb != nil {
		cb(e)
	}
}

// pushRedis — LPUSH + LTRIM (atomic in MULTI). 실패 시 fail-open
// (in-memory ring 만 사용) + failCount 증가.
func (r *AuditRing) pushRedis(rdb *redis.Client, key string, maxLen int64, logger *slog.Logger, e AuditEntry) {
	data, err := json.Marshal(e)
	if err != nil {
		r.noteRedisFail()
		logger.Warn("audit Redis marshal 실패", slog.Any("error", err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ctx, span := auditTracer.Start(ctx, "redis.audit.push",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "lpush+ltrim"),
			attribute.String("audit.key", key),
			attribute.String("audit.resource", e.Resource),
			attribute.String("audit.action", e.Action),
		))
	defer span.End()
	pipe := rdb.Pipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, maxLen-1)
	if _, err := pipe.Exec(ctx); err != nil {
		r.noteRedisFail()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		logger.Warn("audit Redis LPUSH/LTRIM 실패 — in-memory 만 보존",
			slog.String("key", key), slog.Any("error", err))
	}
}

// List 는 시간 역순 (최신 → 오래된) 으로 항목을 복사 반환.
// limit > 0 이면 처음 limit 개만. Redis backend 활성 시 LRANGE 우선
// (재시작 후 보존), 실패 시 in-memory fallback.
func (r *AuditRing) List(limit int) []AuditEntry {
	r.mu.RLock()
	rdb := r.redis
	key := r.redisKey
	logger := r.logger
	r.mu.RUnlock()
	if rdb != nil {
		if out, err := r.listRedis(rdb, key, limit); err == nil {
			return out
		} else {
			r.noteRedisFail()
			logger.Warn("audit Redis LRANGE 실패 — in-memory fallback", slog.Any("error", err))
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	n := r.cap
	if !r.full {
		n = r.next
	}
	if n == 0 {
		return nil
	}

	out := make([]AuditEntry, 0, n)
	// 가장 최근 항목부터 거꾸로 — 시작은 next-1 (또는 cap-1 if next==0).
	idx := r.next - 1
	if idx < 0 {
		idx = r.cap - 1
	}
	for i := 0; i < n; i++ {
		out = append(out, r.buf[idx])
		if limit > 0 && len(out) >= limit {
			break
		}
		idx--
		if idx < 0 {
			idx = r.cap - 1
		}
	}
	return out
}

// listRedis — LRANGE 0 (limit-1) 또는 전체 — 시간 역순 그대로 (LPUSH 라 head = 최신).
func (r *AuditRing) listRedis(rdb *redis.Client, key string, limit int) ([]AuditEntry, error) {
	stop := int64(-1)
	if limit > 0 {
		stop = int64(limit) - 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ctx, span := auditTracer.Start(ctx, "redis.audit.list",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "lrange"),
			attribute.String("audit.key", key),
			attribute.Int("audit.limit", limit),
		))
	defer span.End()
	vals, err := rdb.LRange(ctx, key, 0, stop).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("audit.returned", len(vals)))
	out := make([]AuditEntry, 0, len(vals))
	for _, v := range vals {
		var e AuditEntry
		if err := json.Unmarshal([]byte(v), &e); err != nil {
			r.logger.Warn("audit Redis entry 파싱 실패 (skip)", slog.Any("error", err))
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Len 은 현재 저장된 항목 수.
func (r *AuditRing) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return r.cap
	}
	return r.next
}
