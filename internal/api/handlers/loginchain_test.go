package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/internal/api/middleware"
	"github.com/winwaysystems/wtg/pkg/auth"
	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/svcio"
)

// chain 테스트용 svcio registry — COMHDR 축소판 + 사슬 3개 명세 (필드명은
// 실제 W1101S02.h / W1130A02.h / W1130A03.h 와 동일, 크기만 축소).
func newChainSvcIO(t *testing.T) *svcio.Registry {
	t.Helper()
	reg := svcio.NewRegistry()
	reg.RegisterHeader("COMHDR", []svcio.Field{
		{Name: "trxc", CType: "char", Size: 16},
		{Name: "usid", CType: "char", Size: 30},
		{Name: "loip", CType: "char", Size: 15},
		{Name: "cont", CType: "char", Size: 1},
	})
	dir := t.TempDir()
	specs := map[string]string{
		"W1101S02.h": `typedef struct {	// Input
	char	prGb			 [   1];  // 작업구분
	char 	signMsg			 [ 256];  // 인증서명
} W1101S02_I;

typedef struct {	// Output
	char 	cifNo			[ 20];  // 실명번호
	char 	lgnId			[ 20];  // 로그인ID
	char 	otinYn			[  1];  // 타기관발급여부
	char 	svrCert         [ 64];  // 서버key
} W1101S02_O;
`,
		"W1130A02.h": `typedef struct {	// Input
	char    prGb                [ 1 ]; // 처리구분
	char    fxUserNo            [ 30]; // FX사용자번호
	char    fxUserNm            [ 40]; // FX사용자명
	char    lgnId               [ 20]; // 로그인ID
} W1130A02_I;

typedef struct {	// Output
	char    lgnIdntCon       [ 50]; // 로그인식별내용
	char    fwdPreChkPopYn   [ 1 ]; // 선물환사전점검팝업여부
	char    apllBsopYmd      [ 8 ]; // 당영업년월일
	char    wlbrYmd          [ 8 ]; // 전영업년월일
	char    nxtBsopYmd       [ 8 ]; // 익영업년월일
	char    lgnTs            [ 14]; // 최종로그아웃일시
} W1130A02_O;
`,
		"W1130A03.h": `typedef struct {	// Input
	char    prGb                [ 1 ]; // 처리구분
	char    fxUserNo            [ 30]; // FX사용자번호
	char    fxUserNm            [ 40]; // FX사용자명
	char    lgnIdntCon          [ 50]; // 로그인식별내용
} W1130A03_I;

typedef struct {	// Output
	char    dummy               [ 1 ]; // dummy
} W1130A03_O;
`,
	}
	for name, body := range specs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg.SetDirHeaderDefault(dir, "COMHDR")
	if n, _, err := reg.LoadDir(dir, nil); err != nil || n != 3 {
		t.Fatalf("LoadDir: n=%d err=%v", n, err)
	}
	return reg
}

// chainReply 는 spec 의 Output 필드로 fake 응답 전문을 조립한다.
func chainReply(t *testing.T, reg *svcio.Registry, rkey string, out map[string]interface{}) []byte {
	t.Helper()
	spec, ok := reg.Get(rkey)
	if !ok {
		t.Fatalf("spec %s 없음", rkey)
	}
	body, err := svcio.SerializeWithHeader(spec.HeaderFields,
		map[string]interface{}{"trxc": rkey}, spec.Output, out)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// chainFakeCaller 는 rkey 별 스크립트된 응답을 돌려주고 관측용으로 frame 을 기록.
func chainFakeCaller(t *testing.T, calls *[]*mymq.FrameInput,
	replies map[string]func() (*mymq.Reply, error)) *fakeCaller {
	t.Helper()
	return &fakeCaller{reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
		*calls = append(*calls, in)
		fn, ok := replies[in.Rkey]
		if !ok {
			t.Fatalf("예상 밖 rkey: %s", in.Rkey)
		}
		return fn()
	}}
}

func chainDeps(caller Caller, reg *svcio.Registry) *Deps {
	return &Deps{
		MQ:          caller,
		CallTimeout: time.Second,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SvcIO:       reg,
		LoginChain:  &LoginChainConfig{},
	}
}

