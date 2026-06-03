package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/internal/api/transform"
	"github.com/winwaysystems/wtg/pkg/idempotency"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
	"github.com/winwaysystems/wtg/pkg/routing"
)

// mkAliasRegistry — 가변 인자: alias, exchange, routing_key, active 4 개씩 묶음.
func mkAliasRegistry(t *testing.T, args ...any) *routing.InMemoryRegistry {
	t.Helper()
	reg := routing.NewInMemoryRegistry(nil)
	for i := 0; i < len(args); i += 4 {
		reg.Put(&routing.Rule{
			Alias:      args[i].(string),
			Exchange:   args[i+1].(string),
			RoutingKey: args[i+2].(string),
			Active:     args[i+3].(bool),
		}, "test")
	}
	return reg
}

// fakeCaller 는 핸들러 테스트용 Caller mock.
// reply 함수가 호출 시점의 입력을 받아 broker 응답을 시뮬레이션한다.
type fakeCaller struct {
	reply func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error)
}

func (f *fakeCaller) Call(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
	return f.reply(ctx, in)
}

// 테스트 헬퍼: 인증된 요청을 만들어 핸들러로 보낸다.
func doAuthenticatedTx(t *testing.T, deps *Deps, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx", strings.NewReader(body))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:    "trader01",
		Channel: "WEB",
	}))
	rr := httptest.NewRecorder()
	Transaction(deps)(rr, req)
	return rr
}

func quietDeps(caller Caller) *Deps {
	return &Deps{
		MQ:          caller,
		CallTimeout: 1 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestTransactionSuccess(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			// envelope 가 매매 엔진까지 잘 전달됐는지 검증.
			if in.Xchg != "ORDER" || in.Rkey != "NEW" {
				t.Errorf("xchg/rkey: %q/%q", in.Xchg, in.Rkey)
			}
			if !strings.Contains(string(in.Body), "USDKRW") {
				t.Errorf("body: %q", in.Body)
			}
			return &mymq.Reply{
				Body: []byte(`{"order_id":"O-42","status":"ACCEPTED"}`),
			}, nil
		},
	}
	deps := quietDeps(caller)
	rr := doAuthenticatedTx(t, deps, `{
		"exchange":"ORDER","routing_key":"NEW",
		"data":{"symbol":"USDKRW","qty":1000}
	}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200", rr.Code)
	}
	var env transform.Envelope
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Errn != 0 {
		t.Errorf("Errn: %d", env.Errn)
	}
	var data map[string]string
	_ = json.Unmarshal(env.Data, &data)
	if data["order_id"] != "O-42" {
		t.Errorf("order_id: %q", data["order_id"])
	}
}

func TestTransactionBusinessError(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{
				Errn:   mymq.ErrNoSvc,
				ErrMsg: "unknown transaction code",
			}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"exchange":"ORDER","routing_key":"INVALID"}`)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400 (no_service)", rr.Code)
	}
	var env transform.Envelope
	_ = json.NewDecoder(rr.Body).Decode(&env)
	if env.Errn != mymq.ErrNoSvc {
		t.Errorf("envelope.errn: %d", env.Errn)
	}
}

func TestTransactionAuthErrorFromBroker(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{
				Errn:   mymq.ErrAuth,
				ErrMsg: "Authentication failed",
			}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"routing_key":"NEW","data":{}}`)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: %d, want 401", rr.Code)
	}
}

func TestTransactionBrokerCallFailure(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return nil, mymq.ErrReconnecting
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"routing_key":"NEW"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503", rr.Code)
	}
}

func TestTransactionContextDeadline(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return nil, context.DeadlineExceeded
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"routing_key":"NEW"}`)
	// context.DeadlineExceeded 는 비즈니스 broker 에러와 다른 처리 → mapBrokerError 의 fallback (500).
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500 (unmapped error fallback)", rr.Code)
	}
}

func TestTransactionRawBodyResponse(t *testing.T) {
	// broker 가 JSON 이 아닌 raw text 응답을 보내면 string 으로 감싸서 노출.
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: []byte("RAW_TEXT")}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"routing_key":"PING"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var env transform.Envelope
	_ = json.NewDecoder(rr.Body).Decode(&env)
	var got string
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got != "RAW_TEXT" {
		t.Errorf("string wrap: %q", got)
	}
}

