// QuoteValidation HTTP gateway — gRPC 의 wtg.v1.QuoteValidationService 를
// JSON REST 로 미러. 비-Go FIX gateway (Quickfix-CPP / OnixS / Java) 호환 +
// 운영 도구 (curl / Postman) 디버깅 용이.
//
// 라우트:
//
//	POST /v1/quoteid/validate             — wtgpb.ValidateRequest             → ValidateResponse
//	POST /v1/quoteid/batch-validate       — wtgpb.BatchValidateRequest        → BatchValidateResponse
//	POST /v1/quoteid/mark-consumed        — wtgpb.MarkConsumedRequest         → MarkConsumedResponse
//	POST /v1/quoteid/batch-mark-consumed  — wtgpb.BatchMarkConsumedRequest    → BatchMarkConsumedResponse
//	GET  /v1/quoteid/stats                — QuoteValidationStats
//
// wire 는 protojson — proto-defined 필드명 (camelCase 변환). gRPC 와 동일한
// 핸들러 / 카운터 / Registry 공유.
package price

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	wtgpb "github.com/winwaysystems/wtg/pkg/wtgpb/v1"
)

// grpcErrToHTTP — gRPC status code 를 HTTP status code 로 매핑.
// 핸들러가 inner srv 호출의 err 를 받았을 때 적절한 HTTP status 결정.
func grpcErrToHTTP(err error) int {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError
	}
	switch st.Code() {
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// HTTPMaxBodyBytes — body 최대 크기. BatchValidate 1000건 × ~30byte ≈ 30KB
// 정도지만 protojson + engine_id 등 포함 여유 잡아 256KB.
const HTTPMaxBodyBytes = 256 * 1024

// RegisterQuoteValidationHTTP — mux 에 8개 라우트 등록 (S3-c 에서 swap 2개 추가).
func RegisterQuoteValidationHTTP(mux *http.ServeMux, srv *QuoteValidationServer, logger *slog.Logger) {
	h := &quoteValidationHTTP{srv: srv, logger: logger}
	mux.HandleFunc("POST /v1/quoteid/validate", h.handleValidate)
	mux.HandleFunc("POST /v1/quoteid/batch-validate", h.handleBatchValidate)
	mux.HandleFunc("POST /v1/quoteid/mark-consumed", h.handleMarkConsumed)
	mux.HandleFunc("POST /v1/quoteid/batch-mark-consumed", h.handleBatchMarkConsumed)
	mux.HandleFunc("POST /v1/quoteid/validate-swap", h.handleValidateSwap)
	mux.HandleFunc("POST /v1/quoteid/consume-swap", h.handleConsumeSwap)
	mux.HandleFunc("GET /v1/quoteid/stats", h.handleStats)
	mux.HandleFunc("GET /v1/quoteid/lookup", h.handleLookup)
}

type quoteValidationHTTP struct {
	srv    *QuoteValidationServer
	logger *slog.Logger
}

// readProtoJSON — body 읽고 protojson 으로 unmarshal. 표준 protojson 옵션
// (camelCase 허용, 알 수 없는 필드 무시).
func (h *quoteValidationHTTP) readProtoJSON(r *http.Request, dst proto.Message) error {
	r.Body = http.MaxBytesReader(nil, r.Body, HTTPMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	return opts.Unmarshal(body, dst)
}

// writeProtoJSON — protojson 응답. EmitDefaultValues 로 ord_rej_reason=0
// 같은 zero value 도 명시. EmitUnpopulated 가 v1.34+ deprecated name 이라
// EmitDefaultValues 로 안전.
func (h *quoteValidationHTTP) writeProtoJSON(w http.ResponseWriter, code int, msg proto.Message) {
	opts := protojson.MarshalOptions{EmitDefaultValues: true}
	body, err := opts.Marshal(msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (h *quoteValidationHTTP) writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *quoteValidationHTTP) handleValidate(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.ValidateRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.Validate(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleBatchValidate(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.BatchValidateRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.BatchValidate(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleMarkConsumed(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.MarkConsumedRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.MarkConsumed(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleBatchMarkConsumed(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.BatchMarkConsumedRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.BatchMarkConsumed(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleValidateSwap(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.ValidateSwapRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.ValidateSwap(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleConsumeSwap(w http.ResponseWriter, r *http.Request) {
	req := &wtgpb.ConsumeSwapRequest{}
	if err := h.readProtoJSON(r, req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.srv.ConsumeSwap(r.Context(), req)
	if err != nil {
		h.writeError(w, grpcErrToHTTP(err), err.Error())
		return
	}
	h.writeProtoJSON(w, http.StatusOK, resp)
}

func (h *quoteValidationHTTP) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.srv.Stats())
}

// handleLookup — GET /v1/quoteid/lookup?id=...
//
// 운영 read-only 조회 — Validate 의 RBAC / consumed-marking side-effect 없음.
// 응답: {id, found, record(if found), consumed, consumed_by}. 분쟁 / spike 분석.
//
// Registry.Lookup 은 not-found 를 Found=false 로 표현 (err=nil) — err != nil
// 은 store 자체 실패 (Redis 끊김 등) 만 의미.
func (h *quoteValidationHTTP) handleLookup(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, `{"error":"id 필요"}`, http.StatusBadRequest)
		return
	}
	lr, err := h.srv.LookupReadonly(r.Context(), id)
	if err != nil {
		h.logger.Warn("quoteid lookup 실패",
			slog.String("id", id), slog.Any("err", err))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "store_error", "message": err.Error(),
		})
		return
	}
	out := map[string]any{
		"id":          id,
		"found":       lr.Found,
		"consumed":    lr.Consumed,
		"consumed_by": lr.ConsumedBy,
	}
	if lr.Found {
		out["record"] = lr.Record
	}
	writeJSON(w, http.StatusOK, out)
}
