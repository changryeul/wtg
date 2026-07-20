# 엔진 인증 사슬 로그인 (chain 모드) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `/v1/login` 에 NH 엔진 실제 인증 사슬 (W1101S02 인증서 → W1130A02 세션개설, OTP 는 seam) 모드를 추가한다 — 기존 cookie_t LOGON 경로 무손상, `--login-mode` 스위치.

**Architecture:** `internal/api/handlers/loginchain.go` 오케스트레이터가 `deps.MQ.Call` + `pkg/svcio` 로 두 스텝을 순차 호출. 세션은 `Cookie=nil` + `LgnIdntCon/CifNo` 보관, JWT/refresh 발급 로직은 legacy 와 공유 (`finishLogin` 추출). 로그아웃은 `LgnIdntCon` 있으면 W1130A03 반납.

**Tech Stack:** Go 1.25 (CI 1.23 호환), 기존 `pkg/svcio` / `pkg/routing` / `pkg/auth`, 테스트는 fakeCaller + miniredis (로컬 DB 없음).

**스펙:** `docs/superpowers/specs/2026-07-20-engine-login-chain-design.md`

**컨벤션 주의:** 주석/커밋 메시지 한글, 식별자 영문. env prefix 는 파일 컨벤션 따라 `WTG_API_LOGIN_MODE` (스펙의 `WTG_LOGIN_MODE` 에서 의도적 조정 — mci-api env 는 전부 `WTG_API_*`).

---

### Task 1: pkg/auth.Session 에 LgnIdntCon / CifNo 추가

**Files:**
- Modify: `pkg/auth/session.go:43-59` (Session struct)
- Modify: `pkg/auth/redisstore.go:74-128` (sessionDTO / toDTO / fromDTO)
- Test: `pkg/auth/redisstore_test.go` (기존 round-trip 테스트 파일에 추가)

- [x] **Step 1: Write the failing test**

`pkg/auth/redisstore_test.go` 에 추가 (기존 테스트의 miniredis 셋업 헬퍼 재사용 — 파일 상단의 기존 패턴 확인 후 동일하게):

```go
// chain 모드 세션 — Cookie 없이 LgnIdntCon/CifNo 만 보관하는 세션의 왕복.
func TestRedisStoreChainSessionRoundTrip(t *testing.T) {
	st, _ := newTestStore(t) // 기존 파일의 miniredis 헬퍼 이름에 맞출 것
	ctx := context.Background()

	in := &Session{
		ID:         "sid-chain-1",
		Usid:       "hong01",
		Channel:    "WEB",
		Cookie:     nil, // chain 모드 — cookie_t 없음
		LgnIdntCon: "202607201030|AA:BB|10.0.0.7|hong01",
		CifNo:      "1234567890",
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := st.Put(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := st.Get(ctx, "sid-chain-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.LgnIdntCon != in.LgnIdntCon {
		t.Errorf("LgnIdntCon=%q, want %q", out.LgnIdntCon, in.LgnIdntCon)
	}
	if out.CifNo != in.CifNo {
		t.Errorf("CifNo=%q, want %q", out.CifNo, in.CifNo)
	}
	if out.Cookie != nil {
		t.Errorf("Cookie 는 nil 이어야 함")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/auth/ -run TestRedisStoreChainSessionRoundTrip -v`
Expected: FAIL — `unknown field LgnIdntCon` (컴파일 에러)

- [x] **Step 3: Write minimal implementation**

`pkg/auth/session.go` — Session struct 의 `Cookie` 필드 아래에 추가:

```go
	// chain 모드 (엔진 인증 사슬) 전용 — cookie_t 대신 보관.
	// LgnIdntCon 은 W1130A02 가 발급한 엔진 세션 열쇠 (로그아웃 시 W1130A03 반납용).
	// CifNo 는 실명번호 (고객 식별자 — 응답 미노출, 추후 고객별 스프레드 조회 대비).
	LgnIdntCon string
	CifNo      string
```

`pkg/auth/redisstore.go` — sessionDTO 에 필드 추가 + toDTO/fromDTO 복사:

```go
// sessionDTO 에 (CookieB64 아래):
	LgnIdntCon string          `json:"lgn_idnt_con,omitempty"`
	CifNo      string          `json:"cif_no,omitempty"`

// toDTO: d := &sessionDTO{ ... } 안에
	LgnIdntCon: s.LgnIdntCon,
	CifNo:      s.CifNo,

// fromDTO: s := &Session{ ... } 안에
	LgnIdntCon: d.LgnIdntCon,
	CifNo:      d.CifNo,
```

