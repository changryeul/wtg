# FIX Gateway 설계 — mci-edge-fix

외부 FIX 카운터파티 (기관 고객 / ECN / MM) 가 NH 매매 엔진에 주문을
보낼 수 있도록 WTG 의 새 DMZ edge 를 추가하는 설계. 시세 in (`quote-forwarder`,
35=W/X) 은 이미 처리 중이며 본 문서는 **주문 in / 체결 out** 의 양방향
세션 처리 범위만 다룬다.

대상 독자: 신규 개발자 / 운영자 / 통합 파트너 검토용.
상태: 설계 (구현 전). PoC scope · 결정 4가지는 §11 참조.

## 1. 한 줄 요약

`mci-edge-fix` 를 DMZ 에 신설해 FIX 4.4 session 을 종단한다. 외부 client →
mci-edge-fix → (a) NewOrderSingle 은 mci-api `/v1/tx alias` 로 변환,
(b) ExecutionReport 는 mci-push HTTP push 로 받아 FIX format 으로 송신.
세션 ↔ Principal 매핑은 etcd 의 counterparty 룰 (mci-admin CRUD).

## 2. 현 상태 — 무엇이 준비됐고 무엇이 없는가

| 영역 | 상태 | 위치 |
|---|---|---|
| `ChannelFIX` enum | ✓ 정의됨 | `pkg/session/types.go:33` |
| `pkg/mymq/conventions.go` 의 ChannelFix | "향후" 주석 | line 60 |
| FIX 시세 in (35=W / 35=X) | ✓ `quote-forwarder` 가 처리 | `cmd/quote-forwarder/main.go` |
| QuoteID (FIX tag 117) / OrdRejReason (tag 103) 호환 RPC | ✓ 매칭 엔진 측 검증 | `internal/price/quote_validation.go` |
| FIX 주문 in (35=D NewOrderSingle) | ✗ **edge gateway 없음** | — |
| FIX 체결 out (35=8 ExecutionReport) | ✗ **drop copy 채널 없음** | — |
| FIX session 관리 (Logon/Heartbeat/Resend) | ✗ | — |

→ **mci-edge-fix 신설** + 그 안의 session 관리 + in/out 매핑이 본 설계의 작업
범위.

## 3. 아키텍처 — 한 그림

```
┌────────────────────┐
│ 외부 FIX client    │   TCP :5001 (FIX 4.4, TLS 권장)
│ (기관 / ECN / MM)  │
└─────────┬──────────┘
          │ Logon → NewOrderSingle → ExecutionReport(s) → Logout
          ▼
┌────────────────────────────────────────────────────────────┐
│ mci-edge-fix (DMZ, 신규)                                   │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Session 관리 (QuickFIX/Go)                            │  │
│  │  · SenderCompID/TargetCompID + Seq + Heartbeat       │  │
│  │  · Logon Password 검증 (etcd counterparty 룰)         │  │
│  │  · ResendRequest / SequenceReset 처리                 │  │
│  └──────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Principal 주입                                        │  │
│  │  SenderCompID → etcd lookup → Profile (Chan.Site.Tier)│  │
│  │  → middleware.Principal 형식으로 /v1/tx 호출 시 첨부  │  │
│  └──────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Order in 매핑 — 35=D → /v1/tx alias 변환              │  │
│  │  symbol/side/qty/price → JSON envelope                │  │
│  └──────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Exec out 매핑 — mci-push HTTP push 수신 → 35=8 변환    │  │
│  │  drop copy: 같은 ExecutionReport 가 web ws 와 FIX     │  │
│  │  session 양쪽에 동시 fan-out                          │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────┬─────────────────────────────────┬────────────────┘
          │ POST /v1/tx                     ▲ HTTP push
          ▼                                 │
       mci-api ── broker ── 매매 엔진 ──── mci-push
       (기존, 변경 0)        (기존)         (HTTP push endpoint 재사용)
```

