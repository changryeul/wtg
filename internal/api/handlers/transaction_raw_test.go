package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/policy"
)

// doRawTx — raw 전문 모드 (Content-Type: application/octet-stream) 요청 헬퍼.
func doRawTx(t *testing.T, deps *Deps, hdr map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tx", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid:    "empuser01",
		Channel: "EMP",
	}))
	rr := httptest.NewRecorder()
	Transaction(deps)(rr, req)
	return rr
}

// raw 모드 happy path — 요청 body 가 바이트 그대로 (인코딩/이스케이프 무변형)
// 엔진에 전달되고, 응답도 output 전문 바이트 그대로 나온다.
func TestTransactionRawModeSuccess(t *testing.T) {
	// CP949 '가' (0xB0 0xA1) 포함 — UTF-8 이 아닌 레거시 전문 바이트 보존 검증.
	reqMsg := append([]byte("W3100T01  empuser01 "), 0xB0, 0xA1, ' ', '1')
	repMsg := append([]byte("W3100T01  OK        "), 0xB0, 0xA1)

	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			if in.Xchg != "dom" || in.Rkey != "W3100T01" {
				t.Errorf("xchg/rkey: %q/%q", in.Xchg, in.Rkey)
			}
			if !bytes.Equal(in.Body, reqMsg) {
				t.Errorf("body 무변형 전달 실패:\n got=%x\nwant=%x", in.Body, reqMsg)
			}
			return &mymq.Reply{Body: repMsg}, nil
		},
	}
	deps := quietDeps(caller)
	deps.Routes = mkAliasRegistry(t, "W3100T01", "dom", "W3100T01", true)

	rr := doRawTx(t, deps, map[string]string{"X-WTG-Alias": "W3100T01"}, reqMsg)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type: %q", ct)
	}
	if errn := rr.Header().Get("X-WTG-Errn"); errn != "0" {
		t.Errorf("X-WTG-Errn: %q, want 0", errn)
	}
	if !bytes.Equal(rr.Body.Bytes(), repMsg) {
		t.Errorf("응답 전문 무변형 실패:\n got=%x\nwant=%x", rr.Body.Bytes(), repMsg)
	}
}

// 비즈니스 에러 (errn≠0) 여도 output 전문이 있으면 body 그대로 + errn 은 헤더로.
// 레거시 emp/hts 는 COMHDR 의 eflg/mesg 로 에러를 판단한다 — 위임 원칙.
func TestTransactionRawModeBusinessErrorBodyPassthrough(t *testing.T) {
	repMsg := []byte("W3100T01  ERR e1")
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 3001, ErrMsg: "business reject", Body: repMsg}, nil
		},
	}
	rr := doRawTx(t, quietDeps(caller),
		map[string]string{"X-WTG-Exchange": "dom", "X-WTG-Routing-Key": "W3100T01"}, []byte("msg"))

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200 (body passthrough)", rr.Code)
	}
	if errn := rr.Header().Get("X-WTG-Errn"); errn != "3001" {
		t.Errorf("X-WTG-Errn: %q, want 3001", errn)
	}
	if !bytes.Equal(rr.Body.Bytes(), repMsg) {
		t.Errorf("에러 body passthrough 실패: %q", rr.Body.Bytes())
	}
}

// transport 에러 (엔진 output 전문 자체가 없음) — 매핑된 HTTP status + text 에러.
func TestTransactionRawModeBrokerErrorNoBody(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Errn: mymq.ErrNoSvc, ErrMsg: "no receiver"}, nil
		},
	}
	rr := doRawTx(t, quietDeps(caller),
		map[string]string{"X-WTG-Exchange": "dom", "X-WTG-Routing-Key": "W9999T99"}, []byte("msg"))

	if rr.Code == http.StatusOK {
		t.Fatalf("status: 200 인데 body 없는 broker 에러 — 매핑 status 여야 함")
	}
	if errn := rr.Header().Get("X-WTG-Errn"); errn == "" || errn == "0" {
		t.Errorf("X-WTG-Errn: %q", errn)
	}
}

// 대상 미지정 (alias / exchange+routing_key 헤더 없음) → 400.
func TestTransactionRawModeMissingTarget(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("broker 호출되면 안 됨")
			return nil, nil
		},
	}
	rr := doRawTx(t, quietDeps(caller), nil, []byte("msg"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: %d, want 400", rr.Code)
	}
}

// raw 모드에도 운영 정책 (kill switch 등) 이 동일하게 적용된다.
func TestTransactionRawModePolicyDeny(t *testing.T) {
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			t.Error("정책 차단인데 broker 호출됨")
			return nil, nil
		},
	}
	deps := quietDeps(caller)
	deps.Policy = policy.NewEngine(nil)
	deps.Policy.SetKillSwitch(true, "test")

	rr := doRawTx(t, deps,
		map[string]string{"X-WTG-Exchange": "dom", "X-WTG-Routing-Key": "W3100T01"}, []byte("msg"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d, want 503 (kill switch)", rr.Code)
	}
}

// debug 레벨에서 송수신 전문 preview 가 로그에 남고, info 레벨(운영 기본)
// 에서는 전문이 로그에 노출되지 않는다 (계좌/비밀번호 등 민감정보 보호).
func TestTransactionDebugWireLog(t *testing.T) {
	reqMsg := []byte("W3100T01  reqbody")
	repMsg := []byte("W3100T01  repbody")
	caller := &fakeCaller{
		reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
			return &mymq.Reply{Body: repMsg}, nil
		},
	}

	run := func(level slog.Level) string {
		var buf bytes.Buffer
		deps := quietDeps(caller)
		deps.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
		doRawTx(t, deps, map[string]string{
			"X-WTG-Exchange": "dom", "X-WTG-Routing-Key": "W3100T01"}, reqMsg)
		return buf.String()
	}

	dbg := run(slog.LevelDebug)
	if !strings.Contains(dbg, "tx 전문 송신") || !strings.Contains(dbg, "reqbody") {
		t.Errorf("debug 송신 전문 로그 누락:\n%s", dbg)
	}
	if !strings.Contains(dbg, "tx 전문 수신") || !strings.Contains(dbg, "repbody") {
		t.Errorf("debug 수신 전문 로그 누락:\n%s", dbg)
	}

	info := run(slog.LevelInfo)
	if strings.Contains(info, "reqbody") || strings.Contains(info, "repbody") {
		t.Errorf("info 레벨에 전문 노출:\n%s", info)
	}
}

// bodyPreview — 비인쇄 바이트 escape + truncate 동작.
func TestBodyPreview(t *testing.T) {
	got := bodyPreview(append([]byte("AB"), 0xB0, 0xA1), 100)
	if !strings.Contains(got, "AB") || !strings.Contains(got, `\xb0\xa1`) {
		t.Errorf("escape: %q", got)
	}
	long := bytes.Repeat([]byte("x"), 600)
	got = bodyPreview(long, 512)
	if !strings.Contains(got, "…(600 bytes)") {
		t.Errorf("truncate 표기: %q", got)
	}
}
