# 체결통보 push 계약 (exec_report) — 서버팀 전달용

**결정 (2026-08-31, 방향 C):** OMS(producer)가 **구조화 JSON 체결 이벤트**를 push하고,
WTG는 **무변형 passthrough**, 클라는 그 JSON을 파싱한다. **WTG는 변경 없음.**

---

## ✅ 서버팀이 바꿀 것 — 딱 2가지

지금 OMS는 `POST /v1/internal/push`에 **내부 큐 전문(RTAEXEIQUE)을 raw hex 그대로**
넣고, `user`를 **비워서** 보냅니다. 아래 2개만 바꾸면 끝입니다.

1. **payload를 raw hex → `exec_report` JSON으로** (아래 §2 스키마).
2. **`user` 필드에 로그인ID(예 `yuanta`)를 채운다** (지금은 빈값 → 전체 broadcast).
   채우면 그 유저에게만 간다(targeted). 체결은 주인이 명확하니 **반드시 채울 것.**

> WTG는 `data`를 손대지 않고 클라까지 그대로 전달한다. 즉 **OMS가 넣는 JSON = 클라가
> 받는 JSON.** WTG는 RTAEXEIQUE 포맷을 몰라도 된다(그게 이 방향의 목적).

---

## 1. 지금 vs 목표 (같은 체결, payload만 다름)

**지금 (raw — 클라가 못 알아봄):**
```json
POST /v1/internal/push
X-Push-Secret: <FIX_PUSH_SECRET>
{
  "user": "",
  "data": {"kind":"oms.rta","queue":"RTAEXEIQUE","len":2105,
           "hex":"4f317975616e7461...(2105바이트 고정폭 전문)"}
}
```

**목표 (구조화 — 클라가 바로 표시):**
```json
POST /v1/internal/push
X-Push-Secret: <FIX_PUSH_SECRET>
{
  "user": "yuanta",
  "data": {
    "type": "exec_report", "ver": 1,
    "usid": "yuanta", "acct": "9999", "ord_no": "21011",
    "sym": "USDKRW", "side": "B", "exec_type": "FILL",
    "qty": 1000000, "px": 1381.52, "amt": 1381520000,
    "lp": "NHB", "trdr": "1001",
    "trade_date": "2026-08-31", "value_date": "2026-09-02",
    "ts": 1788137806401
  }
}
```

---

## 2. exec_report v1 스키마

| 필드 | 타입 | 의미 | 이번 체결 값 |
|------|------|------|-------------|
| `type` | string | 고정 `"exec_report"` (클라 라우팅 판별자) | exec_report |
| `ver` | int | 스키마 버전 | 1 |
| `usid` | string | 로그인ID (= push `user`와 동일) | yuanta |
| `acct` | string | 계좌번호 | 9999 |
| `ord_no` | string | 주문번호 | 21011 |
| `sym` | string | 통화쌍 (SymbolMap 외부명, **슬래시 없음**) | USDKRW |
| `side` | string | `B`=매수 / `S`=매도 | B |
| `exec_type` | string | `FILL`(전량) / `PARTIAL`(부분) / `ACK`/`REJECT`/`CANCEL` | FILL |
| `qty` | number | 체결수량 | 1000000 |
| `px` | number | 체결가 | 1381.52 |
| `amt` | number | 체결금액 (qty×px) | 1381520000 |
| `lp` | string | 유동성공급자(LP) | NHB |
| `trdr` | string | 트레이더번호 | 1001 |
| `trade_date` | string | 체결일 `YYYY-MM-DD` | 2026-08-31 |
| `value_date` | string | 결제일 `YYYY-MM-DD` | 2026-09-02 |
| `ts` | int | 체결시각 unix ms | 1788137806401 |

> 필드명/타입은 **계약(고정)**, 값 매핑은 **OMS 소유**. 필요 필드 추가는 `ver` 올리고 추가.