**원칙**: mci-edge-fix 는 **session 종단 + 메시지 변환** 만 책임. 매매
비즈니스 로직 / 인증 결정 / 라우팅 룰 등 일체 손대지 X (`docs/auth.md`
위임 원칙 일관).

## 4. FIX session 관리

### 4.1 표준 메시지

| Msg Type | 이름 | 처리 |
|---|---|---|
| `35=A` | Logon | TargetCompID 검증 + SenderCompID lookup + Password 검증 → 성공 시 Logon 응답 |
| `35=5` | Logout | session close + counterparty unregister |
| `35=0` | Heartbeat | 30초 주기 (운영 조정 가능) |
| `35=1` | TestRequest | 즉시 Heartbeat 회신 |
| `35=2` | ResendRequest | 메시지 store 에서 누락 구간 재전송 |
| `35=4` | SequenceReset | seq 강제 갱신 (GapFill / Reset 두 모드) |
| `35=3` | Reject | session-level 오류 (BeginString 불일치 등) |
| `35=j` | BusinessMessageReject | application-level 오류 (counterparty 미등록 등) |

### 4.2 Sequence 보존

FIX 의 핵심 — 각 방향 (Incoming / Outgoing) seq 가 1부터 단조 증가. 끊김 시
ResendRequest 로 누락 복원. **메시지 store 가 영속 필수**:

- QuickFIX/Go 의 `MessageStoreFactory`:
  - `MemoryStoreFactory` — 재시작 시 seq 1 부터 (위험)
  - `FileStoreFactory` — local disk
  - `MongoStoreFactory` — 분산
- 권장: **per-session FileStore** + 다중 인스턴스 시 sticky (동일 session 은 동일 instance)

### 4.3 LogonReject 사유

| 사유 | 응답 |
|---|---|
| TargetCompID 불일치 | Logout(58=TargetCompID mismatch) |
| SenderCompID 미등록 | Logout(58=Unknown sender) |
| Password 불일치 | Logon Reject (4.4 표준엔 별도 reject — 운영에선 Logout 으로 통일) |
| 동일 session 이미 연결 중 | Logout(58=Duplicate session) |
| Heartbeat 누락 (timeout) | Logout(58=Heartbeat timeout) → session close |

## 5. 인증 / Principal 매핑

### 5.1 etcd schema

```
wtg/fix/counterparties/<SenderCompID> = {
    "password_hash": "bcrypt:$2a$...",
    "profile": {
        "channel": "FIX",
        "site": "HQ",
        "tier": "VIP"
    },
    "usid": "ECN_DEUTSCHE_01",
    "allowed_pairs": ["USD/KRW", "EUR/USD"],
    "rate_limit": {"orders_per_sec": 100, "burst": 200},
    "enabled": true
}
```

mci-admin UI 의 새 페이지 `/fix-counterparties.html` (오늘 customer-pairs
패턴) 에서 운영자가 CRUD. mci-edge-fix 는 watch.

### 5.2 Logon → Principal 변환

```
Logon(35=A) 수신
  └─ 49=SenderCompID, 56=TargetCompID, 554=Password
       │
       ▼
etcd lookup: wtg/fix/counterparties/<49=>
       │
       ▼
검증: password_hash 비교 + enabled=true + TargetCompID = WTG 의 self
       │
       ▼
Principal 구성:
  Usid       = doc.usid            // "ECN_DEUTSCHE_01"
  Channel    = "FIX"
  Site/Tier  = doc.profile.site/tier
  CustomerID = SenderCompID        // ws customer-pairs 와 동일 패턴
```

### 5.3 web 의 JWT 와의 관계

FIX 는 long-lived TCP session 이라 JWT TTL (15분) 개념 부적합. Session
존속 동안 etcd watch 로 Principal 무효화 가능 (counterparty `enabled=false`
세팅 → 다음 메시지 시 session close).

## 6. 주문 흐름 (in) — NewOrderSingle 변환

