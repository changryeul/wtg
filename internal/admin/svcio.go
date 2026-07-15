package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/routing"
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
		sortMode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort"))) // "" | "recent"
		max := 200
		if v := r.URL.Query().Get("max"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				max = n
			}
		}
		var items []svcio.SvcSummary
		if q != "" {
			// Search 가 max 까지 자체 컷. 정렬은 후 단계에서 적용.
			items = reg.Search(q, max)
		} else {
			items = reg.List()
		}
		// 정렬 — recent 는 mtime desc 우선, 같은 mtime 은 code 순 (List 기본).
		if sortMode == "recent" {
			svcioSortRecent(items)
		}
		if len(items) > max {
			items = items[:max]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total": reg.Count(),
			"shown": len(items),
			"q":     q,
			"sort":  sortMode,
			"items": items,
		})
	}
}

// svcioSortRecent — mtime desc 우선 + 같은 mtime 은 code asc (안정 정렬).
// mtime 0 (stat 실패) 는 가장 아래로.
func svcioSortRecent(items []svcio.SvcSummary) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].SourceModUnix, items[j].SourceModUnix
		if a != b {
			return a > b
		}
		return items[i].Code < items[j].Code
	})
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

// SvcRuntimeStats — 운영 호출 통계. mci-api 의 alias-stats 를 svc code 단위로
// 누계한 결과. 페이지 진입 시 한 번 조회 + UI 캐시.
type SvcRuntimeStats struct {
	Aliases        []string `json:"aliases"`         // 이 svc code 로 라우팅되는 alias 목록 (routing_key 매칭)
	Calls          int64    `json:"calls"`           // 누적 호출수
	Errors         int64    `json:"errors"`          // 누적 에러
	AvgLatencyMs   float64  `json:"avg_latency_ms"`  // 가중 평균 (calls × avg)
	MaxLatencyMs   float64  `json:"max_latency_ms"`  // 모든 alias 중 최대
	ErrorRatePct   float64  `json:"error_rate_pct"`  // errors/calls × 100
	LastCallUnix   int64    `json:"last_call_unix"`  // 모든 alias 중 최근 호출 시각
	StatsAvailable bool     `json:"stats_available"` // false 면 mci-api 미접속 — UI 가 grey out
}

// aliasStatsResponse — mci-api 의 /v1/admin/alias-stats 응답.
type aliasStatsResponse struct {
	Aliases []struct {
		Alias        string  `json:"alias"`
		Tier         string  `json:"tier"`
		Calls        int64   `json:"calls"`
		Errors       int64   `json:"errors"`
		AvgLatencyMs float64 `json:"avg_latency_ms"`
		MaxLatencyMs float64 `json:"max_latency_ms"`
		ErrorRatePct float64 `json:"error_rate_pct"`
		LastCallUnix int64   `json:"last_call_unix"`
	} `json:"aliases"`
}

// SvcIOWithStats — GetSvcIO 응답 wrapper. 기존 SvcSpec 필드 inline + runtime_stats 추가.
type SvcIOWithStats struct {
	*svcio.SvcSpec
	RuntimeStats *SvcRuntimeStats `json:"runtime_stats,omitempty"`
}

// SvcIODeps — GetSvcIO 의 의존성. server.go 가 채움.
type SvcIODeps struct {
	Registry       *svcio.Registry
	Routes         routing.Registry // svc code → alias 들 reverse lookup
	UpstreamAPIURL string           // mci-api /v1/admin/alias-stats fetch
	OpenAPIServer  string           // OpenAPI servers[].url 명시값 (외부 API 게이트웨이). 비면 요청 origin
	DevMode        bool             // DevMode 면 관리자 콘솔 same-origin 테스트 서버를 servers[] 에 추가
	HTTPClient     *http.Client     // 짧은 timeout, nil 이면 default 사용
	Logger         *slog.Logger
}

