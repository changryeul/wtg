# WTG 인증/권한 명세

WTG (Winway Trading Gateway) 의 인증·권한 처리 설계.
운영팀/보안팀 검토용 명세서이며, 결정된 항목은 코드(`pkg/auth/` 등)와 운영
구성(JWT 시크릿, Redis 등)에 반영된다.

마지막 갱신: 2026-05-02
결정 상태: 핵심 원칙 확정, 세부 옵션 합의 대기

---

## 1. 핵심 원칙

> **Authentication (사용자가 누구인가)** — WTG 가 처리.
> **Authorization (사용자가 무엇을 할 수 있는가)** — 기존 매매 엔진이 처리.

기존 MyMQ 매매 엔진은 `cookie_t` 기반 인증/권한 검증이 이미 구현되어 있다.
WTG 는 **이를 재구현하지 않으며**, web 세션을 cookie_t 로 매핑해서 그대로
broker 로 전달(passthrough)한다.

이 원칙의 결과:

- **권한 정책 단일 출처** — 거래 한도, 통화쌍 허용, 거래시간 등 룰은 매매
  엔진 한 곳에서만 관리. WTG 코드에는 비즈니스 권한 체크 로직이 없다.
- **WTG 는 얇은 게이트웨이** — JWT 검증, rate limit, MFA 같은 web-layer
  보안만 책임지고, 비즈니스 거부 사유는 항상 엔진의 응답을 기다린 후 그대로
  전달.
- **기존 엔진 무수정** — anyone/websocket 어댑터처럼 WTG 는 엔진 입장에서
  하나의 클라이언트 군일 뿐.

---

## 2. 책임 분담

| 항목                 | WTG                         | 매매 엔진                |
| ------------------ | --------------------------- | -------------------- |
| 사용자 본인 확인 (id/pw)  | ✅ 1차 입력 받음 → 엔진에 LOGON 위임   | ✅ 비밀번호 검증, cookie 발급 |
| MFA (TOTP)         | ✅ web 에서 의무화                | ❌                    |
| JWT 발급/검증          | ✅                           | ❌                    |
| 세션 만료 / 갱신         | ✅ web 세션                    | (별개로) cookie 유효성 검증  |
| 거래 권한              | ❌                           | ✅                    |
| 주문 한도 (1회/일별)      | ❌                           | ✅                    |
| 통화쌍 활성화 여부         | ❌                           | ✅                    |
| 거래시간 검증            | ❌                           | ✅                    |
| Slippage 한도        | ❌                           | ✅                    |
| 동시 로그인 정책          | ✅ web 세션 단위                 | ✅ 엔진 측 KILL 명령 활용    |
| Rate limit (요청 빈도) | ✅ IP/user 단위                | ✅ 엔진 측 사용자별 TPS      |
| 봇/이상 트래픽 탐지        | ✅                           | ❌                    |
| Audit log          | ✅ login/logout/security 이벤트 | ✅ 거래 감사              |

---

## 3. 흐름 — 로그인

```
[브라우저]
   │ POST /v1/login {usid, password, totp}
   │ HTTPS only
   ▼
[mci-edge-api] (DMZ)
   │ - rate limit (IP)
   │ - 봇 탐지
   │ gRPC mTLS
   ▼
[mci-api] (Internal)
   │ 1. TOTP 검증 (Redis 의 사용자 비밀키)
   │ 2. mymq.Call(LOGON 트랜잭션) — cookie 첨부 없이
   │
   ▼
[mymqd] → [매매 엔진]
              3. id/pw 검증 (기존 로직)
              4. cookie_t 발급
              ← reply (cookie_t 동봉)
   ▲
   │ 5. mci-api 가 cookie_t 수신
   │ 6. session_id 생성, Redis 저장:
   │      key  : "wtg:sess:<session_id>"
   │      val  : cookie_t (binary, 348 bytes)
   │      ttl  : 8h (또는 정책 합의값)
   │ 7. JWT 발급:
   │      payload: { sid, usid, site, tier, exp, iat, chan }
   │      서명: RS256 (mci-api private key --jwt-key) — §6
   ▼
[브라우저]
   │ 응답: HttpOnly + Secure 쿠키 또는 Authorization header 로 JWT 회신
```