### 6.1 FIX 원본

```
8=FIX.4.4|9=...|35=D|49=ECN_DEUTSCHE_01|56=WTG|34=42|52=20260629-10:15:00|
11=ORD-7f3a|55=USD/KRW|54=1|38=1000000|40=2|44=1378.55|59=0|117=Q-XXX|10=...
```

| Tag | 의미 | 매핑 |
|---|---|---|
| `49` SenderCompID | session 식별 (Principal 에서 처리) | — |
| `11` ClOrdID | client 주문 ID | envelope.client_order_id |
| `55` Symbol | 통화쌍 | envelope.symbol |
| `54` Side | 1=Buy / 2=Sell | envelope.side |
| `38` OrderQty | 수량 | envelope.qty |
| `40` OrdType | 2=Limit | envelope.ord_type |
| `44` Price | 가격 | envelope.price |
| `59` TimeInForce | 0=Day / 1=GTC / 3=IOC | envelope.tif |
| `117` QuoteID | 시세 잠금 ID | envelope.quote_id (필수 — `internal/price/quote_validation.go` 가 검증) |

### 6.2 변환된 `/v1/tx` 호출

```json
POST /v1/tx
Headers:
  X-WTG-Edge-User: ECN_DEUTSCHE_01
  X-WTG-Edge-Channel: FIX
Body:
{
  "alias": "FIX_NEW_ORDER",
  "data": {
    "client_order_id": "ORD-7f3a",
    "symbol": "USD/KRW",
    "side": "buy",
    "qty": 1000000,
    "ord_type": "limit",
    "price": 1378.55,
    "tif": "day",
    "quote_id": "Q-XXX"
  }
}
```

→ `mci-api` 가 routing alias `FIX_NEW_ORDER` 를 etcd 에서 resolve →
exchange/routing-key → broker → 매매 엔진. **mci-api / broker / 매매 엔진
은 일체 변경 없음** — generic envelope 원칙.

### 6.3 동기 응답 변환 — `/v1/tx` 응답 → FIX ExecutionReport(35=8)

```json
mci-api 응답:
{
  "ok": true,
  "data": {
    "order_id": "ENG-7f3a-001",
    "status": "ACCEPTED",
    "fill_qty": 0,
    "leaves_qty": 1000000
  }
}
```

→ FIX:
```
8=FIX.4.4|...|35=8|37=ENG-7f3a-001|11=ORD-7f3a|39=0|150=0|55=USD/KRW|
54=1|38=1000000|14=0|151=1000000|10=...
```

- `37` OrderID = 엔진 채번
- `39` OrdStatus / `150` ExecType = 0(New) / 1(Partial) / 2(Filled) / 8(Rejected)
- `14` CumQty / `151` LeavesQty

### 6.4 Reject — `/v1/tx` 응답이 거부일 때

```
mymq.Error 의 errn → 35=8 의 39=8(Rejected) + 103(OrdRejReason)

mymq errn 1029 (QUOTE_ID_EXPIRED) → tag 103=99 (Other, text=quote expired)
mymq errn 1030 (KILL_SWITCH)      → tag 103=2  (Exchange closed)
mymq errn 1031 (LIMIT_EXCEEDED)   → tag 103=3  (Order exceeds limit)
```

매핑 표는 `internal/edge/fix/rej_reason.go` 에 별도 (mds 의 OrdRejReason 패턴
참조).

## 7. 체결 흐름 (out) — ExecutionReport drop copy

### 7.1 문제

`/v1/tx` 의 동기 응답으로 New(39=0) 까지는 보냄. 그 후의 부분체결 / 완전체결
/ 취소 등 **비동기 ExecutionReport** 는 broker 의 push fan-out 경로로 매매
엔진에서 발생. 이걸 FIX session 에 흘려야 함.

### 7.2 해결 — mci-push HTTP push endpoint 재사용