func TestTransactionWithKeysAndChannel(t *testing.T) {
	// pkey/nkey/keyc 가 frame 에 정확히 매핑되는지 검증.
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Keyc != mymq.KeyNext {
				t.Errorf("Keyc: %v", in.Keyc)
			}
			if string(in.Pkey) != "p123" {
				t.Errorf("Pkey: %q", in.Pkey)
			}
			if string(in.Nkey) != "n456" {
				t.Errorf("Nkey: %q", in.Nkey)
			}
			return &mymq.Reply{}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller),
		`{"routing_key":"QUERY","keyc":"N","pkey":"p123","nkey":"n456"}`)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestTransactionPanicSafety(t *testing.T) {
	// caller 가 panic 해도 핸들러 단독으로는 panic 이 전파되지만,
	// 운영에서는 Recover 미들웨어가 잡는다. 여기서는 핸들러가 panic 자체는
	// 차단하지 않는다는 사실만 확인 (Recover 미들웨어가 책임).
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			panic("simulated broker panic")
		},
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("핸들러 단독으로는 panic 이 전파되어야 한다 (Recover 미들웨어 책임)")
		}
	}()
	doAuthenticatedTx(t, quietDeps(caller), `{"routing_key":"X"}`)
}

// PrincipalRequired 가 누락된 사용자에게 401 을 반환하는지 별도 검증.
func TestPrincipalRequiredEmpty(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	if _, ok := principalRequired(rr, req); ok {
		t.Error("Principal 없는데 ok 반환")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestPrincipalRequiredBlankUsid(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "",
	}))
	if _, ok := principalRequired(rr, req); ok {
		t.Error("빈 Usid 인데 ok 반환")
	}
}

// Ping 핸들러 검증.
func TestPing(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	Ping(quietDeps(nil))(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ok" {
		t.Errorf("status field: %q", got["status"])
	}
	if got["service"] != "mci-api" {
		t.Errorf("service: %q", got["service"])
	}
}

// 빈 raw passthrough — broker 가 빈 응답.
func TestTransactionEmptyReplyBody(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: nil}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller), `{"routing_key":"PING"}`)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
}

// SessionMode — Principal.Cookie 가 frame 에 자동 첨부되는지 검증.
func TestTransactionAttachesCookieFromPrincipal(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Cookie == nil {
				t.Error("Principal.Cookie 가 frame 에 첨부되지 않음")
			}
			if in.Cookie != nil && in.Cookie.Clid != 0xCAFEBABE {
				t.Errorf("Cookie.Clid: 0x%X", in.Cookie.Clid)
			}
			return &mymq.Reply{}, nil
		},
	}
	deps := quietDeps(caller)
	cookie := &mymq.Cookie{Clid: 0xCAFEBABE}
	req := httptest.NewRequest(http.MethodPost, "/v1/tx",
		strings.NewReader(`{"routing_key":"NEW","data":{}}`))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "trader01", Channel: "WEB", SessionID: "sid-1", Cookie: cookie,
	}))
	rr := httptest.NewRecorder()
	Transaction(deps)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// DevMode — Principal.Cookie 가 nil 이라 frame.Cookie 도 nil.
func TestTransactionDevModeNoCookieAttached(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Cookie != nil {
				t.Errorf("DevMode 인데 cookie 첨부됨: %+v", in.Cookie)
			}
			return &mymq.Reply{}, nil
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller), `{"routing_key":"X"}`)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d", rr.Code)
	}
}