func TestRunLoginChainSuccess(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1101S02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1101S02", map[string]interface{}{
				"cifNo": "1234567890", "lgnId": "hong01", "otinYn": "N", "svrCert": "CERTDATA",
			})}, nil
		},
		"W1130A02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1130A02", map[string]interface{}{
				"lgnIdntCon": "20260720|MAC|10.0.0.7|hong01", "apllBsopYmd": "20260720",
				"wlbrYmd": "20260717", "nxtBsopYmd": "20260721", "lgnTs": "20260719213000",
			})}, nil
		},
	})
	deps := chainDeps(caller, reg)

	res, err := runLoginChain(context.Background(), deps, "SIGNMSG", "10.0.0.7")
	if err != nil {
		t.Fatal(err)
	}
	if res.LgnID != "hong01" || res.CifNo != "1234567890" {
		t.Errorf("LgnID=%q CifNo=%q", res.LgnID, res.CifNo)
	}
	if res.LgnIdntCon != "20260720|MAC|10.0.0.7|hong01" {
		t.Errorf("LgnIdntCon=%q", res.LgnIdntCon)
	}
	if res.ApllBsopYmd != "20260720" || res.SvrCert != "CERTDATA" {
		t.Errorf("부가필드: %+v", res)
	}

	// wire 검증 — ①: usid 공란 + signMsg / ③: usid=lgnId + fxUserNo=lgnId + loip.
	if len(calls) != 2 {
		t.Fatalf("호출 수 %d", len(calls))
	}
	step1 := string(calls[0].Body)
	if !strings.HasPrefix(step1, "W1101S02") || !strings.Contains(step1, "SIGNMSG") {
		t.Errorf("① body: %q", step1)
	}
	if got := strings.TrimSpace(step1[16:46]); got != "" {
		t.Errorf("① usid 는 공란이어야 함: %q", got)
	}
	step3 := string(calls[1].Body)
	if got := strings.TrimSpace(step3[16:46]); got != "hong01" {
		t.Errorf("③ usid=%q", got)
	}
	if !strings.Contains(step3, "10.0.0.7") {
		t.Errorf("③ loip 누락: %q", step3)
	}
	if !strings.Contains(step3[46:], "hong01") {
		t.Errorf("③ fxUserNo 누락: %q", step3)
	}
}

func TestRunLoginChainCertRejected(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1101S02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 91001, ErrMsg: "인증서 검증 실패"}, nil
		},
	})
	deps := chainDeps(caller, reg)

	_, err := runLoginChain(context.Background(), deps, "BADSIGN", "10.0.0.7")
	var stepErr *chainStepError
	if !errors.As(err, &stepErr) {
		t.Fatalf("chainStepError 아님: %v", err)
	}
	if stepErr.Errn != 91001 || stepErr.Step != "cert" {
		t.Errorf("step=%s errn=%d", stepErr.Step, stepErr.Errn)
	}
	if len(calls) != 1 {
		t.Errorf("① 거부 후 ③ 호출되면 안 됨: %d", len(calls))
	}
}

func TestRunLoginChainSessionRejected(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1101S02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1101S02", map[string]interface{}{
				"cifNo": "1234567890", "lgnId": "hong01",
			})}, nil
		},
		"W1130A02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 91004, ErrMsg: "로그인 처리 오류"}, nil
		},
	})
	deps := chainDeps(caller, reg)

	_, err := runLoginChain(context.Background(), deps, "SIGNMSG", "10.0.0.7")
	var stepErr *chainStepError
	if !errors.As(err, &stepErr) {
		t.Fatalf("chainStepError 아님: %v", err)
	}
	if stepErr.Step != "session" || stepErr.Errn != 91004 {
		t.Errorf("step=%s errn=%d", stepErr.Step, stepErr.Errn)
	}
}

func TestRunLoginChainNoSpec(t *testing.T) {
	// SvcIO 는 있으나 명세 미등록 — broker 호출 없이 구성 오류로 즉시 실패해야 함.
	deps := chainDeps(&fakeCaller{reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
		t.Fatal("broker 호출되면 안 됨")
		return nil, nil
	}}, svcio.NewRegistry())

	_, err := runLoginChain(context.Background(), deps, "SIGNMSG", "10.0.0.7")
	if err == nil || !strings.Contains(err.Error(), "명세") {
		t.Errorf("명세 미등록 에러여야 함: %v", err)
	}
}

