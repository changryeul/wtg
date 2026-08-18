package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/svcio"
)

// newValidateSvcIO — yuanta T1204S01 (검증 로그인) 명세 + COMHDR 등록.
func newValidateSvcIO(t *testing.T) *svcio.Registry {
	t.Helper()
	reg := svcio.NewRegistry()
	reg.RegisterHeader("COMHDR", []svcio.Field{
		{Name: "trxc", CType: "char", Size: 16},
		{Name: "usid", CType: "char", Size: 16},
		{Name: "eflg", CType: "char", Size: 1},
		{Name: "rcod", CType: "char", Size: 5},
		{Name: "mesg", CType: "char", Size: 64},
	})
	dir := t.TempDir()
	body := `typedef struct { // Input
	char	user_id			[  8];
	char	user_pw			[100];
} T1204S01_I;

typedef struct { // Output
	char	loginYN			[  1];
	char	changePWYN		[  1];
	char	msg				[100];
} T1204S01_O;
`
	if err := os.WriteFile(filepath.Join(dir, "T1204S01.h"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.SetDirHeaderDefault(dir, "COMHDR")
	if n, _, err := reg.LoadDir(dir, nil); err != nil || n != 1 {
		t.Fatalf("LoadDir n=%d err=%v", n, err)
	}
	return reg
}

func validateDeps(caller Caller, reg *svcio.Registry) *Deps {
	return &Deps{
		MQ:            caller,
		CallTimeout:   time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		SvcIO:         reg,
		LoginValidate: &LoginValidateConfig{},
	}
}

func TestLoginValidateSuccess(t *testing.T) {
	reg := newValidateSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"T1204S01": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "T1204S01", map[string]interface{}{
				"loginYN": "1", "changePWYN": "0", "msg": "",
			})}, nil
		},
	})
	deps := validateDeps(caller, reg)
	store := newStoreForTest(t)
	deps.Sessions = store

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"alias":"T1204S01","data":{"user_id":"yu01","user_pw":"pw"}}`))
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
	sess, err := store.Get(context.Background(), resp.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// 세션 usid=입력 user_id, 쿠키/lgnIdntCon 없음 (검증 로그인은 엔진 쿠키 미발급).
	if sess.Usid != "yu01" {
		t.Errorf("usid=%q want yu01", sess.Usid)
	}
	if sess.Cookie != nil || sess.LgnIdntCon != "" {
		t.Errorf("validate 세션은 쿠키/lgnIdntCon 없어야: cookie=%v lgn=%q", sess.Cookie, sess.LgnIdntCon)
	}
	// broker 로 T1204S01 이 나갔나.
	if len(calls) != 1 || calls[0].Rkey != "T1204S01" {
		t.Fatalf("calls=%v (T1204S01 1건 기대)", calls)
	}
	// 응답 data 에 changePWYN 노출.
	var data map[string]any
	if err := json.Unmarshal(resp.Data, &data); err == nil {
		if _, ok := data["changePWYN"]; !ok {
			t.Errorf("응답 data 에 changePWYN 없음: %v", data)
		}
	}
}

func TestLoginValidateRejected(t *testing.T) {
	reg := newValidateSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, &calls, map[string]func() (*mymq.Reply, error){
		"T1204S01": func() (*mymq.Reply, error) {
			return &mymq.Reply{Body: chainReply(t, reg, "T1204S01", map[string]interface{}{
				"loginYN": "0", "changePWYN": "0", "msg": "비밀번호 오류",
			})}, nil
		},
	})
	deps := validateDeps(caller, reg)
	deps.Sessions = newStoreForTest(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"alias":"T1204S01","data":{"user_id":"yu01","user_pw":"bad"}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "login_failed" || body["message"] != "비밀번호 오류" {
		t.Errorf("body=%v (login_failed + msg 기대)", body)
	}
}

// LoginValidate 미설정이면 alias 가 있어도 validate 로 가지 않는다 (안전).
func TestLoginValidateDisabledIgnoresAlias(t *testing.T) {
	reg := newValidateSvcIO(t)
	caller := &fakeCaller{reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
		if in.Rkey == "T1204S01" {
			t.Fatalf("LoginValidate 미설정인데 T1204S01 호출됨 (validate 로 새면 안 됨)")
		}
		return &mymq.Reply{}, nil // legacy LOGON — cookie 없음 → no_cookie 로 떨어짐
	}}
	deps := validateDeps(caller, reg)
	deps.LoginValidate = nil // 비활성
	deps.Sessions = newStoreForTest(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/login",
		strings.NewReader(`{"alias":"T1204S01","data":{"user_id":"yu01"}}`))
	rr := httptest.NewRecorder()
	Login(deps)(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("validate 미설정인데 200 (검증 로그인이 실행됨) body=%s", rr.Body.String())
	}
}
