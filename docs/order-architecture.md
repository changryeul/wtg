# 주문 아키텍처 — WTG 경계 정리

> **한 줄**: WTG(mci-*)는 **MCI — 다양한 프로토콜(웹/REST·FIX·TCP…)을 받아내는
> 앞단(진입) 게이트웨이**다. 어떤 프로토콜로 들어오든 정규화해 broker(mymq)에 얹고,
> 체결을 화면에 통보한다. **실행·라우팅·매칭·LP 는 WTG 밖** (broker 라우팅 서비스 /
> OMS(FEP) / wfg-rs / LP).
>
> **혼동 금지 3가지**:
> - **WTG = MCI** — 프로토콜 수용/정규화 계층. 라우터도 실행기도 LP 도 아니다.
> - **`dq` ≠ MyMQ(msgq)** — dq 는 OMS 가 쓰는 **별도의 안정 큐 라이브러리**(broker↔OMS,
>   OMS↔daemon 구간). WTG 경계는 **MyMQ(broker)** 까지이고 **WTG 는 dq 를 건드리지 않는다**.
> - **WTG↔OMS 직접 연결은 "멀티캐스트 시세 수신" 하나뿐.** 주문(하향)·체결(상향)은
>   **전부 broker(MyMQ) 경유** — WTG 는 OMS 에 직접 주문을 붙지 않는다. OMS 는 실물로
>   존재하므로 **주문용 스텁 불필요**(WTG 는 broker 에 publish 만, 실 OMS 가 broker 에서 수신).
>   시세 멀티캐스트만 WTG(mci-price/-krx)가 OMS 발 mcast 를 직접 join.
>
> 확정 경위: 사용자 의도 확인 (2026-08). WTG 는 LP(뒷단)가 아니다 — `wtg-fix`(mci-edge-fix-ord)
> 조차 "고객 FIX API 주문의 진입 통로"이지 LP 가 아니다. 어제의 "wfg-rs→WTG FIX GW 카나리"는
> FIX 세션 연결 검증이었을 뿐, 정식 방향은 **고객 FIX → wtg-fix → broker**.

## 1. 전체 흐름

```
[진입 — 하향, 전부 WTG 앞단]          │ WTG 경계 │        [실행 — FEP/wfg, 우리 밖]
 ① 웹 주문화면(FX LP별 / 선물)  /v1/tx ─┐        │
 ② 고객 FIX API                wtg-fix ─┼─▶ broker(mymq) ─(라우팅=broker 서비스)─▶ oms(FEP)
 ③ 레거시 HTS/EMP              edge-tcp ─┘        │            ├ LP별 주문 proc      ├ FX : wfg-rs(FIX) → mock_lp ×4
                                                  │            ├ LP별 MD(시세)       └ 선물: mock_krx
 [시세 — 하향, WTG]                               │            └ 차익 proc(선물+FX 발주)
  mci-price(FX per-source=LP별) ─┐               │
  mci-price-krx(선물)            ─┴─▶ edge-price ─┼─▶ 화면      [체결/응답 — 상향, FEP 내부]
                                                  │            oms 응답/체결 proc ─(dq)─▶ D_FEX(FX)/D_Fut(선물)
 [체결 통보 — 상향, 화면까지]                     │                                         │
  화면 ◀── mci-push(ws) ◀─────────────────────────┴──── (마지막 hop: broker publish 또는 HTTP push) ◀┘
```

## 2. WTG 경계 — 담당 vs 아님

| 구간 | 담당 | WTG 컴포넌트 |
|------|------|--------------|
| **주문 진입** | **WTG** | 웹 `/v1/tx`(passthrough+svcio 조립), 고객 FIX `mci-edge-fix-ord`, 레거시 `mci-edge-tcp` |
| **LP 선택** | 화면/고객이 전문 필드로 지정 → WTG 는 **통과만** | (svcio 명세 필드) |
| **주문 라우팅** | **broker(mymq) 서비스** | WTG 는 alias→exchange/rkey(전송레벨)만. 비즈니스 라우팅 관여 X |
| **LP별 시세** | **WTG** | `mci-price` per-source(BestConsumer/AlgoQuote) → `mci-edge-price` |
| **선물 시세** | **WTG** | `mci-price-krx` → `mci-edge-price` (통합 v2, `?ev=2`) |
| **체결 통보(→화면)** | **WTG** | `mci-push` ws fan-out (트랙A broker rep / 트랙B HTTP push) |
| OMS(FEP): LP별 주문/MD/차익/응답·체결 proc | **FEP** (우리 아님) | — |
| wfg-rs(FIX 엔진), mock_lp/mock_krx, dq, D_FEX/D_Fut | **FEP/wfg 계열** (우리 아님) | — |

## 3. 흐름별 상세