// fetchAliasStats — mci-api 의 alias-stats 를 fetch.
// 1s timeout, 실패 시 nil 반환 (panel 자체를 hide).
func (d *SvcIODeps) fetchAliasStats(ctx context.Context) *aliasStatsResponse {
	if d.UpstreamAPIURL == "" {
		return nil
	}
	cli := d.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 1 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(d.UpstreamAPIURL, "/")+"/v1/admin/alias-stats", nil)
	if err != nil {
		return nil
	}
	// DevMode handshake — admin 가 mci-api 내부 endpoint 호출.
	req.Header.Set("X-WTG-User", "admin")
	resp, err := cli.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out aliasStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return &out
}

// computeRuntimeStats — routes 에서 routing_key=code 인 alias 들을 추출 + alias-stats
// 누계. 매칭 alias 0건이거나 stats 없으면 (Aliases=[], Calls=0, StatsAvailable=true)
// 반환 — UI 가 "운영 호출 없음" 표시.
func (d *SvcIODeps) computeRuntimeStats(ctx context.Context, code string) *SvcRuntimeStats {
	stats := &SvcRuntimeStats{Aliases: []string{}, StatsAvailable: false}
	// 1. routes 에서 routing_key == code 인 alias 들 lookup.
	if d.Routes != nil {
		for _, rule := range d.Routes.List() {
			if !rule.Active {
				continue
			}
			if strings.EqualFold(rule.RoutingKey, code) {
				stats.Aliases = append(stats.Aliases, rule.Alias)
			}
		}
	}
	// 2. mci-api alias-stats fetch.
	resp := d.fetchAliasStats(ctx)
	if resp == nil {
		// stats source 미동작 — Aliases 만 채워서 반환 (UI 가 "통계 미접속" 표시).
		return stats
	}
	stats.StatsAvailable = true
	aliasSet := make(map[string]bool, len(stats.Aliases))
	for _, a := range stats.Aliases {
		aliasSet[strings.ToLower(a)] = true
	}
	// 3. 누계 — alias-stats 의 entry 가 alias × tier 분리이므로 동일 alias 의 여러 tier 합산.
	var totalLatencyMs float64
	for _, e := range resp.Aliases {
		if !aliasSet[strings.ToLower(e.Alias)] {
			continue
		}
		stats.Calls += e.Calls
		stats.Errors += e.Errors
		totalLatencyMs += e.AvgLatencyMs * float64(e.Calls) // 가중합
		if e.MaxLatencyMs > stats.MaxLatencyMs {
			stats.MaxLatencyMs = e.MaxLatencyMs
		}
		if e.LastCallUnix > stats.LastCallUnix {
			stats.LastCallUnix = e.LastCallUnix
		}
	}
	if stats.Calls > 0 {
		stats.AvgLatencyMs = totalLatencyMs / float64(stats.Calls)
		stats.ErrorRatePct = float64(stats.Errors) / float64(stats.Calls) * 100
	}
	return stats
}

// GetSvcIO — GET /v1/admin/svc-io/{code}
//
// 단건 SvcSpec — Input/Output 트리 + Records (named typedef) 메타데이터.
func GetSvcIO(deps *SvcIODeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled",
				"svc-inc-dir 미설정")
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
		// runtime stats join — routes 의 routing_key 매칭 alias 들 + alias-stats 누계.
		// fetch 실패해도 spec 자체는 반환 (StatsAvailable=false 만 다름).
		stats := deps.computeRuntimeStats(r.Context(), code)
		writeJSON(w, http.StatusOK, SvcIOWithStats{SvcSpec: spec, RuntimeStats: stats})
	}
}

// TestWireDeps — TestWireSvc 핸들러의 의존성. server.go 가 채움.
type TestWireDeps struct {
	Registry    *svcio.Registry
	Routes      routing.Registry // svc code → exchange/routing_key alias lookup
	MQ          Caller
	Policy      *policy.Engine
	Audit       *AuditRing
	Hub         *Hub
	CallTimeout time.Duration
	Logger      *slog.Logger
}

