# push 테스트 가이드 — unsolicited fan-out 로컬 검증

> `mci-push` 의 unsolicited push(체결통보·잔고변경 등)를 broker 없이 로컬에서
> 검증한다. producer 가 HTTP 로 push 를 던지면 → user 매칭 → 접속한 client ws 로 fan-out.
> 배경/설계는 CLAUDE.md §2(Push 두 트랙) + `docs/push-secret-rotation.md`.

## 0. 두 트랙 & 경로

```
[Track A] 매매 엔진 ─publish─▶ broker ─▶ mci-push (representative receiver)  ← broker 필요
[Track B] 운영 svc  ─POST /v1/internal/push (X-Push-Secret)─▶ mci-push        ← broker 불요 (본 가이드)
                                                                 │ user 매칭 fan-out
                                                                 ▼
                                              client ws /v1/subscribe ─▶ web/HTS/딜러
```

- **로컬 검증은 Track B** (HTTP push). `mci-push --no-broker` 로 broker 없이 부팅.
- Track A(엔진 발 legacy)는 broker(mymqd)가 있어야 재현.
- 외부(DMZ) client 는 `mci-edge-push`(:8084)가 감싸지만, 로컬은 mci-push(:8081) 직접이 간단.

## 1. 사전 준비

```bash
cd ~/mywork/wtg
make build          # build/bin/mci-push
make cside          # (선택) cside/wtgpush/sample — C SDK 경로 테스트용
```

## 2. 테스트 (터미널 3개)

### 터미널 1 — mci-push standalone (broker 없이)
```bash
./build/bin/mci-push --no-broker --dev --listen :8081 --push-secret devsecret
```
- `--no-broker` HTTP push 전용 / `--dev` ws 인증을 `X-WTG-User` 헤더로 /
  `--push-secret` producer 인증(빈값이면 인증 off)

### 터미널 2 — client ws (수신자, usid=dealer01)
```bash
websocat ws://127.0.0.1:8081/v1/subscribe -H 'X-WTG-User: dealer01'
```
> **websocat 주의**: `-H` 가 가변 인자라 **URL 을 앞에** 둬야 한다. `-H` 를 앞에 두면
> 뒤 URL 까지 헤더로 먹어 `No URL specified` 에러. (websocat 1.14 기준)

### 터미널 3 — push 발사 (producer)

**(a) user 지정 — curl**
```bash
curl -sS -XPOST localhost:8081/v1/internal/push \
  -H 'X-Push-Secret: devsecret' -H 'Content-Type: application/json' \
  -d '{"user":"dealer01","data":{"orderId":123,"status":"FILLED"}}'
# → {"injected":true,"func":13,"subc":54,"user":"dealer01","body_size":..}
```
→ 터미널 2(dealer01)에 `{"orderId":123,"status":"FILLED"}` 수신.

**(b) 전체 broadcast — `user` 생략**
```bash
curl -sS -XPOST localhost:8081/v1/internal/push \
  -H 'X-Push-Secret: devsecret' -d '{"data":{"notice":"장 마감 5분 전"}}'
# → func:4(FCCast)/subc:50(SubBroadcast). 접속한 모든 client 수신
```

**(c) 운영 C SDK 경로 — cside/wtgpush**
```bash
./cside/wtgpush/sample 127.0.0.1 8081 devsecret dealer01 '{"orderId":123,"status":"FILLED"}'
# 사용법: sample <host> <port> <secret> <user("" =broadcast)> '<json-data>'
```
→ 운영 C svc 가 `wtg_push_send()` 한 줄로 던지는 것과 동일 wire. `make test-cside` 가 호환성 검증.

## 3. push 요청 스키마

`POST /v1/internal/push` (헤더 `X-Push-Secret`):

| 필드 | 기본 | 의미 |
|---|---|---|
| `user` | (빈값) | 대상 usid(LogonID). 빈값이면 전체 broadcast |
| `func` | user 있으면 13(FCPush) / 없으면 4(FCCast) | broadcast func code |
| `subc` | user 있으면 54(SubPush) / 없으면 50(SubBroadcast) | sub func code |
| `data` | — | payload(JSON). client 가 받는 본문 |

