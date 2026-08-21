package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/winwaysystems/wtg/internal/api/middleware"
)

// LoginValidateConfig — 검증형(validate) 로그인 설정.
//
// yuanta T1204S01 류: 엔진이 쿠키/세션토큰을 발급하지 않고 성공여부 필드
// (loginYN)만 반환한다. WTG 가 id/pw 를 그 서비스로 검증한 뒤 성공이면
// *자체* 세션/JWT 를 발급한다 (usid = 입력 사용자ID). 이후 거래 svc 는
// COMHDR.usid 로 사용자를 식별하므로 엔진 쿠키가 필요 없다.
//
// chain(NH) / legacy 와 상호배타가 아니라 *요청 단위* 로 분기한다 — 로그인
// 요청이 로그인 서비스 alias(예 T1204S01)를 지정하면 이 모드로 동작하고,
// 없으면 설정된 모드(chain/legacy). NH=W·yuanta=T 로 서비스가 안 겹치므로
// alias 하나로 회사가 갈린다.
type LoginValidateConfig struct {
	SuccessField string // 출력의 성공여부 필드 (기본 "loginYN")
	SuccessValue string // 성공으로 판정할 값 (기본 "1")
	UsidField    string // 입력에서 세션 usid 로 쓸 필드 (기본 "user_id")
}

const (
	defaultValidateSuccessField = "loginYN"
	defaultValidateSuccessValue = "1"
	defaultValidateUsidField    = "user_id"
)

func (c *LoginValidateConfig) successField() string {
	if c != nil && c.SuccessField != "" {
		return c.SuccessField
	}
	return defaultValidateSuccessField
}

func (c *LoginValidateConfig) successValue() string {
	if c != nil && c.SuccessValue != "" {
		return c.SuccessValue
	}
	return defaultValidateSuccessValue
}

func (c *LoginValidateConfig) usidField() string {
	if c != nil && c.UsidField != "" {
		return c.UsidField
	}
	return defaultValidateUsidField
}

// loginViaValidate — 검증형 로그인 (req.Alias 지정 시). alias 서비스(예 T1204S01)를
// svcio 로 조립·호출해 성공여부 필드만 확인하고, 성공이면 WTG 세션/JWT 를 발급한다.
// 엔진 쿠키/lgnIdntCon 없음 — finishLogin 이 세션에 usid 만 보관.
func loginViaValidate(deps *Deps, w http.ResponseWriter, r *http.Request,
	req LoginRequest, channel string,
) {
	cfg := deps.LoginValidate

	var input map[string]interface{}
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &input); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json",
				"data 는 JSON object 여야 함 (검증 로그인): "+err.Error())
			return
		}
	}

	// 세션 usid — 입력의 사용자ID 필드, 없으면 header.usid fallback.
	usid := strField(input, cfg.usidField())
	if usid == "" {
		if v, ok := req.Header["usid"].(string); ok {
			usid = strings.TrimSpace(v)
		}
	}
	if usid == "" {
		writeError(w, http.StatusBadRequest, "no_usid",
			"검증 로그인은 data."+cfg.usidField()+" (세션 식별자) 필수")
		return
	}

	// alias 서비스 호출 — 조립/라우팅/파싱/COMHDR eflg 체크는 chain 과 공유.
	out, err := callChainStep(r.Context(), deps, "validate", req.Alias, usid, channel, req.Header, input)
	if err != nil {
		var stepErr *chainStepError
		if errors.As(err, &stepErr) {
			// 엔진 거부 (errn 또는 COMHDR eflg) — 그대로 노출 (위임 원칙).
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":   "login_failed",
				"errn":    stepErr.Errn,
				"rcod":    stepErr.Rcod,
				"errm":    stepErr.Errm,
				"message": stepErr.Error(),
			})
			return
		}
		deps.Logger.WarnContext(r.Context(), "검증 로그인 실패",
			slog.String("rid", middleware.RequestIDFromContext(r.Context())),
			slog.Any("error", err))
		status, code, msg := mapBrokerError(err)
		writeError(w, status, code, msg)
		return
	}

	// 성공여부 필드 확인 — 실패면 엔진 msg 그대로 노출 (인증 거부 = 401).
	if strField(out, cfg.successField()) != cfg.successValue() {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "login_failed",
			"message": strField(out, "msg"),
			"data":    out,
		})
		return
	}

	// 성공 — output(loginYN/changePWYN/msg 등)을 클라 표시용 data 로 첨부.
	dataOut, _ := json.Marshal(out)
	finishLogin(deps, w, r, channel, usid, nil, "", "", dataOut)
}
