package quoteid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// markConsumedScript — MarkConsumed 를 단일 round-trip atomic 으로 처리.
//
// 기존 (v1.5 까지): GET q + SET NX c — 두 RTT, 그 사이에 race window
// (record 가 expire 되었으나 SETNX 만 성공할 수 있음).
//
// v1.7+ (Lua): GET q → cjson.decode → ValidUntil 검사 → SET NX c 가 한
// Lua 실행 안에서 결정. Redis 단일 스레드라 다른 명령과 인터리브 없음.
// 모든 키가 hash tag `{<id>}` 안에 있어 Cluster slot 동일 — EVAL 가능.
//
// 반환 (table):
//
//	{1, record_json}              — ConsumeOK (지금 막 표시함)
//	{2, record_json, prev_consumer} — ConsumeAlreadyDone
//	{3}                            — ConsumeNotFound
//	{4, record_json}               — ConsumeExpired
var markConsumedScript = redis.NewScript(`
local rec_json = redis.call("GET", KEYS[1])
if not rec_json then
  return {3}
end
local rec = cjson.decode(rec_json)
local valid_until = tonumber(rec["valid_until_unix_nano"])
local now = tonumber(ARGV[2])
if not valid_until or now >= valid_until then
  return {4, rec_json}
end
local ok = redis.call("SET", KEYS[2], ARGV[1], "EX", ARGV[3], "NX")
if ok then
  return {1, rec_json}
end
local prev = redis.call("GET", KEYS[2])
if not prev then prev = "" end
return {2, rec_json, prev}
`)

// RedisRegistry 는 Redis 기반 Registry — 운영 active-active mci-price 가
// 공유. 두 인스턴스가 발급한 QuoteID 가 동일 Redis 에 누적되어 매칭 엔진의
// 단일 검증 source 가 된다.
//
// 키 설계:
//
//   - "<prefix>:q:<quoteid>"  — Record JSON, TTL = (ValidUntil - now) + grace
//
// TTL 이 지나면 Redis 가 자동 삭제 → Get → redis.Nil → ErrNotFound. Memory
// 구현과 동일한 contract.
type RedisRegistry struct {
	rdb    redis.UniversalClient
	prefix string
	grace  time.Duration
	now    func() time.Time
}

// RedisRegistryOptions 는 RedisRegistry 생성 옵션.
type RedisRegistryOptions struct {
	// Prefix — 키 namespace (default "wtg:quoteid").
	Prefix string
	// Grace — ValidUntil 이후에도 record 를 유지하는 시간. last-look 시간 +
	// 네트워크 지연 + clock skew 여유. 0 이면 ValidUntil 도래 즉시 만료.
	Grace time.Duration
	// Now — 테스트용 시간 주입. nil 이면 time.Now.
	Now func() time.Time
}

