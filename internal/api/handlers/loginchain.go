package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/winwaysystems/wtg/pkg/mymq"
	"github.com/winwaysystems/wtg/pkg/routing"
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
	defaultChainExchange = "dom" // NH trn 서비스 exchange 컨벤션 (deploy/seed-catalog.sh)
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
	LgnID      string // = fxUserNo (W1101S02 가 CSC004M/CSC005R upsert — 조사노트 §6.2 해소)
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
	Step string // "cert" | "session" | "logout"
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
