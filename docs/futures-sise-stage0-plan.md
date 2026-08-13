# 선물시세 → web — Stage 0 구현 계획 (트랙 1, JSON-first)

> 목표: **선물 체결(KA) 1종목**이 `C 피드 → WTG(KA→JSON codec) → mci-push → mci-edge-push
> → web ws` 로 **JSON 도달**함을 e2e 증명. 설계 근거: `docs/futures-sise-design.md`.
> 트랙 1(기존 C 수신/파싱 유지, WTG 는 codec+배포) + wire=JSON + codec 위치=WTG(Go).

## 0. Stage 0 범위 (in / out)

**In (이번에 함):**
- 선물 **체결(KA, `KF_CHEG_RTS_T`)** 1 TR, 1 종목
- WTG Go **KA→JSON codec** (`fut.trade` envelope)
- mci-push **broadcast** fan-out (LogonID 빈값 = 전 web conn) — 심볼필터는 web 측
- web ws 로 JSON 수신 확인

**Out (Stage 1+ 로 미룸):**
- 호가(KB)/채권(BA/BB) codec — Stage 1
- **심볼 구독 fan-out** (지금은 broadcast, web 이 client-side 필터) — Stage 1
- snapshot REST (초기 현재가) — Stage 1
- 야간·채권 도메인 — Stage 2
- Go 전면 이관(futures-forwarder) — Stage 2

## 1. 아키텍처 (Stage 0) — **시세 트랙 + 종목 구독** (2025 결정: 구독형 유력)

push 트랙(user/broadcast) 대신 **시세 트랙(종목 구독 fan-out)** 채택. WTG 의
`mci-edge-price` 가 이미 종목 구독(`Subscriber.MatchesPair`, control
`{"type":"subscribe","pairs":[...]}`, `Registry.BroadcastForPair`)을 가지며 필터가
심볼 문자열 기반이라 선물 종목코드에 그대로 동작 → **구독 인프라 재사용**.

```
[기존 C 피드 kbfut_sise]  KRX mcast→파싱→KA 전문 (무변경)
      │ push(pushdata_t: symb=종목, msgb=KA)  (Task 5: → WTG broker)
      ▼
[mci-futures (internal)]  Unsolicited 수신 → ★KA→JSON decode (pkg/futures)★
      │ gRPC SubscribeFutures stream (fut.trade JSON + code)   ※Stage0 는 in-proc/합성
      ▼
[mci-edge-futures (DMZ)]  종목구독 ws fan-out (edge-price Subscriber/Registry 패턴)
      │ Hub.BroadcastBySymbol(code, json) — 구독한 conn 에게만
      ▼
[web]  /v1/subscribe + {"type":"subscribe","symbols":["101V6000"]}
       → 구독 종목의 {"kind":"fut.trade",...} 만 수신
```

**구독 안전성**: envelope 의 `code` 로 web 이 종목 식별 → broadcast→구독 전환이 wire
무변경(additive). Stage 0 부터 종목 필터를 켜서 재작업 원천 차단.

## 2. De-risk 전략 — 0a(WTG 자체) / 0b(C 통합) 분리

C 피드·broker 배선은 환경 의존이라, **WTG 부분을 합성 KA 로 먼저 완결**하고 실 C 통합은 뒤로.

- **0a (WTG only, 의존 0)**: codec + dispatcher 훅 + push→ws 경로를 **합성 KA 주입**으로 검증.
- **0b (C 통합)**: 실 C 피드 push → WTG mymqd 배선 후 실 KA 흐름 확인.

## 3. 태스크 분해

### Task 1 — KA→JSON codec (`pkg/futures/kcheg.go`, 신규 pkg)
- `KF_CHEG_RTS_T` 고정폭(fpush.h 오프셋)을 파싱하는 `DecodeKChe(b []byte) (*FutTrade, error)`.
  - 오프셋: type@0[2], code@2[12], bprc@14[9], (ocol@23[1]) oprc@24[9] … csgn 끝.
  - 숫자 필드는 `strings.TrimSpace` + `strconv.ParseFloat/Atoi`. 공백=0/미제공.
- `FutTrade` → JSON envelope `{"kind":"fut.trade","code","time","last","sign","diff",
  "rate","open","high","low","prevClose","settle","cvol","tvol","tamt","nearPrc",
  "farPrc","upLimit","dnLimit","bs"}` (design §5 매핑).
- **단위테스트** `kcheg_test.go`: 실 캡처 or 합성 KA bytes → 기대 JSON 필드 assert.
  (fpush.h 총 길이 = `SZ_KF_CHEG_RTS_T` 로 검증 — 오프셋 정합 회귀)

### Task 2 — mci-push 디코드 훅 (`internal/push/futures_hook.go`)
- Dispatcher 가 `*mymq.Unsolicited` 처리 시, pushdata 봉투를 `DecodePushData` 로 열어
  `type`(or `mkid==30` + `mask`=시세) 판별.