(memstore 는 *Session 을 그대로 보관하므로 무변경 — 확인만.)

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/auth/ -race`
Expected: PASS (전체 — 기존 테스트 무손상 확인)

- [x] **Step 5: Commit**

```bash
git add pkg/auth/session.go pkg/auth/redisstore.go pkg/auth/redisstore_test.go
git commit -m "feat(auth): Session 에 LgnIdntCon/CifNo — chain 로그인 세션 보관"
```

---

### Task 2: loginchain.go — 사슬 오케스트레이터

**Files:**
- Create: `internal/api/handlers/loginchain.go`
- Test: `internal/api/handlers/loginchain_test.go`

- [x] **Step 1: Write the failing test**

`internal/api/handlers/loginchain_test.go` 신규:

```go
package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
func chainFakeCaller(t *testing.T, reg *svcio.Registry, calls *[]*mymq.FrameInput,
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
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
	if !strings.Contains(step3, "hong01") {
		t.Errorf("③ fxUserNo 누락: %q", step3)
	}
}

func TestRunLoginChainCertRejected(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
	// SvcIO 는 있으나 명세 미등록 — 구성 오류로 즉시 실패해야 함.
	deps := chainDeps(&fakeCaller{reply: func(ctx context.Context, in *mymq.FrameInput) (*mymq.Reply, error) {
		t.Fatal("broker 호출되면 안 됨")
		return nil, nil
	}}, svcio.NewRegistry())

	_, err := runLoginChain(context.Background(), deps, "SIGNMSG", "10.0.0.7")
	if err == nil || !strings.Contains(err.Error(), "명세") {
		t.Errorf("명세 미등록 에러여야 함: %v", err)
	}
}
```

(`errors` import 필요 — 파일 상단 import 에 포함할 것.)

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/handlers/ -run TestRunLoginChain -v`
Expected: FAIL — `undefined: runLoginChain` (컴파일 에러)

- [x] **Step 3: Write implementation**

`internal/api/handlers/handlers.go` Deps 에 필드 추가 (SvcIO 아래):

```go
	// LoginChain — 엔진 인증 사슬 (chain 모드) 설정. nil 이면 legacy
	// (단일 LOGON + cookie_t). 채워지면 /v1/login 이 W1101S02 → W1130A02
	// 사슬을 오케스트레이션한다. SvcIO 필수 (부팅 시 검증).
	LoginChain *LoginChainConfig
```

`internal/api/handlers/loginchain.go` 신규:

```go
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/routing"
	"github.com/winwaysystems/wtg/pkg/svcio"
)

// LoginChainConfig 는 엔진 인증 사슬 (chain 모드) 의 단계별 alias.
// docs/superpowers/specs/2026-07-20-engine-login-chain-design.md 참조.
//
// alias 는 라우팅 registry 로 exchange/routing_key resolve — 룰이 없으면
// NH 시드 컨벤션 (exchange="dom", rkey=alias) fallback.
type LoginChainConfig struct {
	CertAlias    string // ① 공인인증서 인증. 빈값이면 "W1101S02"
	SessionAlias string // ③ 로그인처리·세션개설. 빈값이면 "W1130A02"
	LogoutAlias  string // 로그아웃 반납. 빈값이면 "W1130A03"
}

const (
	defaultCertAlias     = "W1101S02"
	defaultSessionAlias  = "W1130A02"
	defaultLogoutAlias   = "W1130A03"
	defaultChainExchange = "dom" // NH trn 서비스 exchange 컨벤션
)

func (c *LoginChainConfig) certAlias() string {
	if c != nil && c.CertAlias != "" {
		return c.CertAlias
	}
	return defaultCertAlias
}

func (c *LoginChainConfig) sessionAlias() string {
	if c != nil && c.SessionAlias != "" {
		return c.SessionAlias
	}
	return defaultSessionAlias
}

func (c *LoginChainConfig) logoutAlias() string {
	if c != nil && c.LogoutAlias != "" {
		return c.LogoutAlias
	}
	return defaultLogoutAlias
}

// chainResult 는 사슬 완주 결과 — 세션 생성 + 응답 data 재료.
type chainResult struct {
	LgnID      string // = fxUserNo (W1101S02 가 CSC004M upsert — 조사노트 §6.2 해소)
	CifNo      string // 실명번호 — 응답 미노출, 세션 보관만
	SvrCert    string
	LgnIdntCon string
	// 클라 표시용 부가 정보 (W1130A02_O)
	FwdPreChkPopYn string
	ApllBsopYmd    string
	WlbrYmd        string
	NxtBsopYmd     string
	LgnTs          string
}

// chainStepError 는 엔진이 사슬의 특정 단계를 거부한 경우 (errn passthrough).
type chainStepError struct {
	Step string // "cert" | "session"
	Errn uint32
	Errm string
}

func (e *chainStepError) Error() string {
	return fmt.Sprintf("login chain %s 단계 거부: errn=%d %s", e.Step, e.Errn, e.Errm)
}

// resolveChainRoute 는 alias 를 exchange/routing_key 로 resolve 한다.
// 라우팅 룰 미등록 시 NH 컨벤션 fallback.
func resolveChainRoute(deps *Deps, alias string) (exchange, rkey string) {
	if rule, err := routing.Resolve(deps.Routes, alias); err == nil {
		return rule.Exchange, rule.RoutingKey
	}
	return defaultChainExchange, alias
}

// callChainStep 은 사슬 한 단계를 svcio 조립 → Call → 파싱까지 수행한다.
// step 은 chainStepError 표시용 ("cert" / "session" / "logout").
func callChainStep(ctx context.Context, deps *Deps, step, alias, enforceUsid string,
	header map[string]interface{}, input map[string]interface{},
) (out map[string]interface{}, err error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("chain %s 입력 marshal: %w", step, err)
	}
	// 명세 lookup 은 항상 alias(=transaction code) 기준 — trxc 도 alias 로 박힘.
	body, spec, err := wireBuildBody(deps.SvcIO, alias, enforceUsid, header, data)
	if err != nil {
		return nil, err
	}
	if body == nil || spec == nil {
		return nil, fmt.Errorf("chain %s: svc 명세 %s 미등록 — --svc-inc-dir 확인", step, alias)
	}

	exchange, rkey := resolveChainRoute(deps, alias)
	frame := &mymq.FrameInput{
		Func: mymq.FCTran,
		Subc: mymq.SubTranMsg,
		Dirf: mymq.DirForward,
		Keyc: mymq.KeySend,
		Xchg: exchange,
		Rkey: rkey,
		Body: body,
		// Cookie 미첨부 — chain 모드는 cookie_t 를 쓰지 않는다.
	}
	callCtx, cancel := context.WithTimeout(ctx, deps.CallTimeout)
	defer cancel()
	reply, err := deps.MQ.Call(callCtx, frame)
	if err != nil {
		return nil, err
	}
	if mqErr := reply.AsError(); mqErr != nil {
		return nil, &chainStepError{Step: step, Errn: reply.Errn, Errm: reply.ErrMsg}
	}
	_, out, err = wireParseReply(spec, reply.Body)
	if err != nil {
		return nil, fmt.Errorf("chain %s 응답 파싱: %w", step, err)
	}
	return out, nil
}

// runLoginChain 은 엔진 인증 사슬을 완주한다:
//
//	① W1101S02 인증서 인증  → cifNo + lgnId (usid 공란 — 인증 전)
//	   (② W1107A01 OTP seam — 이번 범위 제외, --login-otp 도입 시 여기 삽입)
//	③ W1130A02 세션개설     → lgnIdntCon + 영업일 (usid=lgnId, loip=클라IP)
//
// fxUserNo ≡ lgnId (W1101S02 가 CSC004M/CSC005R upsert — 소스 확인,
// docs/engine-auth-login-mapping.md §6.2).
func runLoginChain(ctx context.Context, deps *Deps, signMsg, clientIP string) (*chainResult, error) {
	cfg := deps.LoginChain

	// ① 인증서 인증.
	out1, err := callChainStep(ctx, deps, "cert", cfg.certAlias(), "",
		map[string]interface{}{"loip": clientIP},
		map[string]interface{}{"prGb": "1", "signMsg": signMsg})
	if err != nil {
		return nil, err
	}
	res := &chainResult{
		CifNo:   strField(out1, "cifNo"),
		LgnID:   strField(out1, "lgnId"),
		SvrCert: strField(out1, "svrCert"),
	}
	if res.LgnID == "" {
		return nil, fmt.Errorf("chain cert: 응답에 lgnId 없음")
	}

	// ② OTP seam — 이번 범위 제외 (스펙 §2). 도입 시 W1107A01 호출 삽입 지점.

	// ③ 세션개설.
	out3, err := callChainStep(ctx, deps, "session", cfg.sessionAlias(), res.LgnID,
		map[string]interface{}{"loip": clientIP},
		map[string]interface{}{"prGb": "1", "fxUserNo": res.LgnID, "lgnId": res.LgnID})
	if err != nil {
		return nil, err
	}
	res.LgnIdntCon = strField(out3, "lgnIdntCon")
	if res.LgnIdntCon == "" {
		return nil, fmt.Errorf("chain session: 응답에 lgnIdntCon 없음")
	}
	res.FwdPreChkPopYn = strField(out3, "fwdPreChkPopYn")
	res.ApllBsopYmd = strField(out3, "apllBsopYmd")
	res.WlbrYmd = strField(out3, "wlbrYmd")
	res.NxtBsopYmd = strField(out3, "nxtBsopYmd")
	res.LgnTs = strField(out3, "lgnTs")
	return res, nil
}

// strField 는 svcio 출력 map 의 문자열 필드를 trim 해서 꺼낸다.
func strField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// svcio import 는 spec 타입 참조가 없으면 제거할 것 (컴파일러가 알려줌).
var _ = svcio.Registry{}
```