### 3-1. FX 주문 (LP별)
- 화면은 **LP별**로 분리 — 각 화면은 그 LP 의 **per-source 시세**(WTG mci-price 소스별)를 받아 표시.
- 주문은 LP 선택자를 실어 `/v1/tx`(또는 고객 FIX) → broker. broker 라우팅 서비스가 OMS 의 해당 LP 주문 proc 으로 보냄 → wfg-rs → 그 LP 의 mock_lp.
- **mock_lp 4개**(SMB/KMB/EBS/CMB 축) — *작업 중*. FX per-source ↔ 주문 LP 가 같은 축(확정 매핑은 mock_lp 완성 후 잠금).

### 3-2. 선물 주문
- 선물 주문화면 → mci → broker → oms → mock_krx. 시세는 KRX 경로(mci-price-krx, e2e 검증됨).

### 3-3. 차익거래 (서버측 조건주문)
- **OMS 내 차익 proc** 이 선물+FX 를 발주(서버측). LP별 시세도 OMS 가 수신.
- WTG 역할: (미확정) 조건 설정이 화면→mci→broker 로 들어가는지 vs OMS 내부 설정인지 — TBD.

### 3-4. 체결/응답 (상향)
- FX : mock_lp → wfg-rs → oms 응답/체결 proc → dq → **D_FEX** 데몬
- 선물: mock_krx → oms 응답/체결 proc → dq → **D_Fut** 데몬
- **화면까지 마지막 hop 은 WTG mci-push** — 데몬/broker 에서 WTG 로 올라와 사용자별 ws fan-out.

## 4. WTG 각 구간: 지금 되는 것 / 채워야 할 것

### 진입 ① 웹 `/v1/tx`
- 되는 것: generic passthrough, svcio 고정폭 조립, CP949, **회사별 COMHDR**, `ctyp` 세션채널 확립, chain 로그인.
- 채울 것: LP 선택자 필드가 svcio 명세에 있는지 확인, FX/선물 주문 TR alias 라우팅 등록.

### 진입 ② 고객 FIX `wtg-fix`(mci-edge-fix-ord)
- 되는 것: FIX4.4 acceptor(:5001), Logon/Password, 35=D/F/G→`/v1/tx`, etcd 카운터파티 CRUD(admin), drop-copy(`/v1/internal/exec-report`), SIGHUP reload, FileStore, metrics. (세션 검증 완료 = 어제 카나리)
- **채울 것(주문 leg 활성화 조건)**:
  1. ✅ **`FIX_NEW_ORDER`/`FIX_CANCEL_ORDER`/`FIX_REPLACE_ORDER` alias 라우팅 등록 완료** —
     `exchange=OMS, routing_key=NEWORDER/CANCEL/REPLACE`. seed `etc/wtg-routes.json` + etcd 라이브.
     `/v1/tx {alias:"FIX_NEW_ORDER"}` → resolve→publish 검증(OMS 부재로 `errn 1002` = 정상 신호).
  2. **고객 카운터파티 등록** — 현재 `ECN_TEST_01`/`ECN_MD_TEST_01` 뿐 (order_alias=`EXAMPLE` 플레이스홀더). wfg/고객 CID 등록 필요
  3. 심볼 표기 일치(WTG 는 Symbol(55) 무변환 전달)
  4. 동기 35=8(New) ack 여부 결정 — 현재 성공 시 FIX 무응답, 상태는 비동기 drop-copy 로만
  5. TLS/mTLS (DMZ 외부 노출 시, Phase D-2)

### 진입 ③ 레거시 `edge-tcp`
- 되는 것: raw TCP 전문 gateway → `/v1/tx` raw 모드(CP949 무손상).
- 채울 것: 해당 화면(HTS/EMP) 매핑.

### LP별 시세
- 되는 것: per-source(BestConsumer/AlgoQuote), edge-price per-profile fan-out, KRX 경로.
- 채울 것: LP명 ↔ source명 확정 매핑(mock_lp 완성 후), FX 화면의 per-source 구독 배선.

### 체결 통보 `mci-push`
- 되는 것: 트랙A(broker 대표수신)/트랙B(HTTP push), 사용자별 ws fan-out, consistent hash ring.
- 채울 것: **D_FEX/D_Fut → WTG 마지막 hop 방식 확정**(TBD).

## 5. 확정된 사실 (혼동 금지)
- **`dq` = OMS 의 별도 안정 큐 라이브러리** (msgq/MyMQ 아님). 지금까지 OMS 가 **체결·주문
  후처리·응답을 안정적으로 수신**하는 데 썼고, **OMS 로 주문 송신에도 쓸 예정**. 즉
  broker↔OMS · OMS↔daemon 구간의 신뢰 전송 계층. **WTG 무관** (WTG 경계 = MyMQ broker).

