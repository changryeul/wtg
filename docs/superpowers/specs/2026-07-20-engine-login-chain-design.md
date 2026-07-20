# 엔진 인증 사슬 로그인 (chain 모드) — 설계 스펙

- 작성일: 2026-07-20
- 근거 조사: `docs/engine-auth-login-mapping.md` (2026-07-18, nh 소스 추적)
- 상태: 사용자 승인 완료 (접근안 B — 별도 chain 오케스트레이터)

## 1. 배경 / 목표

기존 WTG `/v1/login` 은 단일 LOGON 트랜잭션 → `reply.Cookie(cookie_t)` → 세션 모델.
NH 엔진의 실제 로그인은 **다단계 사슬**이며 cookie_t 발급이 없다:

```
① W1101S02 공인인증서 인증  → cifNo(실명번호) + lgnId
② W1107A01 보안매체/OTP     → 성공/실패            ← 이번 범위 제외 (seam 만)
③ W1130A02 로그인처리        → lgnIdntCon(세션 열쇠) + 영업일
```

소스 추적으로 확정된 사실 (설계의 전제):

- **fxUserNo ≡ lgnId** — W1101S02 가 인증서 검증 후 `CSC004M`(FX_USER_NO=LGN_ID=cert user_id)
  과 `CSC005R`(사용자고객번호관계) 를 자동 upsert 한다. 별도 매핑 조회 불필요.
- **`lgnIdntCon` 은 W1130A02(로그인)/W1130A03(로그아웃)만 소비** — 거래 서비스는
  세션 열쇠를 검증하지 않는다. 실효 세션은 지금처럼 WTG JWT/Redis 가 담당하고,
  `lgnIdntCon` 은 로그아웃 반납용으로만 보관한다.
- W1130A02 는 중복 로그인 시 기존 세션을 DB 에서 강제 로그아웃 처리한다 (WTG 개입 불필요).

## 2. 확정 결정

| 결정 | 내용 |
|-----|------|
| API 형태 | **단일샷** `POST /v1/login` — WTG 가 ①③ 순차 오케스트레이션 (서버 상태 없음) |
| 레거시 호환 | cookie_t 기반 LOGON 경로 **유지** + `--login-mode=legacy\|chain` 스위치 (기본 legacy) |
| OTP (②) | **이번 범위 제외** — ①과 ③ 사이 no-op seam 만 남김 |
| JWT/Profile | **무변경** — Claims(usid/Site/Tier), UserProfileResolver 그대로. tier 제거는 별도 트랙 |
| 구조 | 접근안 B — `internal/api/handlers/loginchain.go` 별도 오케스트레이터 |

## 3. 아키텍처

### 3.1 loginchain.go (신규)

HTTP 와 무관하게 `deps.MQ.Call` + `pkg/svcio` 만으로 동작하는 오케스트레이터.

```go
type LoginChainConfig struct {
    CertAlias    string // 기본 "W1101S02"
    SessionAlias string // 기본 "W1130A02"
    LogoutAlias  string // 기본 "W1130A03"
}

type chainResult struct {
    LgnID      string // = fxUserNo
    CifNo      string
    LgnIdntCon string
    // 클라 표시용 부가 정보
    FwdPreChkPopYn, ApllBsopYmd, WlbrYmd, NxtBsopYmd, LgnTs string
}

func runLoginChain(ctx, deps, signMsg, clientIP string) (*chainResult, error)
```

- alias → exchange/routing_key 는 `/v1/tx` 와 동일하게 라우팅 registry 로 resolve
  (NH 시드 기준 exchange=`dom`). registry 미구성 시 `dom/<alias>` fallback.
- 전문 조립/파싱은 기존 `wireBuildBody` / `wireParseReply` (pkg/svcio) 재사용.

### 3.2 login.go (분기 추가)

`deps.LoginChain != nil` 이면 chain 분기:

1. `req.Data.signMsg` 필수 (누락 → 400)
2. `runLoginChain` 실행
3. 세션 생성: `Usid=lgnId`, `Cookie=nil`, `LgnIdntCon`/`CifNo` 저장
4. JWT/refresh 발급 — 기존 로직 그대로 (Profile 은 UserProfileResolver 로 resolve)

legacy 경로 코드는 무변경.

### 3.3 mci-api 부팅 플래그

- `--login-mode=legacy|chain` (env `WTG_LOGIN_MODE`, 기본 `legacy`)
- chain 은 `--svc-inc-dir`(svcio Registry) 필수 — 없으면 부팅 시 에러로 fail-fast.
- alias 3개 override 플래그 (`--login-cert-alias` 등, 기본값이면 생략 가능).