// alias 가 활성 룰이면 exchange/routing_key 가 룰 값으로 치환되어 broker 호출.
func TestTransactionResolvesAlias(t *testing.T) {
	reg := mkAliasRegistry(t, "ORDER_NEW", "ORDER_V2", "NEW_V2", true)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Xchg != "ORDER_V2" || in.Rkey != "NEW_V2" {
				t.Errorf("alias 치환 실패: %q/%q", in.Xchg, in.Rkey)
			}
			return &mymq.Reply{}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Routes = reg
	rr := doAuthenticatedTx(t, deps,
		`{"alias":"ORDER_NEW","data":{}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// alias 미등록이면 404 (broker 호출 안 됨).
func TestTransactionUnknownAlias(t *testing.T) {
	reg := mkAliasRegistry(t)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("미등록 alias 인데 broker 호출됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller)
	deps.Routes = reg
	rr := doAuthenticatedTx(t, deps, `{"alias":"NOPE"}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

// alias 비활성도 404 (보수적 거부 — fallback 안 함).
func TestTransactionInactiveAlias(t *testing.T) {
	reg := mkAliasRegistry(t, "OFF", "X", "Y", false)
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("비활성 alias 인데 broker 호출됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller)
	deps.Routes = reg
	rr := doAuthenticatedTx(t, deps, `{"alias":"OFF"}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", rr.Code)
	}
}

// 정책 — kill switch 활성 시 503 (broker 호출 안 됨).
func TestTransactionKillSwitchBlocks(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("kill switch 인데 broker 호출됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller)
	deps.Policy = policy.NewEngine(nil)
	deps.Policy.SetKillSwitch(true, "admin")

	rr := doAuthenticatedTx(t, deps, `{"routing_key":"NEW","data":{}}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), policy.ReasonKillSwitch) {
		t.Errorf("body: %s", rr.Body.String())
	}
}

// 정책 — 차단 심볼 시 403.
func TestTransactionBlockedSymbol(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("차단 심볼인데 broker 호출됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller)
	deps.Policy = policy.NewEngine(nil)
	_ = deps.Policy.AddBlockedSymbol("USDKRW", "admin")

	rr := doAuthenticatedTx(t, deps,
		`{"routing_key":"NEW","data":{"symbol":"USDKRW","qty":100}}`)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403", rr.Code)
	}
}

// 정책 — 차단 안 된 심볼은 통과.
func TestTransactionPolicyAllowsOther(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Policy = policy.NewEngine(nil)
	_ = deps.Policy.AddBlockedSymbol("USDKRW", "admin")

	rr := doAuthenticatedTx(t, deps,
		`{"routing_key":"NEW","data":{"symbol":"EURUSD"}}`)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (다른 심볼)", rr.Code)
	}
}

// nil reply (caller 가 nil 반환 시 mapBrokerError 내부에서 안전하게 처리되는지).
func TestTransactionNilReplyError(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return nil, errors.New("connection lost")
		},
	}
	rr := doAuthenticatedTx(t, quietDeps(caller), `{"routing_key":"X"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status: %d", rr.Code)
	}
}

// ─── Idempotency-Key ──────────────────────────────────────────────────────────

// doAuthenticatedTxWith — header 포함 변형. body 같이 단일 호출.
func doAuthenticatedTxWith(t *testing.T, deps *Deps, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx", strings.NewReader(body))
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:    "trader01",
		Channel: "WEB",
	}))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	Transaction(deps)(rr, req)
	return rr
}

// 첫 호출은 broker 도달 + reply 캐시. 같은 키 + body 두 번째는 broker 호출
// X + 캐시된 응답 + Idempotency-Cached 헤더.
func TestTransactionIdempotency_CachedReplay(t *testing.T) {
	calls := 0
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls++
			return &mymq.Reply{Body: []byte(`{"order_id":"O-99","status":"ACCEPTED"}`)}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})
	body := `{"exchange":"ORDER","routing_key":"NEW","data":{"symbol":"USDKRW","qty":1000}}`

	// 1차
	rr1 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-1"})
	if rr1.Code != http.StatusOK {
		t.Fatalf("1차 status=%d, want 200", rr1.Code)
	}
	if calls != 1 {
		t.Errorf("1차 broker calls=%d, want 1", calls)
	}
	if rr1.Header().Get("Idempotency-Cached") != "" {
		t.Errorf("1차에 Idempotency-Cached 헤더 — 새 요청이어야")
	}

	// 2차 (replay)
	rr2 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-1"})
	if rr2.Code != http.StatusOK {
		t.Errorf("2차 status=%d, want 200 (replay)", rr2.Code)
	}
	if calls != 1 {
		t.Errorf("2차 broker calls=%d, want 1 (skip)", calls)
	}
	if rr2.Header().Get("Idempotency-Cached") != "true" {
		t.Errorf("2차에 Idempotency-Cached=true 누락")
	}
	if rr1.Body.String() != rr2.Body.String() {
		t.Errorf("응답 본문 불일치:\n1차=%s\n2차=%s", rr1.Body.String(), rr2.Body.String())
	}
}

// 같은 키 + 다른 body → 409 conflict.
func TestTransactionIdempotency_BodyConflict(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})

	// 1차 — KEY-2 + body A
	_ = doAuthenticatedTxWith(t, deps,
		`{"exchange":"ORDER","routing_key":"NEW","data":{"qty":100}}`,
		map[string]string{"Idempotency-Key": "KEY-2"})

	// 2차 — KEY-2 + body B
	rr := doAuthenticatedTxWith(t, deps,
		`{"exchange":"ORDER","routing_key":"NEW","data":{"qty":999}}`,
		map[string]string{"Idempotency-Key": "KEY-2"})

	if rr.Code != http.StatusConflict {
		t.Errorf("2차 status=%d, want 409 (다른 body 충돌)", rr.Code)
	}
}

// 헤더 부재 — idempotency 미적용 (broker 매번 호출, 캐시 X).
func TestTransactionIdempotency_NoHeaderPassThrough(t *testing.T) {
	calls := 0
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls++
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})

	body := `{"exchange":"ORDER","routing_key":"NEW","data":{}}`
	_ = doAuthenticatedTx(t, deps, body)
	_ = doAuthenticatedTx(t, deps, body)
	if calls != 2 {
		t.Errorf("헤더 없으면 매번 broker — calls=%d, want 2", calls)
	}
}

// store nil — 헤더 있어도 idempotency 미적용 (기존 동작).
func TestTransactionIdempotency_StoreDisabled(t *testing.T) {
	calls := 0
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls++
			return &mymq.Reply{Body: []byte(`{"ok":true}`)}, nil
		},
	}
	deps := quietDeps(caller) // Idempotency nil
	body := `{"exchange":"ORDER","routing_key":"NEW","data":{}}`
	_ = doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-3"})
	_ = doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-3"})
	if calls != 2 {
		t.Errorf("store nil 이면 매번 broker — calls=%d, want 2", calls)
	}
}

// broker 비즈니스 에러 (errn != 0, 422) 도 캐시 — 결정적 응답.
func TestTransactionIdempotency_BusinessErrorCached(t *testing.T) {
	calls := 0
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls++
			return &mymq.Reply{Errn: mymq.ErrNoSvc, ErrMsg: "no service"}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})

	body := `{"exchange":"X","routing_key":"Y","data":{}}`
	rr1 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-4"})
	rr2 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-4"})

	if calls != 1 {
		t.Errorf("비즈니스 에러도 캐시 — calls=%d, want 1", calls)
	}
	if rr1.Code != rr2.Code {
		t.Errorf("status mismatch: %d vs %d", rr1.Code, rr2.Code)
	}
	if rr2.Header().Get("Idempotency-Cached") != "true" {
		t.Errorf("2차 비즈니스 에러 응답이 캐시 미표시")
	}
}

// broker network 에러 (5xx) — 캐시 X, 재시도 시 broker 다시 호출.
func TestTransactionIdempotency_BrokerErrorRollback(t *testing.T) {
	calls := 0
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			calls++
			return nil, errors.New("connection lost")
		},
	}
	deps := quietDeps(caller)
	deps.Idempotency = idempotency.NewMemoryStore(idempotency.Options{})

	body := `{"exchange":"X","routing_key":"Y","data":{}}`
	rr1 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-5"})
	if rr1.Code != http.StatusInternalServerError {
		t.Errorf("1차 status=%d, want 500", rr1.Code)
	}
	rr2 := doAuthenticatedTxWith(t, deps, body, map[string]string{"Idempotency-Key": "KEY-5"})
	if calls != 2 {
		t.Errorf("5xx 에러 후 재시도 — broker 다시 호출되어야: calls=%d, want 2", calls)
	}
	_ = rr2
}
