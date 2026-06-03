package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/internal/api/transform"
	"github.com/winwaysystems/wtg/pkg/idempotency"
	"github.com/winwaysystems/wtg/pkg/policy"
)

// BulkRequest — 다건 매매 묶음 요청. items 는 1..bulkMaxItems.
type BulkRequest struct {
	Items       []transform.Envelope `json:"items"`
	StopOnError bool                 `json:"stop_on_error,omitempty"`
}

// BulkItemResult — 한 item 의 처리 결과. broker call 도달 여부 / 응답을 status
// + envelope (정상) 또는 error/message (transport-level 실패) 로 표현.
type BulkItemResult struct {
	Status   int                 `json:"status"`
	Envelope *transform.Envelope `json:"envelope,omitempty"`
	Error    string              `json:"error,omitempty"`
	Message  string              `json:"message,omitempty"`
}

// BulkResponse — 전체 응답. items[i] 는 request.items[i] 와 1:1 대응.
//
// stop_on_error=true 로 중간 break 된 경우 나머지는 status=0 + error="not_attempted".
type BulkResponse struct {
	Items []BulkItemResult `json:"items"`
}

// bulkMaxItems — DoS 방어. 운영 권장 50 이하 (Bulk 의 의도는 batch 수십건 단위).
// 더 큰 묶음은 클라이언트가 chunk 로 분할 권장.
const bulkMaxItems = 100

// BulkTransaction 은 다건 매매를 한 HTTP 요청에 묶어 처리한다 (POST /v1/tx/bulk).
//
// 순차 처리 — ckey 멀티플렉싱으로 병렬 호출도 가능하지만 매매 순서가 중요한
// 운영에서 single broker call 단위 ordering 보장. 병렬 호출은 후속 옵션.
//
// 각 item 별 정책 검사 + alias 메트릭. Idempotency-Key 는 bulk 전체 단위
// (전체 body hash) — items 의 일부만 캐시할 수는 없음.
func BulkTransaction(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalRequired(w, r)
		if !ok {
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_body", err.Error())
			return
		}
		var req BulkRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if len(req.Items) == 0 {
			writeError(w, http.StatusBadRequest, "empty_items", "items 비어있음")
			return
		}
		if len(req.Items) > bulkMaxItems {
			writeError(w, http.StatusBadRequest, "too_many_items",
				"items 개수 한도 초과 (chunk 분할 필요)")
			return
		}

		// Idempotency 전체 reservation (bulk 단위) — bulk 의 부분 캐싱은 안 함.
		idemKey, idemActive, handled := reserveIdempotency(w, r, deps, p.Usid, bodyBytes,
			func(bool) { /* bulk 는 합산 메트릭 없음 — alias 메트릭은 item 별 처리 */ })
		if handled {
			return
		}
		var cw *captureWriter
		if idemActive {
			cw = &captureWriter{ResponseWriter: w, status: http.StatusOK}
			w = cw
			defer func() {
				if cw.status >= 500 {
					_ = deps.Idempotency.Rollback(r.Context(), idemKey)
					return
				}
				_ = deps.Idempotency.Commit(r.Context(), idemKey, &idempotency.CachedReply{
					StatusCode: cw.status,
					Body:       append([]byte(nil), cw.body.Bytes()...),
				})
			}()
		}

		// OTel span — bulk 전체.
		bulkCtx, bulkSpan := otel.Tracer("mci-api").Start(r.Context(), "broker.bulk_call",
			trace.WithAttributes(
				attribute.Int("bulk.items", len(req.Items)),
				attribute.Bool("bulk.stop_on_error", req.StopOnError),
				attribute.String("bulk.usid", p.Usid),
			))
		defer bulkSpan.End()

		traceIDHex := middleware.TraceIDHexFromContext(r.Context())
		if traceIDHex == "" {
			traceIDHex = middleware.RequestIDFromContext(r.Context())
		}

		results := make([]BulkItemResult, len(req.Items))
		var firstErr bool
		for i := range req.Items {
			if firstErr && req.StopOnError {
				results[i] = BulkItemResult{Error: "not_attempted",
					Message: "이전 item 실패 + stop_on_error=true"}
				continue
			}
			res := processBulkItem(bulkCtx, deps, p, &req.Items[i], traceIDHex)
			results[i] = res
			if res.Error != "" || (res.Envelope != nil && res.Envelope.Errn != 0) {
				firstErr = true
			}
		}

		writeJSON(w, http.StatusOK, BulkResponse{Items: results})
	}
}