---

## 4. 흐름 — 인증된 트랜잭션 (예: 신규 주문)

```
[브라우저]
   │ POST /v1/orders + Authorization: Bearer <JWT>
   ▼
[mci-edge-api]
   │ 1. JWT 서명 검증 (서명, 만료)
   │ 2. claim 추출 → 다음 hop 헤더 주입
   │ 3. rate limit (user 단위)
   │ gRPC mTLS
   ▼
[mci-api]
   │ 4. session_id → Redis 조회 → cookie_t 복원
   │    (Redis miss 면 401 — 클라이언트는 재로그인)
   │ 5. mymq.Options.Channel = "WEB" 등 메타 채움
   │ 6. FrameInput.Cookie = cookie_t
   │    FrameInput.Func = FCTran, Subc = SubTranMsg
   │    FrameInput.Xchg = "ORDER", Rkey = "NEW"
   │    FrameInput.Body = JSON → 엔진 페이로드
   │
   ▼
[mymqd] → [매매 엔진]
              7. cookie 검증 (coki[256] 서버측 토큰 일치 확인)
              8. 비즈니스 권한 체크 (거래 권한, 한도, 통화쌍, 시간 등)
              9. 거부 시 errn + WITH_ERROR 로 회신
              10. 통과 시 주문 등록 → reply
   ▲
   │ 11. mci-api: errn 그대로 → HTTP status 변환 → 클라이언트
```

WTG 코드에 어떤 비즈니스 룰도 없다. 거부 사유는 모두 엔진 errn 으로 전달.

---

## 5. 흐름 — 로그아웃

```
[브라우저] → POST /v1/logout
   │
   ▼
[mci-api]
   1. JWT 추출 → session_id
   2. mymq.Call(LOGOFF 트랜잭션) — cookie 첨부
   3. Redis 에서 session 즉시 삭제
   4. (옵션) 동시 접속 KILL 통보용 broadcast — 엔진의 KILL 매크로 활용
   ← 200 OK
```

---

## 6. JWT 설계

### 메커니즘 한눈에 (발급 → 검증 → 갱신)

핵심은 **RS256 비대칭** — 발급은 mci-api 의 private key 한 곳, 검증은 edge 의
public key. edge(DMZ)는 검증만 되고 **위조는 불가**. 구현: `pkg/auth/jwt.go`
(`Issuer.Sign` / `Verifier.Verify`, alg=RS256 **고정**).

```
                       ┌───────────────── mci-api (Internal, private key --jwt-key) ────────────────┐
  ①로그인               │  broker LOGON → 엔진 인증 + cookie_t                                        │
 client ─POST /v1/login─▶  cookie_t → SessionStore[sid]                                              │
 {usid,passwd}         │  Site/Tier 서버 결정(UserProfileResolver, 클라입력 무시)                     │
                       │  Claims{sid,usid,site,tier,exp} ── private key 로 RS256 Sign ──▶ access_token │
                       │  refresh_token 발급 (RefreshStore, 8h)                                      │
        ◀──────────────┘  { access_token, refresh_token, expires_at }                                
                                                                                                      
  ②매 요청 (ws 는 ?access_token= 쿼리 → 헤더 변환)                                                     
 client ─token 첨부─▶ mci-edge-* (DMZ, public key --jwt-pub)                                          
                       Verify: 서명(public key) + alg=RS256 + exp 확인 → Principal{usid,site,tier}     
                       → DB 불요(stateless). site/tier 로 Profile 매칭 → 마진 시세 fan-out             
                       (tier 가 서명 안에 있어 클라가 등급 위조 불가)                                    
                                                                                                      
  ③access 만료 → 재로그인 없이 갱신                                                                    
 client ─POST /v1/refresh {refresh_token}─▶ mci-api → 새 access_token                                 
```