mci-push 는 이미 broker 우회 HTTP push endpoint 보유 (Phase 2.x). 매매
엔진이 ExecutionReport 발생 시:

```
매매 엔진 → mci-push HTTP push (X-Push-Secret 인증)
     POST /v1/push
     body: {
       "target_user": "ECN_DEUTSCHE_01",
       "channel": "FIX",
       "payload": {
         "type": "exec_report",
         "order_id": "ENG-7f3a-001",
         "client_order_id": "ORD-7f3a",
         "exec_id": "EXEC-001",
         "exec_type": "F",         // Trade
         "ord_status": "1",        // Partial
         "fill_qty": 500000,
         "leaves_qty": 500000,
         "last_px": 1378.56,
         "transact_time": "20260629-10:15:01.234"
       }
     }
```

mci-push 가 `channel=FIX` 메시지를 **mci-edge-fix 의 신규 HTTP receive
endpoint** (또는 gRPC stream) 로 forward. mci-edge-fix 가 JSON → FIX
35=8 변환 후 해당 SenderCompID 의 session 으로 송신.

### 7.3 drop copy 모드

- **단일 session** — 해당 counterparty 만 받음 (default)
- **dual** — 같은 ExecutionReport 가 web ws 와 FIX session 양쪽 (운영자가
  counterparty 룰에 `also_to_web_ws: true` 추가)

## 8. Cancel / Replace

```
OrderCancelRequest (35=F)        → /v1/tx alias FIX_CANCEL_ORDER
OrderCancelReplace (35=G)        → /v1/tx alias FIX_REPLACE_ORDER
OrderCancelReject (35=9)         ← /v1/tx 응답 변환 (취소 거부)
```

매핑은 NewOrderSingle 과 같은 패턴. 추가 tag (41=OrigClOrdID, 37=OrderID) 만
유의.

## 9. Reject 처리 — session vs application

| 종류 | 메시지 | 발생 |
|---|---|---|
| Session-level | `35=3` Reject | BeginString 불일치 / MsgType 미지원 / Seq gap 후 GapFill 실패 |
| Application-level | `35=j` BusinessMessageReject | counterparty 등록 안 됨 / FIX_NEW_ORDER alias 미등록 / quote_id 검증 실패 |
| Order-level | `35=8` ExecutionReport (39=8) | 매매 엔진의 비즈니스 거부 (한도 초과 / 통화 미허용 등) |

운영자가 어떤 reject 가 자주 나는지 admin UI 에서 카운터 가시화 (Phase B).

## 10. 라이브러리 선택 — QuickFIX/Go

### 10.1 후보