### 원천 RTA 전문 → 스키마 매핑 근거
이번 실 체결 전문(`O1yuanta ...`)을 공백 분절한 토큰:
```
[00] O1yuanta   → usid(yuanta)        [01] 9999        → acct
[03] 21011      → ord_no              [06] 1|20260831  → trade_date
[08] 03 USDKRW 20260902 0112 20260831 USDKRW → sym / value_date / trade_date
[09] 1000000.0  → qty                 [15] 1381.520000 → px
[17] 1381520000 → amt                 [24] E           → exec_type(체결)
     ... NHB → lp,  1001 → trdr
```
정확한 필드 오프셋/코드값(side, exec_type 등)은 **OMS의 RTAEXEIQUE 전문 정의가 authoritative** —
OMS가 그 정의로 위 스키마에 매핑하면 된다.

---

## 3. WTG 경로 (참고 — 변경 없음)

```
OMS ─POST /v1/internal/push {user, data}─▶ mci-push(:8081)
     └ user 채워짐 → LogonID=user 로 fan-out
     └ data 무변형
mci-push ─gRPC PushService(전량)─▶ edge-push(:8084)
     └ FanoutToUser(logon_id) : logon_id == 클라 등록ID(=?x_wtg_user) 인 연결에만
클라 ws 수신: {"func":13,"subc":54,"logon_id":"yuanta","data":{...exec_report...}}
```

- 인증: `X-Push-Secret: <FIX_PUSH_SECRET>` 헤더 필수.
- **매칭 키**: push의 `user`(→logon_id) == 클라의 `?x_wtg_user=<값>` 등록ID. 둘 다 `yuanta`면 매칭.
- `user`를 비우면 broadcast(전체 유저) — **체결엔 쓰지 말 것.**

---

## 4. 각 측 책임

| 측 | 할 일 |
|----|------|
| **OMS(서버팀)** | RTA 전문 → `exec_report` JSON 매핑 + `user`=로그인ID로 push (§1 목표) |
| **WTG** | 무변경 (passthrough + user→logon_id fan-out 이미 동작) |
| **클라** | `data.type=="exec_report"` 수신 시 필드로 체결 화면 갱신 (raw hex 파싱 불필요) |

---

## 5. 왜 이 방향 (요약)

- **채널 확장 0 작업** — web/HTS/모바일 등 모든 채널이 계약 하나 공유.
- **WTG passthrough 순수성** — 내부 포맷 무지, 비즈니스 로직 없음.
- **포맷 소유권 = OMS(체결 주인)** — 스키마/버전 일원화.
- **내부 큐 포맷 비노출** — 캡슐화.

---

## 6. 검증 (반영 후)

1. OMS가 `exec_report` JSON을 `user=yuanta`로 push (§1 목표 payload).
2. 클라(`?x_wtg_user=yuanta`) ws에 `{"logon_id":"yuanta","data":{"type":"exec_report",...}}` 수신.
3. 화면에 체결 표시.
4. (선택) WTG측 도달 확인: mci-push `/metrics`의 delivered/drop 카운터는 **직결 ws 기준**이라
   edge-push 클라에겐 무의미 — 실제 도달은 클라 ws 수신으로 확인 (경로는 이미 검증됨).

### curl 스모크 (서버팀 자가 테스트)
```bash
curl -s -X POST http://127.0.0.1:8081/v1/internal/push \
  -H "X-Push-Secret: $FIX_PUSH_SECRET" -H "Content-Type: application/json" \
  -d '{"user":"yuanta","data":{"type":"exec_report","ver":1,"usid":"yuanta",
       "acct":"9999","ord_no":"E2E","sym":"USDKRW","side":"B","exec_type":"FILL",
       "qty":1000000,"px":1381.52,"amt":1381520000,"lp":"NHB","trdr":"1001",
       "trade_date":"2026-08-31","value_date":"2026-09-02","ts":1788137806401}}'
# → yuanta 로 접속한 클라 ws 에 그대로 도착해야 정상
```