- **usid**(누구) = 엔진 LOGON 이 정한 신원 → ws 등록·push 타겟 키.
- **sid**(세션) → SessionStore → **cookie_t** → 매매 요청 시 broker 에 첨부(권한 판정은 엔진). §1 위임 모델.

### Payload (claim) — `pkg/auth.Claims`

```json
{
  "sid": "01HJK...",      // session id (SessionStore key) → cookie_t 복원용
  "usid": "trader01",     // 사용자 ID (누구인가 — ws 등록/push 타겟)
  "chan": "WEB",          // 채널 코드 (mqhdr.chan[4])
  "site": "BRANCH",       // 거래주체 ─┐ Profile 결정 → 마진 등급
  "tier": "VIP",          // 고객 등급 ─┘ (서명돼 있어 위조 불가)
  "iat": 1735689600,
  "exp": 1735690500,      // access: 짧게 (분 단위)
  "jti": "01HJK..."       // 단일 사용 검증용
}
```

`coki[256]`(cookie_t) 자체는 JWT 에 넣지 않는다 (크기 + 보안). SessionStore(Redis)에만 저장.

### 서명 — RS256 고정 (코드 강제)

| 알고리즘 | |
|---------|-------|
| **RS256** (비대칭) | **채택·고정**. edge 는 public key 만 → DMZ 노출돼도 위조 불가, 키 회전(`kid`) 용이 |
| ~~HS256~~ (대칭) | 미채택. 대칭키는 검증자 모두가 발급 능력을 갖게 돼 DMZ 부적합 |

- `Verifier.Verify` 는 header `alg` 가 RS256 이 아니면 `ErrJWTUnsupportedAlg` 로 거부 (alg confusion 방어).
- 키 배포: 발급자 mci-api `--jwt-key`(private PEM), 검증자 edge `--jwt-pub`(public PEM). 회전은 header `kid` + `KeyMap`.

### 만료/갱신

| 토큰            | 수명  | 사용처                      |
| ------------- | --- | ------------------------ |
| Access JWT    | 15분 | 모든 API 요청                |
| Refresh token | 8시간 | access 재발급 (HttpOnly 쿠키) |

8시간 후 강제 재로그인. 매매 시간에 맞춰 정책 조정 가능 (운영 합의 필요).

### 회수 (Revocation)

- 정상 로그아웃: Redis 세션 즉시 삭제 → `sid` 가 무효화되어 access JWT 도
  실질적으로 사용 불가
- 강제 로그아웃 (관리자/이상거래): 동일하게 Redis 세션 삭제 + 엔진 KILL 통보

### 6.5 고객 등급(tier) 출처 — Site/Tier 는 어디서 오나

JWT 의 `site`/`tier` 는 로그인 때 **`UserProfileResolver.Resolve(usid)`** 가 결정해
박는다 (클라 입력 무시 — 위조 방지). resolver backend:

| backend | 소스 | 용도 |
|---|---|---|
| `StaticResolver` | JSON 파일 (in-memory) | dev / 단일 인스턴스 |
| `EtcdResolver` | etcd `wtg/auth/user-profiles/{usid}` (watch) | **운영 표준** — 다중 인스턴스 hot reload |

**고객 DB 의 등급을 자동 반영** — `fx-sync` 가 고객 마스터를 etcd 로 미러 (login 코드 무변경):

```
[고객 DB] ──fx-sync --table=user_profile──▶ etcd wtg/auth/user-profiles/{usid}
  grade    (GradeMapper: 등급코드→Tier)      = {site, tier} JSON
                                                    │ watch (즉시 hot reload)
                                                    ▼
                                   mci-api login 의 EtcdResolver → JWT.site/tier
```