func TestLoginChainHandlerSuccess(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1101S02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1101S02", map[string]interface{}{
				"cifNo": "1234567890", "lgnId": "hong01", "svrCert": "CERTDATA",
			})}, nil
		},
		"W1130A02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1130A02", map[string]interface{}{
				"lgnIdntCon": "IDNT-1", "apllBsopYmd": "20260720",
			})}, nil
		},
	})
	deps := chainDeps(caller, reg)
	store := newStoreForTest(t)
	deps.Sessions = store

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"data":{"signMsg":"SIGNMSG"}}`))
	req.RemoteAddr = "10.0.0.7:51234"
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp LoginResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SessionID == "" {
		t.Fatal("session_id 없음")
	}
	// 세션 검증 — usid=lgnId, LgnIdntCon/CifNo 보관, Cookie nil.
	sess, err := store.Get(context.Background(), resp.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Usid != "hong01" || sess.LgnIdntCon != "IDNT-1" || sess.CifNo != "1234567890" {
		t.Errorf("세션: usid=%q lgn=%q cif=%q", sess.Usid, sess.LgnIdntCon, sess.CifNo)
	}
	if sess.Cookie != nil {
		t.Error("chain 세션에 Cookie 가 있으면 안 됨")
	}
	// 응답 data — 영업일/lgnId 노출 + cifNo 미노출.
	var data map[string]any
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["apllBsopYmd"] != "20260720" || data["lgnId"] != "hong01" {
		t.Errorf("data=%v", data)
	}
	if _, ok := data["cifNo"]; ok {
		t.Error("cifNo 는 응답에 노출 금지")
	}
}

func TestLoginChainHandlerCertRejected(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1101S02": func() (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 91001, ErrMsg: "인증서 검증 실패"}, nil
		},
	})
	deps := chainDeps(caller, reg)
	deps.Sessions = newStoreForTest(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"data":{"signMsg":"BAD"}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["error"] != "login_failed" || body["errn"] != float64(91001) {
		t.Errorf("body=%v", body)
	}
}

func TestLoginChainHandlerMissingSignMsg(t *testing.T) {
	deps := chainDeps(&fakeCaller{reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
		t.Fatal("broker 호출되면 안 됨")
		return nil, nil
	}}, newChainSvcIO(t))
	deps.Sessions = newStoreForTest(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"data":{}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLogoutChainReturnsLgnIdntCon(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1130A03": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "W1130A03",
				map[string]interface{}{"dummy": "0"})}, nil
		},
	})
	deps := chainDeps(caller, reg)
	store := newStoreForTest(t)
	deps.Sessions = store

	sess := &auth.Session{
		ID: "sid-1", Usid: "hong01", Channel: "WEB",
		LgnIdntCon: "IDNT-1", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/logout", nil)
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "hong01", Channel: "WEB", SessionID: "sid-1",
	}))
	rr := httptest.NewRecorder()
	Logout(deps)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// W1130A03 이 lgnIdntCon 을 실어 호출됐는지.
	if len(calls) != 1 || calls[0].Rkey != "W1130A03" {
		t.Fatalf("calls=%d", len(calls))
	}
	if !strings.Contains(string(calls[0].Body), "IDNT-1") {
		t.Errorf("lgnIdntCon 미포함: %q", string(calls[0].Body))
	}
	// 세션은 삭제됨.
	if _, err := store.Get(context.Background(), "sid-1"); err == nil {
		t.Error("세션이 삭제돼야 함")
	}
}

func TestLogoutChainEngineFailureStillDeletes(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"W1130A03": func() (*mymq.Reply, error) {
			return &mymq.Reply{Errn: 99999, ErrMsg: "엔진 오류"}, nil
		},
	})
	deps := chainDeps(caller, reg)
	store := newStoreForTest(t)
	deps.Sessions = store
	_ = store.Put(context.Background(), &auth.Session{
		ID: "sid-2", Usid: "hong01", Channel: "WEB",
		LgnIdntCon: "IDNT-2", ExpiresAt: time.Now().Add(time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/logout", nil)
	req = req.WithContext(middleware.ContextWithPrincipal(req.Context(), &middleware.Principal{
		Usid: "hong01", Channel: "WEB", SessionID: "sid-2",
	}))
	rr := httptest.NewRecorder()
	Logout(deps)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if _, err := store.Get(context.Background(), "sid-2"); err == nil {
		t.Error("엔진 실패에도 세션은 삭제돼야 함 (멱등)")
	}
}
