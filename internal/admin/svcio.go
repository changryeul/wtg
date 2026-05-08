package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/svcio"
)

// ListSvcIO — GET /v1/admin/svc-io[?q=KEYWORD&max=N]
//
// 등록된 매매 svc 의 가벼운 summary 목록. q 파라미터로 code/name prefix 검색.
func ListSvcIO(reg *svcio.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reg == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled",
				"svc-inc-dir 미설정 — 헤더 인덱스가 비어있음")
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		max := 200
		if v := r.URL.Query().Get("max"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				max = n
			}
		}
		var items []svcio.SvcSummary
		if q != "" {
			items = reg.Search(q, max)
		} else {
			items = reg.List()
			if len(items) > max {
				items = items[:max]
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total": reg.Count(),
			"shown": len(items),
			"q":     q,
			"items": items,
		})
	}
}

// ListSvcIOHeaders — GET /v1/admin/svc-io/headers
//
// 등록된 모든 공통 헤더 (COMHDR / BROADCAST_H / ...) 의 정의 + byte size +
// 필드 트리. UI 가 단일 화면에 펼쳐 보이기 위해 한 번에 반환.
func ListSvcIOHeaders(reg *svcio.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reg == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled",
				"svc-inc-dir 미설정")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"headers": reg.ListHeaders(),
		})
	}
}

// GetSvcIO — GET /v1/admin/svc-io/{code}
//
// 단건 SvcSpec — Input/Output 트리 + Records (named typedef) 메타데이터.
func GetSvcIO(reg *svcio.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reg == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled",
				"svc-inc-dir 미설정")
			return
		}
		code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
		if code == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid", "code 필수")
			return
		}
		spec, ok := reg.Get(code)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not_found", "code 미등록: "+code)
			return
		}
		writeJSON(w, http.StatusOK, spec)
	}
}

// TestWireDeps — TestWireSvc 핸들러의 의존성. server.go 가 채움.
type TestWireDeps struct {
	Registry    *svcio.Registry
	MQ          Caller
	Policy      *policy.Engine
	Audit       *AuditRing
	Hub         *Hub
	CallTimeout time.Duration
	Logger      *slog.Logger
}

// testWireRequest — POST body.
type testWireRequest struct {
	Channel    string                 `json:"channel"`             // "WEB"/"MOB"/"HTS"/"ADM"/"EMP" 또는 빈 값
	Exchange   string                 `json:"exchange,omitempty"`  // 명시 시 spec.code 추론 우선. alias 미사용 직접 호출.
	RoutingKey string                 `json:"routing_key,omitempty"`
	Header     map[string]interface{} `json:"header,omitempty"` // 공통 헤더 (COMHDR 등) 의 필드 → 값. spec.HeaderType 이 비어있으면 무시.
	Input      map[string]interface{} `json:"input"`            // SvcSpec.Input 필드 → 값
}

// testWireResponse — 응답 panel.
type testWireResponse struct {
	Code         string                 `json:"code"`
	Channel      string                 `json:"channel"`
	Exchange     string                 `json:"exchange"`
	RoutingKey   string                 `json:"routing_key"`
	HeaderType   string                 `json:"header_type,omitempty"`
	SentBytes    int                    `json:"sent_bytes"`
	SentHex      string                 `json:"sent_hex"`
	RecvBytes    int                    `json:"recv_bytes"`
	RecvHex      string                 `json:"recv_hex"`
	ParsedHeader map[string]interface{} `json:"parsed_header,omitempty"`
	Parsed       map[string]interface{} `json:"parsed,omitempty"`
	Errn         int                    `json:"errn,omitempty"`
	Errm         string                 `json:"errm,omitempty"`
	DurationMs   int64                  `json:"duration_ms"`
}