- **매핑 seam**: `internal/fxsync.GradeMapper` — 고객 DB 원시 등급/조직 코드를
  `session.Tier`(VIP/GOLD/STD) / `session.Site`(BRANCH/HQ) 로 변환. 미등록 코드는
  fallback(STD/BRANCH) — 로그인 자체는 막지 않음. 운영 등급 체계 확정 시 매핑표만 채움.
- **writer 결정 (확정)**: **fx-sync 배치 pull**. 고객 DB 를 주기적으로 SELECT →
  etcd 미러. (고객/login 서비스가 그 키에 직접 push 하는 event 방식도 가능하지만
  본 프로젝트는 배치로 확정 — 운영 단순, 고객 DB 무수정.)
- **운영**: cron 으로 `fx-sync --table=user_profile` (또는 `--table=all`) 주기 실행.
- **freshness (배치의 함의)**: 등급 변경은 *다음 sync + 해당 고객 재로그인* 시 반영.
  즉시 반영이 필요한 케이스(긴급 강등 등)는 수동 `fx-sync` 1회 + 세션 강제 만료로 커버.
  admin UI 수동 등록은 미등록 사용자 예외용.
- **현황**: File backend(dev, `etc/db-mirror/user_profile.json`) + Syncer + GradeMapper
  구현·검증 완료 (단위 + `sync→EtcdResolver→tier` 통합 e2e). **남은 것: Oracle backend
  (실 SELECT) — 고객 마스터 테이블/컬럼 + 등급 체계 확정 후 `GradeMapper` 표만 채움.**

---

## 7. 세션 저장소 (Redis)

### 키 구조

```
wtg:sess:<session_id>      → cookie_t (binary, ~352 bytes) + 메타
wtg:user:<usid>:sessions   → set of session_id (동시 접속 추적)
wtg:totp:<usid>            → TOTP 비밀키 (KMS 로 보호 권장)
wtg:ratelimit:ip:<ip>      → 토큰 버킷
wtg:ratelimit:user:<usid>  → 토큰 버킷
```

### TTL

- `wtg:sess:*` — refresh 만료와 동일 (8h), 활동 시 슬라이딩
- `wtg:user:*:sessions` — sess TTL + 1h (정리용)

### HA

Redis Cluster 또는 Sentinel. 단일 instance 는 SPOF 라 부적격.

---

## 8. MFA (TOTP)

- 표준 RFC 6238 (Google Authenticator 호환)
- 등록 시 QR 코드 → 사용자 디바이스에 등록
- 비밀키는 KMS 로 암호화 보관, 또는 HSM
- 로그인 시 6자리 코드 검증
- 직원(ChannelAdmin): 의무화
- 일반 트레이더(ChannelWeb): 정책 합의 필요 (의무 권장)

---

## 9. 추가 web-layer 보안

### Rate limit

- L1 (DMZ): IP 단위 토큰 버킷 (Redis-backed)
  - 일반 endpoint: 100 req/min
  - 로그인: 5 req/min (brute-force 차단)
- L2 (Internal): user 단위 토큰 버킷
  - 주문: 사용자별 50 req/min (운영 합의 후 조정)

### CORS / CSRF

- CORS: 화이트리스트 기반 origin (운영 도메인만)
- CSRF: SameSite=Strict 쿠키 + 더블 서밋 토큰

### 보안 헤더 (mci-edge-api 미들웨어)

```
Strict-Transport-Security: max-age=31536000; includeSubDomains; preload
Content-Security-Policy: default-src 'self'; ...
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: strict-origin-when-cross-origin
```

### 봇 탐지

- 비정상 패턴 (UA 위변조, 비정상 호출 빈도) 자동 차단
- 외부 솔루션 (Cloudflare Turnstile / hCaptcha) 통합 가능

---

## 10. Audit Log

WTG 가 직접 기록하는 audit 이벤트:

| 이벤트 | 필드 |
|-------|-----|
| LOGIN_SUCCESS | usid, ip, ua, chan, mfa_used, ts |
| LOGIN_FAILURE | usid, ip, ua, reason (bad_password / mfa_fail / locked), ts |
| LOGOUT | usid, sid, reason (user / forced), ts |
| SESSION_EXPIRED | usid, sid, ts |
| RATE_LIMIT | ip 또는 usid, endpoint, ts |
| MFA_REGISTER / RESET | usid, by_admin, ts |
| ADMIN_ACTION | usid (admin), target_usid, action, ts |

비즈니스 거래 audit 은 매매 엔진 책임 (이미 구현됨).
WTG audit log 는 별도 immutable 저장소에 7년 보관 (금융 규제).

---

## 11. 디렉토리 / 코드 구조 (계획)

```
pkg/auth/
├── jwt.go              JWT 발급/검증 (RS256)
├── totp.go             RFC 6238 TOTP
├── session.go          Redis 세션 저장/조회
├── ratelimit.go        토큰 버킷
└── audit.go            구조화 audit log emitter

internal/api/handlers/
├── login.go            POST /v1/login (TOTP + LOGON 트랜잭션)
├── refresh.go          POST /v1/refresh (refresh → access 재발급)
└── logout.go           POST /v1/logout (LOGOFF + Redis 삭제)

internal/edge/
└── middleware/
    ├── jwt_validate.go  JWT 검증 미들웨어
    ├── ratelimit.go     IP rate limit
    ├── security_hdr.go  HSTS/CSP 등 헤더
    └── audit.go         웹 액세스 로그
```

---

## 12. 합의 필요 항목 (P0~P2)

| # | 항목 | 디폴트 / 권장 | 우선순위 |
|---|-----|-------------|---------|
| 1 | JWT 알고리즘 | RS256 | P0 (Phase 2 시작 전) |
| 2 | Access / Refresh 만료 | 15min / 8h | P0 |
| 3 | MFA 의무화 범위 | 직원 의무, 일반 권장 | P0 |
| 4 | 동시 로그인 정책 | 거부 / 강제 종료 / 허용 | P0 |
| 5 | Redis 토폴로지 | Cluster (3 master 권장) | P1 |
| 6 | TOTP 비밀키 보관 | KMS 또는 HSM | P1 |
| 7 | 보안 헤더 정책 | 본 문서 디폴트 | P1 |
| 8 | Rate limit 임계값 | 본 문서 디폴트 | P2 |
| 9 | 비밀번호 정책 | 매매 엔진 정책 따름 | P2 |
| 10 | Audit 보관 기간 | 7년 (금융 규제) | P0 |
| 11 | 인증 실패 잠금 정책 | 매매 엔진 측 정책 따름 | P2 |
| 12 | 외부 SSO 연동 (직원) | OIDC / SAML | P2 |

---

## 13. 변경 시 영향

이 명세에서 권한 위임 원칙(§1) 이 깨지면 다음이 무너진다:

- 권한 룰이 두 곳에 분산 → 동기화 부담, 일관성 무너짐
- WTG 가 엔진 내부 룰을 알아야 함 → 결합도 증가
- 엔진 룰이 바뀔 때마다 WTG 도 같이 배포 필요

**원칙 위반 발의는 승인 없이 진행 금지**. 새 거래 종류가 추가되어도 WTG 코드
변경 없이 엔진 단독 변경으로 처리 가능해야 한다.

---

## 14. 다음 단계

1. 본 문서를 운영팀/보안팀에 공유 → P0 항목 합의
2. `pkg/auth/jwt.go` `pkg/auth/session.go` 골격 구현 (Phase 1 잔여)
3. Phase 2 (mci-api) 시작 시 login/logout 핸들러부터 통합
4. Penetration test 시나리오 사전 정의 (Phase 8 대비)