- **선물 시세면** `pkg/futures.DecodeKChe(msgb)` → JSON → 그 JSON 을 broadcast body 로.
  (선물 아니면 기존 경로 그대로 — user push/notice 무변경)
- 삽입점: Dispatcher 의 Unsolicited→fan-out 사이 transform. 최소 침습 = 기존
  `OnUnsolicited`/dispatch 앞단에 필터.
- **flag** `--futures-decode`(기본 on) 로 토글 가능하게.

### Task 3 — 합성 KA producer (`cmd/fut-tester/main.go`, 신규 검증 도구)
- pkg/mymq 로 pushdata_t(mkid=30, symb, mask=시세, msgb=합성 KA) 를 broker 에 publish.
  (또는 dispatcher 에 직접 Unsolicited 주입하는 in-process 테스트)
- 결정적 KA 값(예: 101V6000, last=265.75) → web 에서 값까지 assert 가능.
- `mock-lp`/`algo-tester` 계열 검증도구 컨벤션 따름.

### Task 4 — e2e ws 수신 검증 (`scripts/fut-stage0-verify.sh`)
- 최소 스택: mci-push + mci-edge-push (broker 필요 시 docker mymqd, or in-process).
- Task 3 로 합성 KA 주입 → mci-edge-push ws (`/v1/subscribe` 계열) 연결 → 수신 JSON
  이 `{"kind":"fut.trade","code":"101V6000","last":265.75}` 인지 assert.
- websocat + jq, 또는 Go ws 클라(`ws-load-gen` 계열).

### Task 5 — 실 C 피드 통합 (0b)
- 기존 `kbfut_sise` 의 `myrq_push()` 대상 broker 를 **WTG mymqd** 로 (같은 MyMQ 면 config,
  다르면 exchange/queue 배선 or 브리지). C 코드 변경 최소(대상 endpoint).
- mci-push 를 그 exchange 의 QF_UNSOL_REP 로 등록(pushdata 수신).
- 실 KA 1종목 흐름 → web ws JSON 확인. (0a 통과 후)

## 4. 검증 게이트

| 게이트 | 방법 | 통과 기준 |
|---|---|---|
| G1 codec | `go test ./pkg/futures/` | 합성 KA → JSON 필드 정확, 오프셋 정합 |
| G2 훅 (in-proc) | dispatcher 에 Unsolicited 주입 단위테스트 | 선물=JSON broadcast, 비선물=무변경 |
| G3 e2e 합성 (0a) | `fut-stage0-verify.sh` | web ws 가 결정적 KA 값 JSON 수신 |
| G4 e2e 실피드 (0b) | 실 C 피드 1종목 | 실 체결 → web ws JSON |

## 5. 산출물

- `pkg/futures/` — kcheg.go(codec) + 테스트 (재사용: Stage1 KB/채권 확장 기반)
- `internal/push/futures_hook.go` — 디코드 훅 + flag
- `cmd/fut-tester/` — 합성 KA 주입 검증도구
- `scripts/fut-stage0-verify.sh` — e2e 자동 검증
- (0b) 기존 C 피드 push 대상 배선 메모

## 6. 리스크 / 결정

1. **broker 배선(0b)**: C 피드 push 가 WTG mymqd 에 닿는지 = 환경 확인 필요. 0a 는 무의존이라
   먼저 완결 → 0b 리스크 격리.
2. **fan-out 모델**: Stage 0 는 broadcast(전 conn). 종목 많으면 web 부하 ↑ → Stage 1 심볼구독
   필수. Stage 0 는 1종목이라 무해.
3. **KA 오프셋 정합**: fpush.h 필드 크기 기준 하드코딩 — `SZ_KF_CHEG_RTS_T` 총길이 assert 로
   회귀 방어 (W1422 svcio 정합 검증과 동일 접근).
4. **인코딩**: KA 는 ASCII 수치라 CP949 무관. 종목명 등 한글 필드 있으면 경계변환(design 별개).

## 7. 예상 규모 (CC 기준)

- Task 1 codec+test: ~반나절 (오프셋 매핑 + 테스트)
- Task 2 훅: ~반나절 (dispatcher 삽입점 + flag)
- Task 3/4 도구+스크립트: ~반나절
- Task 5 (0b): 환경 배선 의존 (broker 확인 후)
- **0a(G1~G3) = 1~1.5일**, 0b 는 배선 확인 후 별도.

## 8. 착수 순서

1. Task 1 (codec) → G1
2. Task 3 (합성 producer, in-proc 주입) + Task 2 (훅) → G2
3. Task 4 (e2e 스택) → G3  ← **여기까지 0a 완결 = Stage 0 핵심 증명**
4. Task 5 (실 C 피드) → G4  ← 0b, 환경 준비되면