| 옵션 | 장점 | 단점 |
|---|---|---|
| **QuickFIX/Go** (https://github.com/quickfixgo/quickfix) | 사실상 표준, 활발한 maintenance, FileStore / MongoStore 내장, FIX 4.0~5.0 SP2 | external 의존 1개, generated code (gocraft/dbr 같은 패턴) |
| 자체 구현 (mds 의 `mds_fix.c` 포팅) | 외부 의존 0 (cside 원칙) | Logon/Resend/Sequence boilerplate 가 크고 4.4 spec 100+ msg 지원 부담 |
| simplefix / 직접 파서 | 가벼움 | session 보일러플레이트 없음 — Logon/Resend 직접 구현 |

**추천: QuickFIX/Go** ← **`docs/quickfix-go-spike.md` 평가로 확정**.
- WTG 의 다른 외부 의존 (`go.etcd.io/etcd`, `redis/go-redis` 등) 과 같은 수준
- mds 가 자체 FIX 구현으로 가는 건 C 환경 / cside 원칙 (외부 의존 0) 때문 —
  Go 환경의 mci-edge-fix 는 그 제약 없음
- Phase A (Logon + NewOrderSingle + ExecutionReport) 만 보면 ~500 lines.
  자체 구현은 ~3000+
- spike 확인 결과 — 의존성 6개 / vulncheck 깨끗 / boilerplate 84 LOC /
  multi-session 자동 지원

### 10.2 의존성 추가

```bash
go get github.com/quickfixgo/quickfix
go get github.com/quickfixgo/fix44/...
```

`go.mod` 에 ~10 transitive 추가 예상. `make vulncheck` 통과 확인 필요.

## 11. 결정 사항 4가지 (PoC 진입 전)

| 결정 | 답 | 이유 |
|---|---|---|
| **라이브러리** | QuickFIX/Go | 표준, 보일러플레이트 절감 |
| **Session ↔ Principal 매핑** | etcd 룰 (`wtg/fix/counterparties/<SenderCompID>`) | 운영자가 mci-admin UI 에서 CRUD, customer-pairs 와 동일 패턴 |
| **drop copy** | mci-push HTTP push 재사용 + mci-edge-fix 가 FIX 변환 | broker 우회, 운영 일관 (Phase 2.x 방향) |
| **NewOrderSingle → 매매** | `/v1/tx` alias — **카운터파티별 (`Counterparty.OrderAlias`)** + envelope 의 `raw_fix` map (모든 tag 보존) | generic envelope + dialect cover (Phase B Layer 2/3) |

이 4 결정에 동의 안 하시는 부분이 있으면 PoC 들어가기 전에 짚어야 함.

## 12. 단계별 로드맵

| Phase | 범위 | 추정 |
|---|---|---|
| **A — 주문 in (단방향)** | mci-edge-fix 신설 + Logon + NewOrderSingle → `/v1/tx` 까지. ExecutionReport 동기 (New 39=0) 만. drop copy 없음. | ✓ **완료** (`cmd/mci-edge-fix/` + `internal/edge/fix/`, E2E 3 케이스 PASS) |
| **B-1 — etcd watch + admin CRUD** | counterparty 등록 dynamic 갱신 + admin UI/REST | ✓ **완료** (`internal/edge/fix/counterparty_policy.go` + `internal/admin/admin_fix_counterparties.go` + `ui/fix-counterparties.html`) |
| **B-1a — dialect cover (Layer 2 + 3)** | 카운터파티별 OrderAlias + envelope 의 raw_fix map (모든 tag 보존). user-defined / ECN required tag 처리. | ✓ **완료** (`Counterparty.OrderAlias` + `OrderEnvelope.RawFix` + admin UI 입력 칸) |
| **B-2 — 체결 out (drop copy)** | mci-edge-fix 의 `POST /v1/internal/exec-report` HTTP receive endpoint + 35=8 비동기 송신 + OrdRejReason 매핑. mci-push 는 손대지 않음 (매매 엔진이 channel=FIX 면 mci-edge-fix 로 직접 push — 운영 단순). | ✓ **완료** (`exec_report.go` + `exec_report_http.go` + `orderrej_reason.go` + E2E PASS) |
| B-2b — 운영 보강 | ResendRequest 처리 (이미 quickfix 자동 처리) / 다중 ExecReport 의 sequence 보장 / drop copy 의 ack mechanism | 별도 — 운영 측정 후 |
| **C — Cancel/Replace + SIGHUP reload** | 35=F/G 매핑 + envelope.Op 분기 (한 alias 가 lifecycle 처리) + SIGHUP 으로 새 CID 등록 (Acceptor 재시작). | ✓ **완료** (`cancel_replace_mapper.go` + `Server.Reload` + SIGHUP handler + E2E PASS) |
| **D-1 — FileStore + Prometheus metrics** | quickfix FileStore (재시작 시 seq 보존, `--store-dir`) + 8 metric (logon ok/reject / orders received/forwarded/rejected / exec_report sent/rejected / reload ok/fail / active_sessions gauge) + `/metrics` endpoint | ✓ **완료** (`metrics.go` + `selectStoreFactory` + `--store-dir` + `MetricsHandler`) |
| D-2 — TLS / mTLS | listen 측 + initiator 인증서 + 운영 권장 cipher suite | 3일 |
| D-3 — 다중 인스턴스 sticky LB | session sticky 정책 + LB 설정 가이드 (HAProxy / NLB) + Mongo 백엔드 옵션 (다중 인스턴스 shared store) | 1주 |

**총 ~3주 PoC + ~1주 운영 강화 = 약 1개월**.

## 13. 한계 / 비범위

| 항목 | 비범위 이유 |
|---|---|
| FIX 5.0 / FIXT | 4.4 가 NH 운영 표준. 5.0 은 후속 phase. |
| Market Data (35=W/X) 송신 | quote-forwarder 가 수신만 처리. FIX 시세 송신은 별도 (mci-edge-md). |
| Trade Capture Report (35=AE) | 매매 엔진 후행 보고 — 본 PoC 범위 외. |
| Allocation (35=J/P) | 기관 trade 후 allocation. NH 이 별도 시스템에서 처리. |
| 다중 LP 통합 | mci-edge-fix 는 카운터파티 인입만. 외부 LP 로의 outbound FIX 는 별도 (mci-fix-lp). |

## 14. 핵심 차이 / 운영 리스크

### 14.1 stateful session 의 비용

- web (ws) 는 stateless fan-out 이지만 FIX 는 **per-session seq 영속 필수**.
- 다중 인스턴스 운영 시 동일 session 이 다른 instance 에 붙으면 seq mismatch.
- 해법: **sticky LB** (`SenderCompID` hash) 또는 **shared store** (Mongo 등).
- 운영 단순화를 위해 PoC 는 sticky LB + per-instance FileStore.

### 14.2 reject 표면 증가

매매 엔진의 어떤 errn 도 FIX OrdRejReason 으로 매핑돼야 한다. 신규 errn
발생 시 매핑 표 업데이트 필요 — 운영 문서 (`docs/fix-rejcodes.md` 후속) 로
별도 관리.

### 14.3 시간 동기

FIX 의 `52=SendingTime` 은 GMT. NH 운영 머신과 카운터파티 머신의 시간 차가
1초 이상이면 reject 발생 가능 — NTP 보장 필수. (mds 의 운영 표준 그대로)

## 15. 참고

- `pkg/session/types.go:33` — `ChannelFIX` enum (이미 정의)
- `pkg/mymq/conventions.go:60` — `ChannelFix` (향후)
- `internal/price/quote_validation.go` — FIX 4.4 tag 117 QuoteID 검증 (이미 동작)
- `cmd/quote-forwarder/main.go` — FIX 4.4 (35=W/X) 시세 수신 (이미 동작)
- `docs/auth.md` — 인증·권한 위임 원칙 (FIX session 도 동일)
- `docs/customer-connections.md` — ws 고객 접속 관리 (FIX 와의 매핑 비교 가능)
- `docs/push-secret-rotation.md` — mci-push HTTP push 인증 (drop copy 가 사용할 endpoint)
- `internal/edge/price/customer_pair_policy.go` — etcd watch 패턴 (FIX counterparty 등록의 직접 참조 모델)

### 외부

- QuickFIX/Go: https://github.com/quickfixgo/quickfix
- FIX 4.4 spec: https://www.fixtrading.org/standards/fix-4-4/
- OrdRejReason (tag 103) values: FIX 4.4 §4.3

## 16. 다음 단계

이 설계 문서에 운영자/아키텍트가 동의하면:
1. **Phase A (1주)** 부터 시작 — Logon + NewOrderSingle → broker 단방향.
2. PoC 결과 검증 후 Phase B 진입 (drop copy).
3. Phase C 의 mci-admin UI 추가는 customer-pairs PoC 와 동일 패턴 — 빠른 진행
   가능 (1일).

4 결정 (§11) 중 변경이 필요한 부분 있으면 본 문서를 먼저 update — PoC
시작 전에 합의.