// NewRedisRegistry — 호출자가 만든 UniversalClient 를 그대로 받는다.
// Close 는 호출자가 관리 (RedisStore 와 동일 컨벤션).
func NewRedisRegistry(rdb redis.UniversalClient, opt RedisRegistryOptions) *RedisRegistry {
	if opt.Prefix == "" {
		opt.Prefix = "wtg:quoteid"
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &RedisRegistry{
		rdb:    rdb,
		prefix: opt.Prefix,
		grace:  opt.Grace,
		now:    opt.Now,
	}
}

// Redis 키 형식 — hash tag `{<id>}` 안에 QuoteID 를 담아 두 키가 cluster
// 의 same slot 으로 라우팅되도록 한다. Standalone/Sentinel 에서는 hash tag
// 가 의미 없지만 결과 같음 — 안전한 미래 호환.
//
//	<prefix>:{<id>}:q  — record JSON
//	<prefix>:{<id>}:c  — consumer_id (MarkConsumed)
//
// 두 키가 same slot 이므로 향후 Lua script 로 multi-key atomic 가능.

func (r *RedisRegistry) key(id QuoteID) string {
	return r.prefix + ":{" + string(id) + "}:q"
}

// consumedKey — MarkConsumed 표시 키. SET NX 로 원자적 first-writer-wins.
func (r *RedisRegistry) consumedKey(id QuoteID) string {
	return r.prefix + ":{" + string(id) + "}:c"
}

func (r *RedisRegistry) Put(ctx context.Context, rec Record) error {
	if rec.ValidUntil <= rec.IssuedAt {
		return ErrInvalidRecord
	}
	if rec.QuoteID == "" {
		return ErrInvalidRecord
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("quoteid: marshal: %w", err)
	}
	// TTL 계산: (ValidUntil - now) + grace. 음수가 될 수 있는 경계 케이스 방어.
	remaining := time.Unix(0, rec.ValidUntil).Sub(r.now()) + r.grace
	if remaining <= 0 {
		// 이미 만료된 record — 등록하지 않음 (Put 후 즉시 Get → Nil 이 normal).
		return nil
	}
	return r.rdb.Set(ctx, r.key(rec.QuoteID), body, remaining).Err()
}

func (r *RedisRegistry) Get(ctx context.Context, id QuoteID) (Record, error) {
	body, err := r.rdb.Get(ctx, r.key(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("quoteid: redis get: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return Record{}, fmt.Errorf("quoteid: unmarshal: %w", err)
	}
	return rec, nil
}

// Consumed — read-only 조회. consumedKey 가 존재하면 그 value (consumer) 반환.
func (r *RedisRegistry) Consumed(ctx context.Context, id QuoteID) (string, bool, error) {
	v, err := r.rdb.Get(ctx, r.consumedKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("quoteid: redis get consumed: %w", err)
	}
	return v, true, nil
}

// MarkConsumed — Lua script 로 단일 round-trip atomic. v1.7 도입.
//
// 기존 GET + SETNX 는 사이에 race window 가 있어 record 가 grace-edge 시점
// 에 만료될 때 SETNX 만 성공할 수 있었음 (consumed=true 인데 record 는
// EXPIRED — 정합성 깨짐). markConsumedScript 가 GET → ValidUntil 검사 →
// SETNX 를 한 Lua 실행 안에서 처리해 인터리브 차단.
//
// Cluster 호환: 모든 키가 hash tag `{<id>}` 안에 있어 same slot 보장 — EVAL
// 가능. ClusterClient 가 자동 routing.
func (r *RedisRegistry) MarkConsumed(ctx context.Context, id QuoteID, consumerID string) (ConsumeResult, error) {
	// TTL — script 가 SET EX 에 쓰는 seconds 단위. 보존 윈도우 (ValidUntil
	// 까지 남은 + grace). 음수 / 0 은 안 됨 (Redis 가 reject).
	now := r.now()
	// ValidUntil 은 record 안에 있어 script 가 본다. 그래도 클라이언트가
	// 미리 정한 grace 를 SETNX TTL 로 써야 record GC 후 한참 뒤 SETNX 가
	// 남는 일을 막는다. 안전한 상한 = grace + 1s (validity 평균 cap).
	ttl := r.grace + time.Second
	if ttl <= 0 {
		ttl = time.Second
	}
	ttlSec := int64(ttl / time.Second)
	if ttlSec < 1 {
		ttlSec = 1
	}
	raw, err := markConsumedScript.Run(ctx, r.rdb,
		[]string{r.key(id), r.consumedKey(id)},
		consumerID,
		strconv.FormatInt(now.UnixNano(), 10),
		strconv.FormatInt(ttlSec, 10),
	).Result()
	if err != nil {
		return ConsumeResult{}, fmt.Errorf("quoteid: eval markConsumed: %w", err)
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return ConsumeResult{}, fmt.Errorf("quoteid: script 반환 형식 mismatch: %T", raw)
	}
	code, _ := arr[0].(int64)
	switch code {
	case 1: // ConsumeOK
		rec, perr := decodeRecordFromLua(arr, 1)
		if perr != nil {
			return ConsumeResult{}, perr
		}
		return ConsumeResult{Status: ConsumeOK, Record: rec}, nil
	case 2: // ConsumeAlreadyDone
		rec, perr := decodeRecordFromLua(arr, 1)
		if perr != nil {
			return ConsumeResult{}, perr
		}
		prev := ""
		if len(arr) > 2 {
			prev, _ = arr[2].(string)
		}
		return ConsumeResult{Status: ConsumeAlreadyDone, Record: rec, ConsumedBy: prev}, nil
	case 3: // ConsumeNotFound
		return ConsumeResult{Status: ConsumeNotFound}, nil
	case 4: // ConsumeExpired
		rec, perr := decodeRecordFromLua(arr, 1)
		if perr != nil {
			return ConsumeResult{}, perr
		}
		return ConsumeResult{Status: ConsumeExpired, Record: rec}, nil
	default:
		return ConsumeResult{}, fmt.Errorf("quoteid: 알 수 없는 script code %d", code)
	}
}

// lookupScript — record + consumed 를 atomic 으로 조회.
//
// Validate 의 기존 2 RTT (GET q + GET c) 를 1 RTT 로 압축. 두 키 사이에
// race window 도 차단 (이론적으로 record TTL 만료 + consumed 만 남은 짧은
// 순간이 가능했음).
//
// 반환:
//
//	{0}                              — record 없음, consumed 없음
//	{1, rec_json}                    — record 있음, consumed 없음
//	{2, rec_json, consumer}          — record 있음, consumed 있음
//	{3, "", consumer}                — record 없음 (GC), consumed marker 남음
var lookupScript = redis.NewScript(`
local rec = redis.call("GET", KEYS[1])
local c   = redis.call("GET", KEYS[2])
if not rec and not c then return {0} end
if not rec and c       then return {3, "", c} end
if rec and not c       then return {1, rec} end
return {2, rec, c}
`)

// Lookup — record + consumed atomic 조회.
func (r *RedisRegistry) Lookup(ctx context.Context, id QuoteID) (LookupResult, error) {
	raw, err := lookupScript.Run(ctx, r.rdb,
		[]string{r.key(id), r.consumedKey(id)},
	).Result()
	if err != nil {
		return LookupResult{}, fmt.Errorf("quoteid: lookup: %w", err)
	}
	return decodeLookup(raw)
}

// LookupMany — pipeline 으로 N Lookup 묶음 송신.
//
// Note: pipeline 안에서는 Script.Run 의 EVALSHA→EVAL fallback 이 동작하지
// 않는다 (Run 이 동기적으로 NOSCRIPT 를 못 봄). 따라서 Eval 직접 사용 —
// 매 호출 EVAL source 송신으로 약간 더 길지만 batch 안에서 일관 동작.
func (r *RedisRegistry) LookupMany(ctx context.Context, ids []QuoteID) ([]LookupResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(ids))
	for i, id := range ids {
		cmds[i] = lookupScript.Eval(ctx, pipe,
			[]string{r.key(id), r.consumedKey(id)},
		)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// per-cmd 오류는 아래 루프 처리.
		_ = err
	}
	out := make([]LookupResult, len(ids))
	for i, cmd := range cmds {
		raw, err := cmd.Result()
		if err != nil {
			// 미완성/장애 — Found=false 로 표시. Caller 가 후속 retry 가능.
			out[i] = LookupResult{}
			continue
		}
		lr, derr := decodeLookup(raw)
		if derr != nil {
			out[i] = LookupResult{}
			continue
		}
		out[i] = lr
	}
	return out, nil
}

// decodeLookup — lookupScript 반환을 LookupResult 로.
func decodeLookup(raw any) (LookupResult, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return LookupResult{}, fmt.Errorf("quoteid: lookup 반환 형식 mismatch: %T", raw)
	}
	code, _ := arr[0].(int64)
	switch code {
	case 0:
		return LookupResult{}, nil
	case 1:
		rec, err := decodeRecordFromLua(arr, 1)
		if err != nil {
			return LookupResult{}, err
		}
		return LookupResult{Found: true, Record: rec}, nil
	case 2:
		rec, err := decodeRecordFromLua(arr, 1)
		if err != nil {
			return LookupResult{}, err
		}
		prev := ""
		if len(arr) > 2 {
			prev, _ = arr[2].(string)
		}
		return LookupResult{Found: true, Record: rec, Consumed: true, ConsumedBy: prev}, nil
	case 3:
		prev := ""
		if len(arr) > 2 {
			prev, _ = arr[2].(string)
		}
		return LookupResult{Found: false, Consumed: true, ConsumedBy: prev}, nil
	default:
		return LookupResult{}, fmt.Errorf("quoteid: 알 수 없는 lookup code %d", code)
	}
}

// MarkConsumedMany — 파이프라인으로 N EvalSha 묶음 송신. 1 connection grab,
// 1 RTT (direct/sentinel). Cluster 에서는 slot 별 자동 라우팅으로 슬롯당
// 1 RTT — 여전히 N 개별 호출보다 빠름.
//
// 각 항목은 같은 Lua script (markConsumedScript) 를 사용 — per-item atomic
// 보장은 v1.7 의 단일 호출 흐름과 동일. batch 자체는 not transaction —
// 일부 OK 일부 EXPIRED 혼재 가능.
func (r *RedisRegistry) MarkConsumedMany(ctx context.Context, reqs []ConsumeRequest) ([]ConsumeResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	now := r.now()
	ttl := r.grace + time.Second
	if ttl <= 0 {
		ttl = time.Second
	}
	ttlSec := int64(ttl / time.Second)
	if ttlSec < 1 {
		ttlSec = 1
	}
	nowNanoStr := strconv.FormatInt(now.UnixNano(), 10)
	ttlStr := strconv.FormatInt(ttlSec, 10)

	// 파이프라인으로 묶어 송신. ClusterClient 의 경우 slot 별로 자동 split.
	// pipeline 에서는 EVALSHA→EVAL fallback 불가 — Eval 직접 사용 (LookupMany 와 동일).
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(reqs))
	for i, req := range reqs {
		cmds[i] = markConsumedScript.Eval(ctx, pipe,
			[]string{r.key(req.QuoteID), r.consumedKey(req.QuoteID)},
			req.ConsumerID, nowNanoStr, ttlStr,
		)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Pipeline.Exec 는 일부만 실패해도 error 반환. 개별 cmd.Err 을 검사해서
		// 가능한 결과는 채워준다.
		// continue — 아래 루프가 per-cmd 에러 처리.
		_ = err
	}

	out := make([]ConsumeResult, len(reqs))
	for i, cmd := range cmds {
		raw, err := cmd.Result()
		if err != nil {
			out[i] = ConsumeResult{Status: ConsumeStatusUnknown}
			continue
		}
		arr, ok := raw.([]any)
		if !ok || len(arr) == 0 {
			out[i] = ConsumeResult{Status: ConsumeStatusUnknown}
			continue
		}
		code, _ := arr[0].(int64)
		switch code {
		case 1:
			rec, perr := decodeRecordFromLua(arr, 1)
			if perr != nil {
				out[i] = ConsumeResult{Status: ConsumeStatusUnknown}
				continue
			}
			out[i] = ConsumeResult{Status: ConsumeOK, Record: rec}
		case 2:
			rec, perr := decodeRecordFromLua(arr, 1)
			if perr != nil {
				out[i] = ConsumeResult{Status: ConsumeStatusUnknown}
				continue
			}
			prev := ""
			if len(arr) > 2 {
				prev, _ = arr[2].(string)
			}
			out[i] = ConsumeResult{Status: ConsumeAlreadyDone, Record: rec, ConsumedBy: prev}
		case 3:
			out[i] = ConsumeResult{Status: ConsumeNotFound}
		case 4:
			rec, _ := decodeRecordFromLua(arr, 1)
			out[i] = ConsumeResult{Status: ConsumeExpired, Record: rec}
		default:
			out[i] = ConsumeResult{Status: ConsumeStatusUnknown}
		}
	}
	return out, nil
}

// decodeRecordFromLua — Lua 반환 배열의 idx 위치에서 record JSON 추출 + unmarshal.
func decodeRecordFromLua(arr []any, idx int) (Record, error) {
	if len(arr) <= idx {
		return Record{}, fmt.Errorf("quoteid: script 결과에 record 누락")
	}
	s, ok := arr[idx].(string)
	if !ok {
		return Record{}, fmt.Errorf("quoteid: script record 타입 mismatch: %T", arr[idx])
	}
	var rec Record
	if err := json.Unmarshal([]byte(s), &rec); err != nil {
		return Record{}, fmt.Errorf("quoteid: script record unmarshal: %w", err)
	}
	return rec, nil
}