// TestWireSvc — POST /v1/admin/svc-io/{code}/test-wire
//
// SvcSpec.Input layout 으로 JSON input 을 wire frame 으로 직렬화 → broker 호출
// → 응답 byte 를 SvcSpec.Output layout 으로 parse 해서 반환.
//
// 정책 엔진은 일반 /v1/tx 와 동일하게 적용 — kill switch / blocked rkeys 등.
// 채널 spoof 는 body 의 channel 또는 X-WTG-Channel 헤더 사용.
//
// 엔드포인트는 *display only* 였던 WIRE 형식에 실제 검증 능력을 부여한다 —
// legacy native client 가 보낼 동일 wire frame 을 broker 가 받음. 클라이언트
// 는 여전히 mci 의 정책/감사 layer 를 거친다.
func TestWireSvc(deps *TestWireDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled",
				"svc-inc-dir 미설정 — 헤더 인덱스가 비어있음")
			return
		}
		if deps.MQ == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "no_broker",
				"broker connection 비활성 (--no-broker?)")
			return
		}

		code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
		if code == "" {
			writeJSONError(w, http.StatusBadRequest, "invalid", "code 필수")
			return
		}
		spec, ok := deps.Registry.Get(code)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not_found", "code 미등록: "+code)
			return
		}

		var req testWireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}

		// 채널 결정: body > 헤더 > 빈값.
		channel := strings.ToUpper(strings.TrimSpace(req.Channel))
		if channel == "" {
			channel = strings.ToUpper(strings.TrimSpace(r.Header.Get(middleware.HeaderEdgeChannel)))
		}

		// exchange / routing_key 결정. 명시 안 하면 code 의 split 추정 (5+나머지).
		exchange, rkey := req.Exchange, req.RoutingKey
		if exchange == "" && rkey == "" {
			exchange, rkey = splitSvcCode(code)
		}

		// 정책 검사 — kill switch / blocked rkeys 등.
		if deps.Policy != nil {
			usid := principalUsid(r)
			d := deps.Policy.Check(policy.Request{
				Usid:       usid,
				Channel:    channel,
				Exchange:   exchange,
				RoutingKey: rkey,
			})
			if !d.Allowed {
				status := http.StatusForbidden
				if d.Reason == policy.ReasonKillSwitch || d.Reason == policy.ReasonMaintenance {
					status = http.StatusServiceUnavailable
				}
				writeJSONError(w, status, d.Reason, d.Message)
				return
			}
		}

		// 1. wire 직렬화 — [HeaderFields(req.Header)][Input(req.Input)].
		// HeaderType 이 비어있는 svc (dev WECHO/TSTSVC 류) 는 raw body 만.
		body, err := svcio.SerializeWithHeader(spec.HeaderFields, req.Header, spec.Input, req.Input)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "serialize_failed", err.Error())
			return
		}

		// 2. broker 호출.
		callTO := deps.CallTimeout
		if callTO <= 0 {
			callTO = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), callTO)
		defer cancel()

		t0 := time.Now()
		ch := mymq.ChannelCode(channel)
		if channel == "" {
			ch = mymq.ChannelWeb
		}
		in := &mymq.FrameInput{
			Func: mymq.FCTran,
			Subc: mymq.SubTranMsg,
			Dirf: mymq.DirForward,
			Keyc: mymq.KeySend,
			Xchg: exchange,
			Rkey: rkey,
			Chan: ch.Bytes(),
			Body: body,
		}
		reply, callErr := deps.MQ.Call(ctx, in)
		dur := time.Since(t0).Milliseconds()

		resp := testWireResponse{
			Code:       code,
			Channel:    channel,
			Exchange:   exchange,
			RoutingKey: rkey,
			HeaderType: spec.HeaderType,
			SentBytes:  len(body),
			SentHex:    hex.EncodeToString(body),
			DurationMs: dur,
		}

		if callErr != nil {
			resp.Errm = callErr.Error()
			writeJSON(w, http.StatusBadGateway, resp)
			return
		}

		// 3. 응답 parse — Output layout 으로.
		respBody := reply.Body
		resp.RecvBytes = len(respBody)
		resp.RecvHex = hex.EncodeToString(respBody)
		resp.Errn = int(reply.Errn)
		resp.Errm = reply.ErrMsg
		if reply.Errn == 0 && (len(spec.Output) > 0 || len(spec.HeaderFields) > 0) {
			// 응답도 [Header][Output] 으로 분리 parse.
			h, parsed, perr := svcio.DeserializeWithHeader(spec.HeaderFields, spec.Output, respBody)
			if perr != nil {
				deps.Logger.WarnContext(ctx, "svcio TestWire 응답 parse 실패",
					slog.String("code", code), slog.Any("err", perr))
			} else {
				resp.ParsedHeader = h
				resp.Parsed = parsed
			}
		}

		// audit + ws push.
		if deps.Audit != nil {
			deps.Audit.Push(AuditEntry{
				At:     time.Now(),
				Action: "SVCIO_TEST_WIRE",
				Usid:   principalUsid(r),
				Attrs: map[string]any{
					"code":     code,
					"channel":  channel,
					"sent":     len(body),
					"recv":     resp.RecvBytes,
					"errn":     resp.Errn,
					"duration": dur,
				},
			})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// ─── 헤더 source 편집 ───────────────────────────────────────────────────────
//
// dev workflow — svc 의 input/output 정의를 UI 에서 직접 편집해서 wire 테스트
// 결과를 즉시 비교. 안전 가드:
//   - dev 디렉터리 (svc-headers) 에서 온 헤더만 편집 가능 (isDevSpec)
//   - 운영 (win/src/inc/trn) 은 read-only — 코드 베이스 / git 무결성 보호
//   - 저장 전 .bak 백업

// EditDeps — source 편집 핸들러 의존성. server.go 가 채움.
type EditDeps struct {
	Registry *svcio.Registry
	Logger   *slog.Logger
	Audit    *AuditRing
}

type sourceResponse struct {
	Code     string `json:"code"`
	Path     string `json:"path"`
	Editable bool   `json:"editable"`
	Reason   string `json:"reason,omitempty"` // editable=false 인 이유
	Content  string `json:"content"`          // UTF-8 (CP949 자동 변환 후)
}

// GetSvcIOSource — GET /v1/admin/svc-io/{code}/source
func GetSvcIOSource(deps *EditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled", "svc-inc-dir 미설정")
			return
		}
		code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
		spec, ok := deps.Registry.Get(code)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not_found", "code 미등록: "+code)
			return
		}
		raw, err := os.ReadFile(spec.SourcePath)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "read_failed", err.Error())
			return
		}
		text, derr := svcio.DecodeKorean(raw)
		if derr != nil {
			text = string(raw) // fallback
		}
		editable, reason := isEditablePath(spec.SourcePath)
		writeJSON(w, http.StatusOK, sourceResponse{
			Code:     spec.Code,
			Path:     spec.SourcePath,
			Editable: editable,
			Reason:   reason,
			Content:  text,
		})
	}
}