## 5a. 시세 수신 목표구조 (확정: OMS 단일수신 + LP별 multicast 재송출)

차익거래로 **OMS 도 시세가 필요** → WTG(quote-forwarder)+OMS 이중 원수신을 없애기 위해
**OMS 를 단일 원 시세 수신자**로 둔다(FEP 는 이미 LP별 마켓데이터 proc 보유). OMS 가
**LP 별로 별도 multicast group 으로 재송출**하고, WTG(mci-price)는 그 group 들을 **직수신**
한다(KRX 의 `mci-price-krx` 멀티캐스트 직수신 패턴과 대칭). → FX/KRX 시세 수신이 대칭이 됨.

```
LP venues(SMB/KMB/EBS/CMB) → OMS(C) [원 수신 + arb 사용]
   │ LP별 multicast group 재송출  (WTG↔OMS 유일 직접 링크 = 시세 mcast)
   ├ SMB group ─┐
   ├ KMB group ─┼─▶ mci-price(FX mcast 수신부, 신규) ─ group→Source(LP) 매핑
   ├ EBS group ─┤     → BestConsumer per (Symbol, LP) → PricingConsumer(마진) → Aggregator(봉)
   └ CMB group ─┘     → mci-edge-price → LP별 화면
```

**역할 분담**
- **OMS (C, 그들)**: 원 LP 시세 수신(이미 함) + arb 사용 + **LP별 group 재송출**(신규 — FEP 가
  이미 소켓/멀티캐스트 다루므로 소품). Go quote-forwarder 로직을 C 로 포팅하는 게 아님.
- **WTG (Go)**: **mci-price 에 FX multicast 수신부 신규**(mci-price-krx 재사용), `group→LP` 매핑
  으로 `Source` 채움. **BestConsumer(per-source)·마진·봉·edge·화면은 무변경**.
- **quote-forwarder**: 실전 시세 경로에서 **빠짐 → test rig 전용**(load-gen 짝, 파이프 회귀 구동).

**현행 LP 구분과의 연속성**: quote-forwarder 는 LP 를 **feed(포트-per-LP)** 로 구분해
`Src=feed label` 을 찍었다. 목표구조는 그 "포트-per-LP" 를 **"group-per-LP"** 로 옮긴 것 —
WTG 는 `group→LP` 매핑만 하면 되고 downstream(Src 기반)은 그대로.

**확정 필요 (구현 전)**
1. **LP ↔ multicast group/port 배정** (SMB/KMB/EBS/CMB = 각 group:port)
2. **wire 포맷** — FIX 35=W? 바이너리 struct(KRX APSISE 류)?
3. **symbol 표기** — quote 검증/BestConsumer 인식 표기와 일치

## 5b. 주문 e2e 테스트 (스텁)
OMS/wfg-rs/그들 mock_lp 준비 전, WTG 두 leg 를 검증하는 도구:
- **진입 leg**: 각 프로토콜(web `/v1/tx` · FIX `fix-tester` · TCP `tcp-tester`)로 주문 →
  WTG 정규화+publish. 등록된 alias 로 broker 도달(수신자 없으면 `errn 1002` = "WTG 는
  제대로 보냄"의 증거).
- **응답/체결 leg**: `cmd/oms-stub` — OMS 체결 응답을 흉내내 **WTG 인바운드 push 경로로
  주입**(mci-push `/v1/internal/push` → 화면 ws, 옵션 mci-edge-fix-ord `/v1/internal/exec-report`
  → FIX 35=8). accept/partial/fill/reject 프리셋. broker 트랜잭션 AP 응답은 pkg/mymq
  (클라 전용)로 불가하므로 broker 를 통하지 않고 체결-수신 endpoint 로 직접 밀어넣음.
  - 검증됨(2026-08): oms-stub accept→partial→fill → edge-push(:8084, --dev) ws 가 화면까지
    3건 수신. "체결통보 마지막 hop = mci-push" 실증.
  - 풀 causal(주문↔체결) 은 broker AP(=실 OMS 또는 C 스텁) 필요 — 현재 test 하네스가 주문
    발주 후 oms-stub 을 같은 파라미터로 호출해 연결.

## 6. 미확정 (TBD — 확정 시 갱신)
1. **체결 화면 도달 마지막 hop**: broker publish→mci-push(대표수신) vs 데몬→HTTP push(트랙B) vs 레거시 edge-tcp?
2. **LP 선택자 필드**: 주문 전문의 어느 필드? (broker 라우팅 서비스가 그 필드로 OMS LP proc 선택)
3. **차익거래 WTG 역할**: 조건 설정이 WTG 경유인가 OMS 내부인가?
4. **mock_lp 4개 ↔ FX per-source(SMB/KMB/EBS/CMB)** 확정 매핑 (mock_lp *작업 중*).