### 3.4 pkg/auth.Session 확장

```go
LgnIdntCon string // 엔진 세션 열쇠 (로그아웃 반납용). chain 모드에서만 채움
CifNo      string // 실명번호 (고객 식별자 — 추후 고객별 스프레드 조회 대비)
```

- Redis/Memory store 직렬화에 포함.
- `Cookie` nil 허용 — `/v1/tx` 미들웨어의 cookie 첨부는 nil 이면 기존처럼 미첨부.

## 4. 데이터 흐름 (wire 상세)

```
POST /v1/login {"data":{"signMsg":"<인증서명>"}}

① dom/W1101S02
   [COMHDR trxc=W1101S02, usid='' (인증 전), ctyp=A, cont=H]
   [W1101S02_I: prGb='1', signMsg]
   → [W1101S02_O: cifNo, lgnId, otinYn, svrCert]

   (OTP seam — 현재 no-op)

③ dom/W1130A02
   [COMHDR trxc=W1130A02, usid=lgnId, loip=클라이언트IP, cont=H]  (maca 공란 — web)
   [W1130A02_I: prGb='1', fxUserNo=lgnId, fxUserNm='', lgnId]
   → [W1130A02_O: lgnIdntCon, fwdPreChkPopYn, apllBsopYmd, wlbrYmd, nxtBsopYmd, lgnTs]

응답 200:
{
  "session_id": "...", "access_token": "...", "refresh_token": "...",
  "data": {"lgnId":"...","fwdPreChkPopYn":"...","apllBsopYmd":"...",
           "wlbrYmd":"...","nxtBsopYmd":"...","lgnTs":"..."}
}
```

- `svrCert`(서버 key, ≤5120B) 는 응답 data 에 **포함**한다 — 클라이언트 인증서
  협상 후속 단계에서 필요할 수 있고, 제외는 언제든 가능하지만 추가는 재배포다.
- `cifNo` 는 응답에 노출하지 않는다 (실명번호 — 세션 내부 보관만).
- 클라이언트 IP 는 기존 미들웨어의 remote addr 추출 규칙 재사용 (X-Forwarded-For 정책 포함).

## 5. 로그아웃

`logout.go` 분기 추가 — 세션 로드 후 `LgnIdntCon != ""` 이면:

```
dom/W1130A03
  [COMHDR usid=usid]
  [W1130A03_I: prGb='1', fxUserNo=usid, fxUserNm='', lgnIdntCon]
```

기존 semantics 유지: **엔진 호출이 실패해도 WTG 세션/refresh 는 항상 삭제** (멱등).
errn 은 응답에 `broker_errn` 으로 동봉 (기존 패턴).

## 6. 에러 처리

| 상황 | 응답 |
|-----|------|
| signMsg 누락 (chain 모드) | 400 `bad_request` |
| ① errn≠0 (인증서 거부) | 401 `login_failed` + errn/errm 그대로 (위임 원칙) |
| ③ errn≠0 | 401 `login_failed` + errn/errm. ③ 실패 시 엔진 DB 에 기록 없음 — 보상 불필요 |
| broker 통신 오류 | 기존 `mapBrokerError` 매핑 (502/504 등) |
| chain 모드 + svcio 명세 미등록 | 부팅 시 fail-fast. 런타임 도달 시 503 |

## 7. 테스트 (로컬 DB 없음 전제)

- **단위**: fake `MQ.Call` 이 svcio 로 조립한 고정폭 응답을 돌려주는 스크립트 방식
  (기존 `wire_test.go`/`login_test.go` 패턴). testdata 에 W1101S02/W1130A02/W1130A03
  미니 `.h` 명세 추가.
  - 성공 사슬 (① → ③ → 세션/JWT 발급, LgnIdntCon 저장 확인)
  - ① 거부 (401 + errn passthrough)
  - ③ 거부 (401)
  - signMsg 누락 (400)
  - legacy 모드 회귀 무손상 (기존 테스트 green)
  - chain 로그아웃 (W1130A03 호출 + 세션 삭제, 엔진 실패에도 삭제)
- **통합**: 기존 `MYMQD_HOST` 게이트 재사용. 실 엔진(EC2, DB-free trn 빌드) 검증은 별도.

## 8. 범위 밖 (명시)

- ② W1107A01 OTP 검증 — seam 만. 추후 `--login-otp` 스위치로 구현 예정.
- JWT tier 제거 / 고객별 스프레드 기반 quote 매칭 — 별도 트랙
  (`docs/customer-spread-sync.md` 참조).
- 중복 로그인 강제종료 push 통보 — 엔진측 주석 처리 상태 (조사노트 §6.3), WTG 미개입.