// testWireRequest — POST body.
type testWireRequest struct {
	Channel    string                 `json:"channel"`            // "WEB"/"MOB"/"HTS"/"ADM"/"EMP" 또는 빈 값
	Exchange   string                 `json:"exchange,omitempty"` // 명시 시 spec.code 추론 우선. alias 미사용 직접 호출.
	RoutingKey string                 `json:"routing_key,omitempty"`
	Header     map[string]interface{} `json:"header,omitempty"`  // 공통 헤더 (COMHDR 등) 의 필드 → 값. spec.HeaderType 이 비어있으면 무시.
	Input      map[string]interface{} `json:"input"`             // SvcSpec.Input 필드 → 값
	DryRun     bool                   `json:"dry_run,omitempty"` // true 면 wire 직렬화만 + broker 호출 skip. UI 의 ▶ 미리보기 용.
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

		// exchange / routing_key 결정 — 우선순위:
		//   1. 요청 본문의 명시적 값 (alias 우회 + 직접 broker 호출)
		//   2. routing.Registry 의 alias = code 매핑
		//   3. 그 외 — 422. 잘못된 휴리스틱(splitSvcCode 의 5+나머지) 으로 broker
		//      에 잘못 라우팅된 transaction 을 보내는 사고 방지.
		exchange, rkey := req.Exchange, req.RoutingKey
		if exchange == "" && rkey == "" {
			if deps.Routes == nil {
				writeJSONError(w, http.StatusUnprocessableEntity, "no_routing",
					"라우팅 미결정 — routing.Registry 미설정. 요청 본문에 exchange/routing_key 명시 필요")
				return
			}
			rule, err := routing.Resolve(deps.Routes, code)
			if err != nil {
				writeJSONError(w, http.StatusUnprocessableEntity, "unknown_alias",
					"라우팅 미결정 — alias 미등록: "+code+". /v1/admin/routes 로 등록하거나 요청 본문에 exchange/routing_key 명시 필요. 진짜 매핑은 /v1/admin/whois?argv1="+code+" 로 broker 에 직접 조회 가능")
				return
			}
			exchange, rkey = rule.Exchange, rule.RoutingKey
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

		// dry_run — broker 호출 skip, wire hex 만 응답.
		// UI 의 ▶ 미리보기 버튼이 form 입력 변경 후 broker 부담 없이 wire layout
		// 검증 가능. 운영 정책 검사 (kill switch 등) 는 이미 통과 — 운영자가 dry_run
		// 만 반복해 form 을 다듬은 후 정식 ▶ 전송 으로 broker 호출.
		if req.DryRun {
			writeJSON(w, http.StatusOK, testWireResponse{
				Code:       code,
				Channel:    channel,
				Exchange:   exchange,
				RoutingKey: rkey,
				HeaderType: spec.HeaderType,
				SentBytes:  len(body),
				SentHex:    hex.EncodeToString(body),
				DurationMs: 0,
			})
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
				At:       time.Now(),
				Action:   "SVCIO_TEST_WIRE",
				Resource: "svcio",
				Usid:     principalUsid(r),
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
				At:       time.Now(),
				Action:   "SVCIO_SAVE_SOURCE",
				Resource: "svcio",
				Usid:     principalUsid(r),
				Attrs:    map[string]any{"code": code, "path": spec.SourcePath, "bytes": len(req.Content)},
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

// splitSvcCode 휴리스틱은 제거됨 — broker 의 진짜 매핑과 자주 안 맞아서
// 잘못 라우팅된 transaction 이 도달하는 사고 발생 (예: W1101S01 의 진짜 매핑은
// xchg=ECHOSVC/rkey=W1101S01 인데 휴리스틱은 xchg=W1101/rkey=S01 로 split).
//
// 대신 TestWireSvc 가 다음 순서로 결정:
//   1. 요청 본문의 명시적 exchange/routing_key
//   2. routing.Registry 의 alias = code 매핑
//   3. 그 외 422 — 운영자가 alias 를 명시 등록해야 함.
//
// 진짜 매핑을 broker 에 직접 물어보는 도구는 scripts/import-svc-routes.sh 참조.

// aliasResolver 는 routing registry 로 code(routing_key) → alias reverse
// lookup 함수를 만든다. 매칭 룰이 없으면 code 자체를 alias 로 (fallback).
func aliasResolver(routes routing.Registry) func(string) string {
	if routes == nil {
		return func(code string) string { return code }
	}
	// code → alias 맵 1회 구성 (List 는 로컬 캐시라 저렴).
	byCode := map[string]string{}
	for _, ru := range routes.List() {
		if ru == nil || ru.RoutingKey == "" {
			continue
		}
		if _, dup := byCode[ru.RoutingKey]; !dup {
			byCode[ru.RoutingKey] = ru.Alias
		}
	}
	return func(code string) string {
		if a, ok := byCode[code]; ok {
			return a
		}
		return code
	}
}

// GetSvcIOOpenAPI — GET /v1/admin/svc-io/openapi.json[?codes=W35*][&download=1]
// svcio Registry 전체(또는 codes 필터)를 OpenAPI 3.0 문서로 생성해 응답.
// 클라이언트 개발자 전달용 — Swagger UI / Postman / codegen 소비 가능.
func GetSvcIOOpenAPI(deps *SvcIODeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps == nil || deps.Registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "svcio_disabled", "svc-inc-dir 미설정")
			return
		}
		codeFilter := strings.TrimSpace(r.URL.Query().Get("codes"))

		var specs []*svcio.SvcSpec
		for _, sum := range deps.Registry.List() {
			if codeFilter != "" && !svcio.CodeMatch(codeFilter, sum.Code) {
				continue
			}
			if sp, ok := deps.Registry.Get(sum.Code); ok {
				specs = append(specs, sp)
			}
		}

		// 서버 URL 우선순위:
		//   1) 명시적 OpenAPIServer flag (외부 API 게이트웨이 — client 개발자 전달용)
		//   2) 요청이 들어온 실제 origin (scheme+host — 브라우저 접근 기준). admin
		//      의 내부 upstream (127.0.0.1:8080) 은 외부/브라우저에서 틀리므로 안 씀.
		server := deps.OpenAPIServer
		serverDesc := ""
		if server == "" {
			server = requestOrigin(r)
		} else {
			serverDesc = "외부 DMZ (mci-edge-api) — 실 클라이언트 호출용"
		}

		// DevMode 면 관리자 콘솔 same-origin 을 두 번째 서버로 추가한다. Swagger UI
		// "Try it out" 은 외부 DMZ(8090) 로는 CORS·self-signed TLS·JWT 때문에
		// 브라우저에서 막히므로, same-origin(admin :9090) 의 /v1/tx 프록시로 우회.
		// (admin 이 X-WTG-User DevMode 헤더로 인증 → 내부 mci-api 로 forward)
		testServer, testServerDesc := "", ""
		if deps.DevMode {
			if origin := requestOrigin(r); origin != "" && origin != server {
				testServer = origin
				testServerDesc = "관리자 콘솔 테스트 (DevMode, same-origin)"
			}
		}

		doc := svcio.BuildOpenAPI(specs, svcio.OpenAPIOptions{
			Title:          "WTG 매매 서비스 API (svc I/O 명세 자동 생성)",
			Version:        "1.0.0",
			Server:         server,
			ServerDesc:     serverDesc,
			TestServer:     testServer,
			TestServerDesc: testServerDesc,
			AliasFor:       aliasResolver(deps.Routes),
		})

		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", "attachment; filename=wtg-openapi.json")
		}
		writeJSON(w, http.StatusOK, doc)
	}
}

// requestOrigin 은 들어온 요청의 scheme+host 를 돌려준다 (프록시 헤더 우선).
// OpenAPI servers URL 기본값 — 클라이언트가 실제로 접근한 origin 을 반영.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}