(주의: 마지막 `var _` 는 placeholder 가 아니라 import 정리 지침 — 구현 후 `svcio` 참조가 없으면 import 와 함께 삭제.)

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/handlers/ -run TestRunLoginChain -v -race`
Expected: PASS (4개)

- [x] **Step 5: Commit**

```bash
git add internal/api/handlers/loginchain.go internal/api/handlers/loginchain_test.go internal/api/handlers/handlers.go
git commit -m "feat(api): 엔진 인증 사슬 오케스트레이터 — W1101S02→W1130A02"
```

---

### Task 3: login.go chain 분기 + finishLogin 추출

**Files:**
- Modify: `internal/api/handlers/login.go` (세션/JWT/refresh 발급부를 `finishLogin` 으로 추출, chain 분기 추가)
- Modify: `internal/api/handlers/loginchain.go` (HTTP 층 `loginViaChain` 추가)
- Test: `internal/api/handlers/loginchain_test.go` (핸들러 레벨 테스트 추가)

- [x] **Step 1: Write the failing test**

`loginchain_test.go` 에 추가:

```go
func TestLoginChainHandlerSuccess(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
	store := newStoreForTest(t) // login_test.go 의 기존 헬퍼 재사용
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
	// 응답 data — 영업일 노출 + cifNo 미노출.
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
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
```

(`net/http` / `net/http/httptest` import 추가. `newStoreForTest` 시그니처가 다르면 login_test.go 의 실제 이름/시그니처에 맞출 것.)

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/handlers/ -run TestLoginChainHandler -v`
Expected: FAIL — chain 분기가 없어 legacy 경로로 흘러 500/502 계열 응답

- [x] **Step 3: Write implementation**

3a. `login.go` — 세션 생성 + JWT/refresh 발급부 (기존 153~280행) 를 함수로 추출.
기존 코드를 **그대로 옮기고** 파라미터만 주입 (동작 무변경):

```go
// finishLogin 은 인증 완료 후 공통 마무리 — 세션 저장 + JWT/refresh 발급 + 응답.
// legacy (cookie_t) / chain (lgnIdntCon) 양쪽이 공유한다.
// cookie 와 lgnIdntCon/cifNo 는 상호 배타 — 모드에 따라 한쪽만 채워진다.
func finishLogin(deps *Deps, w http.ResponseWriter, r *http.Request,
	channel, usid string, cookie *mymq.Cookie, lgnIdntCon, cifNo string,
	dataOut json.RawMessage,
) {
	sid, err := auth.NewSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "rng", err.Error())
		return
	}
	ttl := deps.SessionTTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	now := time.Now()
	// Profile 권위 출처 — UserProfileResolver 로 usid → (Site, Tier). (기존 주석 유지)
	var site, tier string
	if deps.UserProfiles != nil {
		up, err := deps.UserProfiles.Resolve(r.Context(), usid)
		if err == nil {
			site = string(up.Site)
			tier = string(up.Tier)
		} else if !errors.Is(err, auth.ErrUserProfileNotFound) {
			deps.Logger.WarnContext(r.Context(), "UserProfile Resolve 실패 — 빈 Profile 로 진행",
				slog.String("usid", usid), slog.Any("error", err))
		}
	}
	sess := &auth.Session{
		ID:         sid,
		Usid:       usid,
		Channel:    channel,
		Cookie:     cookie,
		LgnIdntCon: lgnIdntCon,
		CifNo:      cifNo,
		IssuedAt:   now,
		ExpiresAt:  now.Add(ttl),
		Profile: session.Profile{
			Channel: session.Channel(channel),
			Site:    session.Site(site),
			Tier:    session.Tier(tier),
		},
		LogonID: session.LogonID(usid),
	}
	if err := deps.Sessions.Put(r.Context(), sess); err != nil {
		deps.Logger.ErrorContext(r.Context(), "세션 저장 실패", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "session_store", err.Error())
		return
	}

	deps.Logger.InfoContext(r.Context(), "로그인 성공",
		slog.String("usid", usid), slog.String("sid", sid), slog.String("chan", channel))

	resp := LoginResponse{
		SessionID: sid,
		ExpiresAt: sess.ExpiresAt,
		Channel:   channel,
		Data:      dataOut,
	}
	// (이하 기존 JWT/refresh 발급 블록을 usid/channel/sid/now 기준으로 그대로 이동)
	...
	writeJSON(w, http.StatusOK, resp)
}
```

기존 `Login` 의 legacy 경로는 `finishLogin(deps, w, r, channel, usid, reply.Cookie, "", "", dataOut)` 호출로 축소. **기존 테스트가 전부 green 인 것으로 리팩토링 무손상을 검증한다.**

3b. `login.go` 의 `Login` — 페이로드 디코딩/channel 결정 직후에 분기:

```go
		if deps.LoginChain != nil {
			loginViaChain(deps, w, r, req, channel)
			return
		}
```

3c. `loginchain.go` 에 HTTP 층 추가:

```go
// chainLoginData 는 chain 모드의 /v1/login 요청 data.
type chainLoginData struct {
	SignMsg string `json:"signMsg"`
}

// loginViaChain 은 chain 모드의 /v1/login 처리 (스펙 §4).
func loginViaChain(deps *Deps, w http.ResponseWriter, r *http.Request,
	req LoginRequest, channel string,
) {
	var in chainLoginData
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", "data 파싱 실패: "+err.Error())
			return
		}
	}
	if in.SignMsg == "" {
		writeError(w, http.StatusBadRequest, "missing_sign_msg",
			"chain 로그인은 data.signMsg (인증서명) 필수")
		return
	}

	res, err := runLoginChain(r.Context(), deps, in.SignMsg, clientIPOf(r))
	if err != nil {
		var stepErr *chainStepError
		if errors.As(err, &stepErr) {
			// 엔진 거부 — errn 그대로 노출 (위임 원칙, legacy 와 동일 포맷).
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":   "login_failed",
				"errn":    stepErr.Errn,
				"errm":    stepErr.Errm,
				"message": stepErr.Error(),
			})
			return
		}
		deps.Logger.WarnContext(r.Context(), "chain 로그인 실패",
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			slog.Any("error", err))
		status, code, msg := mapBrokerError(err)
		writeError(w, status, code, msg)
		return
	}

	// 응답 data — cifNo 는 노출하지 않는다 (실명번호, 스펙 §4).
	dataOut, _ := json.Marshal(map[string]string{
		"lgnId":          res.LgnID,
		"svrCert":        res.SvrCert,
		"fwdPreChkPopYn": res.FwdPreChkPopYn,
		"apllBsopYmd":    res.ApllBsopYmd,
		"wlbrYmd":        res.WlbrYmd,
		"nxtBsopYmd":     res.NxtBsopYmd,
		"lgnTs":          res.LgnTs,
	})
	finishLogin(deps, w, r, channel, res.LgnID, nil, res.LgnIdntCon, res.CifNo, dataOut)
}

// clientIPOf 는 클라이언트 IP — X-Forwarded-For (edge 뒤) 첫 항목, 없으면 RemoteAddr.
func clientIPOf(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
```

(`net`, `net/http`, `errors`, `log/slog`, `middleware` import 추가.)

- [x] **Step 4: Run tests to verify all pass**

Run: `go test ./internal/api/handlers/ -race`
Expected: PASS — 신규 3개 + **기존 login/refresh/logout 테스트 전부** (finishLogin 리팩토링 무손상)

- [x] **Step 5: Commit**

```bash
git add internal/api/handlers/login.go internal/api/handlers/loginchain.go internal/api/handlers/loginchain_test.go
git commit -m "feat(api): /v1/login chain 모드 — 사슬 오케스트레이션 + finishLogin 공통화"
```

---

### Task 4: logout — W1130A03 반납

**Files:**
- Modify: `internal/api/handlers/logout.go` (chain 분기)
- Test: `internal/api/handlers/loginchain_test.go` (추가)

- [x] **Step 1: Write the failing test**

```go
func TestLogoutChainReturnsLgnIdntCon(t *testing.T) {
	reg := newChainSvcIO(t)
	var calls []*mymq.FrameInput
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
	caller := chainFakeCaller(t, reg, &calls, map[string]func() (*mymq.Reply, error){
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
```

(`pkg/auth` import 추가.)

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/handlers/ -run TestLogoutChain -v`
Expected: FAIL — W1130A03 미호출 (calls=0)

- [x] **Step 3: Write implementation**

`logout.go` — 기존 `p.Cookie != nil` LOGOFF 블록 **앞에** chain 분기 추가:

```go
		// chain 모드 — 세션의 lgnIdntCon 을 W1130A03 으로 반납.
		// 실패해도 세션 삭제는 진행 (멱등 — 기존 LOGOFF semantics 와 동일).
		if deps.LoginChain != nil && p.SessionID != "" {
			if sess, err := deps.Sessions.Get(r.Context(), p.SessionID); err == nil && sess.LgnIdntCon != "" {
				_, err := callChainStep(r.Context(), deps, "logout",
					deps.LoginChain.logoutAlias(), sess.Usid,
					map[string]interface{}{"loip": clientIPOf(r)},
					map[string]interface{}{
						"prGb":       "1",
						"fxUserNo":   sess.Usid,
						"lgnIdntCon": sess.LgnIdntCon,
					})
				if err != nil {
					var stepErr *chainStepError
					if errors.As(err, &stepErr) {
						brokerErrn, brokerErrm = stepErr.Errn, stepErr.Errm
					}
					deps.Logger.WarnContext(r.Context(), "W1130A03 반납 실패 — 세션은 삭제 진행",
						slog.String("sid", p.SessionID), slog.Any("error", err))
				}
			}
		}
```

(`errors` import 추가. `brokerErrn/brokerErrm` 은 기존 변수 재사용 — 선언 위치가 chain 분기보다 아래면 선언을 위로 이동.)

- [x] **Step 4: Run tests to verify all pass**

Run: `go test ./internal/api/handlers/ -race`
Expected: PASS (신규 2개 + 기존 logout 테스트 무손상)

- [x] **Step 5: Commit**

```bash
git add internal/api/handlers/logout.go internal/api/handlers/loginchain_test.go
git commit -m "feat(api): chain 로그아웃 — W1130A03 lgnIdntCon 반납"
```

---

### Task 5: Config + server 배선 (--login-mode)

**Files:**
- Modify: `internal/api/config.go` (LoginMode + alias 3종 + 검증)
- Modify: `internal/api/server.go:368-381` (Deps 배선 + 부팅 로그)
- Test: `internal/api/config_test.go` (없으면 신규)

- [x] **Step 1: Write the failing test**

`internal/api/config_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

func TestLoadConfigLoginModeChainRequiresSvcInc(t *testing.T) {
	_, err := LoadConfig([]string{"--login-mode=chain"})
	if err == nil || !strings.Contains(err.Error(), "svc-inc-dir") {
		t.Errorf("chain 은 svc-inc-dir 필수여야 함: %v", err)
	}
}

func TestLoadConfigLoginModeChainOK(t *testing.T) {
	cfg, err := LoadConfig([]string{"--login-mode=chain", "--svc-inc-dir=/tmp/inc"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginMode != "chain" {
		t.Errorf("LoginMode=%q", cfg.LoginMode)
	}
}

func TestLoadConfigLoginModeInvalid(t *testing.T) {
	_, err := LoadConfig([]string{"--login-mode=banana"})
	if err == nil {
		t.Error("잘못된 login-mode 는 에러여야 함")
	}
}

func TestLoadConfigLoginModeDefaultLegacy(t *testing.T) {
	cfg, err := LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginMode != "" && cfg.LoginMode != "legacy" {
		t.Errorf("기본은 legacy: %q", cfg.LoginMode)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestLoadConfigLoginMode -v`
Expected: FAIL — `flag provided but not defined: -login-mode`

- [x] **Step 3: Write implementation**

`config.go` — Config struct (SvcCommonHeaderFile 아래):

```go
	// LoginMode — /v1/login 동작. "legacy"(기본, 빈값 동일) = 단일 LOGON +
	// cookie_t. "chain" = 엔진 인증 사슬 (W1101S02→W1130A02, cookie 없음).
	// chain 은 SvcIncDir 필수 — 전문 조립이 svc I/O 명세에 의존.
	// docs/superpowers/specs/2026-07-20-engine-login-chain-design.md 참조.
	LoginMode string

	// chain 모드 단계별 alias override (비면 W1101S02/W1130A02/W1130A03).
	LoginCertAlias    string
	LoginSessionAlias string
	LoginLogoutAlias  string
```

env (LoadConfig 의 env 절):

```go
	if v := os.Getenv("WTG_API_LOGIN_MODE"); v != "" {
		cfg.LoginMode = v
	}
```

flag (flag 절):

```go
	fs.StringVar(&cfg.LoginMode, "login-mode", cfg.LoginMode, "로그인 모드: legacy(기본) | chain (엔진 인증 사슬 W1101S02→W1130A02). chain 은 --svc-inc-dir 필수")
	fs.StringVar(&cfg.LoginCertAlias, "login-cert-alias", cfg.LoginCertAlias, "chain ① 인증서 인증 alias (기본 W1101S02)")
	fs.StringVar(&cfg.LoginSessionAlias, "login-session-alias", cfg.LoginSessionAlias, "chain ③ 세션개설 alias (기본 W1130A02)")
	fs.StringVar(&cfg.LoginLogoutAlias, "login-logout-alias", cfg.LoginLogoutAlias, "chain 로그아웃 반납 alias (기본 W1130A03)")
```

검증 (fs.Parse 후, ParseSeedPolicy 검증 옆):

```go
	switch cfg.LoginMode {
	case "", "legacy":
	case "chain":
		if cfg.SvcIncDir == "" {
			return cfg, fmt.Errorf("--login-mode=chain 은 --svc-inc-dir 필수 (전문 조립이 svc I/O 명세 의존)")
		}
	default:
		return cfg, fmt.Errorf("--login-mode: %q — legacy | chain 중 하나", cfg.LoginMode)
	}
```

`server.go` — deps 조립 직후:

```go
	if s.cfg.LoginMode == "chain" {
		deps.LoginChain = &handlers.LoginChainConfig{
			CertAlias:    s.cfg.LoginCertAlias,
			SessionAlias: s.cfg.LoginSessionAlias,
			LogoutAlias:  s.cfg.LoginLogoutAlias,
		}
		s.logger.Info("로그인 chain 모드 활성 — 엔진 인증 사슬",
			slog.String("cert", deps.LoginChain.CertAlias),
			slog.String("session", deps.LoginChain.SessionAlias))
	}
```

- [x] **Step 4: Run tests to verify all pass**

Run: `go test ./internal/api/... -race`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/api/config.go internal/api/config_test.go internal/api/server.go
git commit -m "feat(api): --login-mode=chain 스위치 + 부팅 검증 (svc-inc-dir 필수)"
```

---

### Task 6: 전체 게이트 + 문서

**Files:**
- Modify: `docs/operations.md` (mci-api flag 카탈로그에 --login-mode 절 추가)
- Modify: `docs/auth.md` (§3 로그인 흐름에 chain 모드 문단 + 스펙 링크)
- Modify: `CLAUDE.md` + `AGENTS.md` (mci-api 행에 chain 모드 한 줄 — 동기화 필수)

- [x] **Step 1: Run full CI gate**

Run: `make ci`
Expected: lint + vulncheck + test-race + build 전부 green

- [x] **Step 2: Update docs**

- `docs/operations.md` mci-api flag 표에: `--login-mode` / `--login-*-alias` 4행 + "chain 은 --svc-inc-dir 필수, etcd 라우팅에 W1101S02/W1130A02/W1130A03 alias 없으면 dom/<alias> fallback" 주석.
- `docs/auth.md` §3 에 문단: "chain 모드 (NH 엔진 사슬)" — 스펙 문서 링크 + cookie_t 대신 lgnIdntCon 보관.
- `CLAUDE.md`/`AGENTS.md` mci-api 행: "`/v1/login` chain 모드 (`--login-mode=chain`, W1101S02→W1130A02 사슬)" 추가.

- [x] **Step 3: Commit**

```bash
git add docs/operations.md docs/auth.md CLAUDE.md AGENTS.md
git commit -m "docs: chain 로그인 모드 운영 플래그/인증 흐름 반영"
```

---

## EC2 최종 테스트 절차 (참고 — 코드 범위 밖)

1. **svcio 실 헤더 검증**: mci-api 부팅 로그의 `svc I/O 명세 로드 loaded/failed` 에서
   `W1101S02`/`W1130A02`/`W1130A03` 3개가 loaded 에 포함되는지. W1101S02.h 는
   `cert_out` 등 부가 struct 가 있어 파서가 거부하면 → trn 헤더를 WTG 측 spec dir 에
   `_I/_O` 만 남긴 사본으로 두는 fallback.
2. **라우팅 시드**: `deploy/seed-catalog.sh` 에 `W1101S02`/`W1130A02`/`W1130A03`
   → `dom/<code>` 룰 3건 추가 (또는 패턴 룰 `W11*` 활용). fallback 이 있으므로
   미시드여도 동작은 하나, 정책/감사 일관성 위해 시드 권장.
3. **smoke**: `curl -XPOST /v1/login -d '{"data":{"signMsg":"..."}}'` → 200 +
   session/JWT → `/v1/tx` (svcio 트랜잭션 1건) → `/v1/logout` → 엔진
   `TB_FXB_CSC015L` 에서 LGN_YN 'Y'→'N' 확인.
4. **주의 — DB**: W1101S02/W1130A02 는 Oracle 필수. db2stub (DB-free) 빌드 trn 으로는
   사슬이 실값을 못 돌려줌 → LFORA 실 DB 물린 svc 로 테스트.

## 엔진(trn) 측 수정 후보 (사용자 판단 필요)

| # | 항목 | 내용 | 필수? |
|---|------|------|------|
| 1 | W1101S02 인증서 검증 모듈 | `cert_out` 생성부가 실제 공인인증서 라이브러리 의존. EC2 에 모듈 미설치면 dev bypass (서명 파싱 없이 user_id 추출) 스위치 필요 | 테스트 환경에 따라 |
| 2 | W1130A02 중복로그인 통보 | 기존 세션 강제 로그아웃 시 broadcast+SMS 코드가 통째 주석 (조사노트 §6.3). 밀어내기 실시간 통보를 원하면: 주석 해제(레거시 broadcast) 또는 **cside/wtgpush 로 mci-push HTTP push** (권장 — Track B) | 선택 |
| 3 | W1107A01 | 이번 범위 제외 — 수정 불필요 | — |
| 4 | W1101S02.h 헤더 정리 | svcio 파서가 부가 struct 때문에 거부하면 `_I/_O` 만 남긴 사본 필요 (엔진 수정 아님, 명세 사본) | 파서 결과에 따라 |

## Self-Review 결과

- 스펙 커버리지: §3 오케스트레이터(Task 2)·login 분기(Task 3)·Session 확장(Task 1)·플래그(Task 5), §5 로그아웃(Task 4), §6 에러(Task 2/3), §7 테스트(각 Task) — 전부 매핑. §4 의 svrCert 노출 포함(Task 3 dataOut). OTP seam 은 runLoginChain 주석으로 자리 표시(스펙 §8).
- placeholder: 없음 (Task 3 의 `...` 는 "기존 코드 블록 그대로 이동" 지시 — 신규 작성 아님).
- 타입 일관성: `chainStepError`/`runLoginChain`/`callChainStep`/`clientIPOf`/`finishLogin` 명칭 Task 간 일치 확인.