type saveRequest struct {
	Content string `json:"content"`
}

// SaveSvcIOSource — PUT /v1/admin/svc-io/{code}/source
//
// 요청: {content: "..."} (UTF-8). 응답: 새로 파싱된 SvcSpec.
// 절차: 검증 (editable) → .bak → 새 내용 쓰기 → ReloadFile (parser) → audit.
func SaveSvcIOSource(deps *EditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled", "svc-inc-dir 미설정")
			return
		}
		code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
		spec, ok := deps.Registry.Get(code)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "not_found", "code 미등록: "+code)
			return
		}
		if editable, reason := isEditablePath(spec.SourcePath); !editable {
			writeJSONError(w, http.StatusForbidden, "read_only", reason)
			return
		}
		var req saveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		// 빈 내용은 거부 (실수로 파일 비우는 사고 방지).
		if strings.TrimSpace(req.Content) == "" {
			writeJSONError(w, http.StatusBadRequest, "empty", "내용이 비어있음")
			return
		}

		// .bak 백업 — 직전 내용 보존. 이미 .bak 가 있으면 덮어쓰기 (가장 최근만 유지).
		bak := spec.SourcePath + ".bak"
		if old, err := os.ReadFile(spec.SourcePath); err == nil {
			_ = os.WriteFile(bak, old, 0o644)
		}

		// 새 내용 쓰기 (UTF-8 — parser 가 자동 감지하므로 OK).
		if err := os.WriteFile(spec.SourcePath, []byte(req.Content), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "write_failed", err.Error())
			return
		}

		// 재파싱 + registry 갱신.
		newSpec, perr := deps.Registry.ReloadFile(spec.SourcePath)
		if perr != nil {
			// 파싱 실패 — 사용자가 syntax 잘못 입력. 파일은 이미 저장됐으나 spec 은
			// 옛 것이 그대로 남음. UI 가 에러 표시 + .bak 복원 안내.
			writeJSONError(w, http.StatusUnprocessableEntity, "parse_failed",
				"저장됐으나 파싱 실패 — "+perr.Error()+" (이전 내용은 "+bak+" 에 보존)")
			return
		}

		if deps.Audit != nil {
			deps.Audit.Push(AuditEntry{
				At:     time.Now(),
				Action: "SVCIO_SAVE_SOURCE",
				Usid:   principalUsid(r),
				Attrs:  map[string]any{"code": code, "path": spec.SourcePath, "bytes": len(req.Content)},
			})
		}
		writeJSON(w, http.StatusOK, newSpec)
	}
}

// isEditablePath — dev 디렉터리 (svc-headers) 의 헤더만 편집 허용.
// 운영 디렉터리 (win/src/inc/trn 등) 는 보호 — dev 도구로 운영 코드 손상 방지.
func isEditablePath(p string) (bool, string) {
	if p == "" {
		return false, "source path 알 수 없음"
	}
	if strings.Contains(p, "svc-headers") {
		return true, ""
	}
	return false, "운영 헤더 디렉터리는 read-only — dev svc-headers/ 만 편집 가능"
}

// splitSvcCode — code 에서 exchange/routing_key 추정.
//   "ECHOSVC_PING" → ("ECHOSVC", "PING")  — underscore split (dev svc 컨벤션)
//   "W1104S01"     → ("W1104",   "S01")    — 5+나머지 (legacy W svc 컨벤션)
//   그 외          → (code,     "")        — 호출자가 명시 권장
func splitSvcCode(code string) (string, string) {
	if i := strings.Index(code, "_"); i > 0 {
		return code[:i], code[i+1:]
	}
	if len(code) >= 8 {
		return code[:5], code[5:]
	}
	return code, ""
}