// processBulkItem — 단일 item 의 처리. 정책 → frame build → broker.Call →
// FromReply. transaction.go 의 단일 흐름과 동일한 단계 — 코드 중복 줄이려면
// 추후 공통 헬퍼 가능.
func processBulkItem(ctx context.Context, deps *Deps, p *middleware.Principal,
	env *transform.Envelope, traceIDHex string) BulkItemResult {

	callStart := time.Now()
	recordAlias := func(isErr bool) {
		deps.AliasMetrics.RecordCall(env.Alias, time.Since(callStart), isErr)
	}

	if err := env.ValidateRequest(); err != nil {
		recordAlias(true)
		return BulkItemResult{Status: http.StatusBadRequest, Error: "validation", Message: err.Error()}
	}

	// 정책 검사 — kill switch / 정비창 / 차단 심볼.
	if deps.Policy != nil {
		req := policy.Request{
			Usid: p.Usid, Channel: p.Channel,
			Alias: env.Alias, Exchange: env.Exchange, RoutingKey: env.RoutingKey,
			Symbol: extractSymbol(env.Data),
		}
		if d := deps.Policy.Check(req); !d.Allowed {
			status := http.StatusForbidden
			if d.Reason == policy.ReasonKillSwitch || d.Reason == policy.ReasonMaintenance {
				status = http.StatusServiceUnavailable
			}
			recordAlias(true)
			return BulkItemResult{Status: status, Error: d.Reason, Message: d.Message}
		}
	}

	frame, err := env.BuildFrame(0, p.Usid, traceIDHex, deps.Routes)
	if err != nil {
		recordAlias(true)
		if errors.Is(err, transform.ErrUnknownAlias) {
			return BulkItemResult{Status: http.StatusNotFound, Error: "unknown_alias", Message: err.Error()}
		}
		return BulkItemResult{Status: http.StatusBadRequest, Error: "build_frame", Message: err.Error()}
	}
	if p.Cookie != nil {
		frame.Cookie = p.Cookie
	}

	callCtx, cancel := context.WithTimeout(ctx, deps.CallTimeout)
	defer cancel()
	itemCtx, span := otel.Tracer("mci-api").Start(callCtx, "broker.call",
		trace.WithAttributes(
			attribute.String("broker.xchg", env.Exchange),
			attribute.String("broker.rkey", env.RoutingKey),
			attribute.String("broker.usid", p.Usid),
			attribute.String("bulk.alias", env.Alias),
		))
	reply, err := deps.MQ.Call(itemCtx, frame)
	if err != nil {
		span.RecordError(err)
	}
	span.End()

	if err != nil {
		deps.Logger.WarnContext(ctx, "bulk item broker Call 실패",
			slog.String("usid", p.Usid),
			slog.String("alias", env.Alias),
			slog.String("rkey", env.RoutingKey),
			slog.Any("error", err))
		status, code, msg := mapBrokerError(err)
		recordAlias(true)
		return BulkItemResult{Status: status, Error: code, Message: msg}
	}

	// 비즈니스 에러 (errn != 0): envelope 그대로 + status 422.
	if mqErr := reply.AsError(); mqErr != nil {
		status, _, _ := mapBrokerError(mqErr)
		recordAlias(true)
		return BulkItemResult{Status: status, Envelope: transform.FromReply(reply)}
	}

	recordAlias(false)
	return BulkItemResult{Status: http.StatusOK, Envelope: transform.FromReply(reply)}
}
