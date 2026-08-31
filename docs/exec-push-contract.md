# 체결통보 push 계약 (exec_report) — OMS ↔ WTG ↔ 클라

**결정 (2026-08-31): 방향 (C).** OMS(producer)가 **구조화 JSON 체결 이벤트**를 push하고,
WTG는 **순수 passthrough**(무변형 전달), 클라는 그 JSON을 파싱한다. WTG는 OMS 내부
큐 포맷(RTAEXEIQUE)을 모른다 — 포맷 소유권은 체결의 주인인 OMS에 있다.

배경: 초기엔 OMS가 내부 큐 전문(`kind:oms.rta`, RTAEXEIQUE 고정폭 hex)을 raw 그대로
push했다. 그러면 채널마다(web/HTS/모바일…) 파싱을 중복 구현해야 하고 WTG가 내부
포맷에 묶인다. 구조화 계약 하나로 모든 채널이 공유하고, WTG는 MCI 본연(passthrough)을
지킨다.

## 1. WTG 경로 (무변경 — 확인용)

OMS producer → `POST http://<mci-push>:8081/v1/internal/push`:

```json
{
  "user": "<로그인ID>",        // 대상. 채우면 그 유저에게만(FanoutToUser). 빈값=broadcast(전체)
  "data": { ...exec_report... } // 아래 §2 구조화 이벤트. WTG 무변형 통과.
}
```

- 인증: `X-Push-Secret: <FIX_PUSH_SECRET>` 헤더.
- `user`(=LogonID)는 클라의 `?x_wtg_user=<값>` 등록ID와 **정확히 일치**해야 매칭.
  체결은 주인이 명확하므로 **broadcast 금지, user 채워 targeted 권장** (타 유저에게 노출 방지).
- WTG(mci-push → edge-push)는 `data`를 그대로 ws 클라에 전달:
  `{"func":13,"subc":54,"logon_id":"<user>","data":{...exec_report...},"received_unix_nano":...}`

## 2. 체결 이벤트 스키마 (exec_report v1)

OMS가 RTA 전문 필드를 아래로 매핑해 `data`에 싣는다 (필드명/타입은 계약, 값은 OMS 소유):

```jsonc
{
  "type": "exec_report",     // 고정. 클라 라우팅 판별자
  "ver": 1,
  "usid": "yuanta",          // 로그인ID (push의 user 와 동일)
  "acct": "9999",            // 계좌번호
  "ord_no": "21011",         // 주문번호
  "sym": "USDKRW",           // 통화쌍 (SymbolMap 외부명, 슬래시 없음)
  "side": "B",               // B=매수 / S=매도
  "exec_type": "FILL",       // FILL(전량) / PARTIAL(부분) / (필요시 ACK/REJECT/CANCEL)
  "qty": 1000000,            // 체결수량
  "px": 1381.52,             // 체결가
  "amt": 1381520000,         // 체결금액 (qty×px)
  "lp": "NHB",               // 유동성공급자(LP)
  "trdr": "1001",            // 트레이더번호
  "trade_date": "2026-08-31",// 체결일 (YYYY-MM-DD)
  "value_date": "2026-09-02",// 결제일
  "ts": 1788137806401        // 체결시각 (unix ms)
}
```

참고 — 방금 실 체결 전문에서 확인된 원천 값 (RTAEXEIQUE 디코드):
`O1yuanta / 9999 / 주문 21011 / USDKRW / 체결일 20260831 / 결제일 20260902 /
수량 1000000 / 체결가 1381.52 / 금액 1381520000 / exec 'E' / LP NHB / trdr 1001`.
→ OMS가 이 필드들을 위 스키마로 매핑. (side·exec_type 정확값은 OMS 전문 정의 기준.)

## 3. 각 측 책임

| 측 | 할 일 |
|----|------|
| **OMS(서버팀)** | RTA 전문 → exec_report JSON 매핑. `user`=로그인ID로 targeted push(§1). |
| **WTG** | 무변경. `data` passthrough + `user`→LogonID 매칭 fan-out (이미 동작). |
| **클라** | `type=="exec_report"` 수신 시 필드로 체결 화면 갱신. raw hex 파싱 불필요. |

## 4. 왜 이 방향 (요약)

- 채널 확장 0 작업 (모든 채널이 계약 하나 공유).
- WTG passthrough 순수성 유지 (내부 포맷 무지, 비즈니스 로직 없음).
- 체결 포맷 소유권이 OMS(주인)에 — 스키마/버전 관리 일원화.
- 내부 큐 포맷(RTAEXEIQUE) 외부 비노출 (캡슐화).

## 5. 검증 (계약 반영 후)

1. OMS가 exec_report JSON을 `user=yuanta`로 push.
2. 클라(`x_wtg_user=yuanta`) ws 에 `{"logon_id":"yuanta","data":{"type":"exec_report",...}}` 수신.
3. 화면에 체결 표시. (WTG측은 `cmd` ws 프로브로 도달 확인 가능 — 이미 경로 검증됨.)