응답: `{"injected":bool,"func":..,"subc":..,"user":..,"body_size":..}`.

## 4. 확인 / 관측

```bash
curl -s localhost:8081/v1/push-stats | jq .   # 주입/전달/드롭 카운터
curl -s localhost:8081/v1/ping                # liveness
```

## 5. dev vs 운영 — "로그인한 유저를 어떻게 아는가"

**중요**: dev 의 `X-WTG-User` 는 client 가 usid 를 **그냥 주장**하는 것 — 로그인 검증이
없어 spoof 가능하다(`--dev` 우회). 실제 운영에선 신원이 **로그인 시점에 엔진이 못박고**
JWT 로 흐른다:

```
1) 딜러 로그인 (POST /v1/login)
   → 매매 엔진 인증 + cookie_t(usid) 발급
   → WTG 가 usid claim 박힌 JWT(access_token) 발급   ← RS256 서명, 위조 불가
2) client ws 접속:  ?access_token=<JWT>
   → 미들웨어가 JWT 서명 검증 + usid 추출 → ws 연결이 그 usid 로 등록  ← X-WTG-User 아님
3) push 발신: 엔진/운영 svc 가 주문·계좌 문맥에서 소유자 usid 를 알고 push {"user":usid}
   → 2)의 등록 usid 와 매칭 → 그 딜러에게만 전달
```

즉 **usid = cookie_t.usid = 엔진 LogonID** — 로그인 때 정해진 같은 값이 JWT(ws 등록)와
push(엔진 발신) 양쪽에 쓰여 매칭된다. dev 의 `X-WTG-User` 는 "그 JWT 가 담았을 usid" 대역.
로컬 fan-out 배선 검증만이면 X-WTG-User 로 충분하고, 운영은 그 자리에 JWT usid 가 들어갈 뿐.

**실제 JWT 흐름 테스트** (X-WTG-User 대신):
```bash
TOKEN=$(curl -sS -XPOST localhost:8080/v1/login \
  -d '{"user":"dealer01","password":"..."}' | jq -r .access_token)
websocat "ws://127.0.0.1:8081/v1/subscribe?access_token=$TOKEN"
curl -XPOST localhost:8081/v1/internal/push -H 'X-Push-Secret: devsecret' \
  -d '{"user":"dealer01","data":{"status":"FILLED"}}'
```
> `/v1/login` 은 매매 엔진 인증(broker 연동)이 필요. broker 없이 배선만이면 X-WTG-User 대역.

## 6. 트러블슈팅

| 증상 | 원인 | 해결 |
|---|---|---|
| websocat `No URL specified` | `-H` 가변 인자가 URL 을 먹음 | URL 을 앞에: `websocat ws://... -H '...'` |
| push `401` | `X-Push-Secret` 불일치 | 헤더 값 확인, 또는 `--push-secret` 생략(인증 off) |
| ws `401 unauthorized` | dev 는 `X-WTG-User` 헤더 필요 | `websocat ws://... -H 'X-WTG-User: <usid>'` |
| `injected:true` 인데 무반응 | push `user` ≠ ws `X-WTG-User`, 또는 그 user ws 미접속 | usid 정확히 일치 / broadcast 는 user 생략 |
| broadcast 는 오는데 user 지정 안 옴 | usid 대소문자·공백 불일치 | 양쪽 문자열 완전 일치 확인 |

## 관련
- `cside/wtgpush/` — HTTP push C SDK (`sample.c` + `libwtgpush.a`), `make test-cside`
- `internal/push/http_push.go` — `/v1/internal/push` 핸들러 + 요청 스키마
- `internal/push/handlers.go` — ws `/v1/subscribe` + Registry 등록
- `docs/push-secret-rotation.md` — secret 회전 절차
- `docs/push-monitoring.md` — source/CN 가시화 dashboard
- `docs/customer-connections.md` — 고객별 접속·fan-out 3 트랙 전체 가이드
