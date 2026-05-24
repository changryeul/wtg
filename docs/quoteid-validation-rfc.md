# RFC — NewOrderSingle.QuoteID 검증 API 계약

| 항목 | 값 |
|------|-----|
| 상태 | Draft v1 |
| 작성 | 2026-05-24 |
| 대상 | 매칭 엔진 팀 + WTG 팀 |
| 의존 | [pkg/quoteid (commit e207638)](../pkg/quoteid/) — Registry + Generator |
| 후속 | (4) HA — Redis Sentinel/Cluster + 두 mci-price 인스턴스 |

## 1. Context

WTG 의 mci-price 가 publish 하는 모든 CustomerQuote 에는 (commit e3de924
부터) **FIX 4.4 tag 117 호환 QuoteID** + ValidUntilTime 이 부착된다. 클라이언트가
이 quote 를 보고 주문할 때, NewOrderSingle (FIX 'D') 의 `QuoteID` 필드로 같은
ID 를 들고 온다. 매칭 엔진은 체결 직전에 이 QuoteID 를 검증해서 사용자가
**본 가격** 과 **체결 가격** 의 동치를 보장해야 한다.

이 문서는 매칭 엔진이 호출할 검증 API 의 계약을 정의한다.

### 1.1 표준 정합

- **FIX 4.4 / 5.0 SP2** — tag 117 (QuoteID), tag 62 (ValidUntilTime),
  tag 103 (OrdRejReason), tag 380 (BusinessRejectReason)
- **FX Global Code 2024 (BIS/GFXC) Principle 17** — last-look 의 시간/사유
  투명성. 검증 호출 자체가 audit trail.
- **MiFID II RTS 27** — best-execution: "사용자가 본 가격 → 체결 가격"
  의 lineage 가 감사 가능.
- **EBS Direct / Refinitiv FXall** — QuoteID 기반 NewOrderSingle 이 사실상
  업계 표준 워크플로.

## 2. Goals / Non-goals

### Goals (v1)

1. 매칭 엔진이 단일 RPC 로 QuoteID 의 **존재 / 만료 / 발행자 / 발행 시점 가격** 을 권위 있게 조회.
2. FIX OrdRejReason 매핑이 표준 코드로 가능 (감독당국 호환).
3. mci-price 두 인스턴스 (active-active) 어느 쪽에 물어도 동일 결과 (Redis Registry 공유).
4. p99 검증 RPC < 10ms (Redis lookup + gRPC overhead).

### Non-goals (별도 트랙)

- **MarkConsumed(QuoteID)** — 같은 ID 로 중복 주문 방지. v2 에서 다룬다.
- **BatchValidate** — 대량 주문 효율. v2.
- **HTTP REST 게이트웨이** — 비-Go FIX gateway 호환용. v2.
- **last-look hold-time 자체의 분배 / 트레이딩 정책** — 엔진 책임 (Principle 17).
- **slippage tolerance / side validation** — 엔진 책임.

## 3. Service 위치 결정

후보:
- **(A) mci-price 내부 endpoint** — `PriceService` 와 동일 binary, 동일
  Registry 인스턴스 직접 접근.
- (B) 별도 `cmd/quoteid-svc` — 독립 binary.

**결정: (A) mci-price 에 내장.**

근거:
- mci-price 는 이미 QuoteID 발행자 (single source of truth). 검증을 같은
  binary 에 두면 발급 → 등록 → 검증 path 가 한 프로세스 안에서 결정성 보장.
- HA 는 **(4) 트랙** 의 두 mci-price 인스턴스 자체로 충족 — 엔진은 두 인스턴스
  모두에 dial.
- 운영 단순성 — 추가 binary / 추가 deploy / 추가 모니터링 라인 없음.

(B) 의 장점 (스케일 분리) 는 v1 트래픽 (대형 은행 FX dealing desk 기준 초당
1k~10k 주문) 에서 의미가 없다. v2 에서 재검토.

## 4. API Design

**별도 gRPC service** `wtg.v1.QuoteValidationService` 신설 — `PriceService`
와 분리. 엔진은 quote stream 구독 권한이 필요 없고, 권한/감사 별도 처리.

### 4.1 Proto 초안

```protobuf
syntax = "proto3";
package wtg.v1;

service QuoteValidationService {
  // Validate 는 QuoteID 의 권위 데이터를 조회한다.
  // 엔진은 RPC 결과로 ValidationStatus + Record 를 받아, 자체 정책
  // (slippage / last-look hold / side check) 과 결합해 최종 fill or reject.
  rpc Validate(ValidateRequest) returns (ValidateResponse);
}

message ValidateRequest {
  string quote_id = 1;

  // 호출자(엔진)의 컨텍스트. v1 에서는 audit 로깅 외에 사용하지 않지만
  // v2 의 MarkConsumed / 정책 검증에 미리 자리잡기.
  string engine_id     = 10; // 호출자 식별 (예: "matching-engine-A")
  int64  ts_unix_nano  = 11; // RPC 발신 시각 — clock skew 모니터링용
}

message ValidateResponse {
  ValidationStatus status = 1;

  // Record — status == OK 일 때 채워짐. NOT_FOUND/EXPIRED 시 비어있음.
  QuoteRecord record = 2;

  // FIX OrdRejReason (tag 103) 매핑. status != OK 인 경우 채워짐.
  // 엔진은 ExecutionReport 의 tag 103 에 그대로 넣을 수 있다.
  int32  ord_rej_reason = 3;
  string reject_text    = 4; // 사람용 설명 (감독당국 로그 보조)
}

enum ValidationStatus {
  STATUS_UNSPECIFIED = 0;
  OK                 = 1; // 발행 기록 존재, 아직 ValidUntil 도래 X
  NOT_FOUND          = 2; // Registry 에 없음 (한 번도 발행 안 됐거나 grace 후 GC)
  EXPIRED            = 3; // ValidUntil 도래 후 grace 안 — 기록은 있지만 거래 불가
}

// QuoteRecord — pkg/quoteid.Record 와 1:1.
message QuoteRecord {
  string quote_id              = 1;
  string pair                  = 2;
  string channel               = 3;
  string site                  = 4;
  string tier                  = 5;
  string tenor                 = 6;
  double bid                   = 7;
  double ask                   = 8;
  int64  issued_unix_nano      = 9;
  int64  valid_until_unix_nano = 10;
  uint64 sequence              = 11;
  string issuer                = 12; // mci-price 인스턴스 prefix ("A" / "B")
}
```

### 4.2 정책 분담

| 검사 항목 | 위치 | 비고 |
|----------|------|------|
| QuoteID 존재 | mci-price (Registry.Get) | NOT_FOUND |
| ValidUntil 도래 여부 | mci-price (Registry TTL + grace) | EXPIRED |
| Record echo (사용자가 본 가격) | mci-price | response.record |
| **side validation** (BUY=ask vs SELL=bid) | **엔진** | tag 54 (Side) |
| **slippage tolerance** | **엔진** | tier별 정책 |
| **last-look hold time** | **엔진** | Principle 17, tier별 |
| **사용자 ↔ Profile 일치** | **엔진** | 인증/권한 위임 원칙 |
| **잔량 / 한도** | **엔진** | RTS 27 외 영역 |

WTG 는 "사용자가 본 가격" 의 권위 사본만 제공하고, **거래 적합성 판단은
일체 엔진** — 기존 [auth 위임 원칙](../CLAUDE.md#인증권한-분담) 의 연장.

### 4.3 OrdRejReason 매핑

| ValidationStatus | OrdRejReason (FIX tag 103) | reject_text |
|------------------|---------------------------|-------------|
| OK               | 0 (미사용)                | "" |
| NOT_FOUND        | 5 (Unknown order)         | "quote_id not found" |
| EXPIRED          | 13 (Stale order)          | "quote_id expired" |

엔진이 자체 정책 (slippage 초과 등) 으로 거절하는 경우는 별도 FIX 코드
(`OrdRejReason=3` Order Exceeds Limit 등) — 본 RFC 범위 밖.

## 5. 호출 흐름

```
[client]                  [mci-edge-api]            [mci-price]            [matching-engine]
   |                          |                         |                        |
   |--- NewOrderSingle(D) --->|                         |                        |
   |    QuoteID=A-mq4-1f      |                         |                        |
   |    Side=BUY              |                         |                        |
   |    OrderQty=1m USD       |                         |                        |
   |                          |--- forward order (alias) ----------------------->|
   |                          |    (broker 경유)         |                        |
   |                          |                         |                        |
   |                          |                         |<--- Validate(A-mq4-1f) |
   |                          |                         |     QuoteValidationSvc |
   |                          |                         |                        |
   |                          |                         |---- (Registry.Get) --->|
   |                          |                         |                Redis   |
   |                          |                         |<-------- record -------|
   |                          |                         |                        |
   |                          |                         |---- response (OK+rec) ->|
   |                          |                         |                        |
   |                          |                         |        (side / slippage|
   |                          |                         |         / last-look 검사)|
   |                          |                         |                        |
   |                          |<------- ExecutionReport (fill or reject) --------|
   |<--- forward ExecRep -----|                         |                        |
```

## 6. HA / 배포

```
                              ┌─────────────────┐
                              │ Redis Sentinel/ │
                              │   Cluster       │ (Registry 공유)
                              └────┬───────┬────┘
                                   │       │
            ┌──────────────────────┘       └──────────────────────┐
            ▼                                                       ▼
    ┌───────────────┐                                       ┌───────────────┐
    │ mci-price (A) │                                       │ mci-price (B) │
    │ QuoteID gen=A │                                       │ QuoteID gen=B │
    │ Validate svc  │                                       │ Validate svc  │
    └───────┬───────┘                                       └───────┬───────┘
            │                                                       │
            └─────────────────────┬─────────────────────────────────┘
                                  │
                              ┌───▼──────────────────┐
                              │ matching-engine      │
                              │  (gRPC round-robin   │
                              │   to both A and B)   │
                              └──────────────────────┘
```

- 엔진은 두 mci-price 모두에 gRPC channel 유지 — client-side round-robin
  load balancing (grpc `pick_first` 또는 `round_robin` resolver).
- 한 인스턴스 down → 자동 failover. 두 인스턴스 모두 down → Redis 만으로는
  validate 불가, 신규 주문 거절 (**fail-safe**, FX Global Code Principle 2).
- Redis Sentinel/Cluster 가 단일 장애점 — 그 자체는 운영 표준 패턴 (cf.
  pkg/auth.RedisStore 와 동일 Redis 클러스터 사용 가능).

## 7. Authentication / TLS

내부 통신만 — 외부 노출 없음. mci-chart ↔ mci-price 의 upstream gRPC 와
동일 패턴 (`pkg/tlsutil` reload, mTLS 옵션). 운영 default:
- `--grpc-tls-cert` / `--grpc-tls-key` / `--grpc-tls-ca` (mTLS)
- dev — insecure 허용 (단, log 에 명시적 경고)

## 8. Performance

| 단계 | 예산 |
|------|------|
| 엔진 → mci-price gRPC | 1-3ms (사내망) |
| Registry.Get (Redis Sentinel) | 1-2ms |
| record 직렬화 | <1ms |
| **합계 (p99)** | **~10ms** |

OrderSingle 처리 latency budget 안에 들어옴. 대량 주문 시 BatchValidate (v2)
필요.

## 9. Failure Modes

| 시나리오 | 결과 | 대응 |
|---------|------|------|
| 한 mci-price down | 다른 인스턴스로 자동 failover | 정상 (HA 설계) |
| 두 mci-price 모두 down | 신규 검증 불가 | **신규 주문 거절** — engine 측 circuit breaker |
| Redis 단독 outage | 양쪽 mci-price 가 Registry.Get 실패 | 동일 — 신규 주문 거절 |
| gRPC timeout | engine 측 retry once → fail | OrdRejReason 적절 매핑 |
| QuoteID 발행 시 Redis 일시 장애 | mci-price 가 `quote_register_errors` 증가, publish 는 계속 | 클라이언트 quote 받지만 검증 시 NOT_FOUND → 거절 (fail-safe) |
| Clock skew (engine ↔ mci-price) | ValidUntil 비교 오차 | NTP/PTP 동기화, `ts_unix_nano` 비교 모니터링 |

## 10. Open Questions

1. **gRPC service permission** — 엔진만이 호출자라면 mTLS CN allowlist 면
   충분. 별도 토큰?  
   → v1: mTLS CN 기준.

2. **Validation 호출 자체의 logging granularity** — 모든 호출 로깅하면 양이
   크다 (TPS × N). MiFID II RTS 27 는 "체결된" 호출의 추적은 의무, "거절된"
   호출은 권고.  
   → 기본 INFO: status≠OK 만, DEBUG: 전체.

3. **engine_id 의 미리 등록 여부** — 추적 / 통계 위해 사전 등록 권장.  
   → mci-price config 의 `--known-engines` flag 로 화이트리스트 (옵션).

4. **응답 캐시** — 같은 QuoteID 가 연속 검증되는 경우? 일반적으로 1 QuoteID
   = 1 주문 → 캐시 효과 적음 + caching 은 정합성 risk.  
   → 캐시 안 함.

## 11. 다음 단계

1. 본 RFC 의 매칭 엔진 팀 리뷰 / 합의.
2. proto 정식 채택 (`api/proto/wtg/v1/quote_validation.proto`) — 본 문서의
   초안 [api/proto/wtg/v1/quote_validation.proto](../api/proto/wtg/v1/quote_validation.proto) 를 그대로 시작점으로.
3. mci-price 내 `QuoteValidationService` 구현 — `pkg/quoteid.Registry` 의
   thin wrapper.
4. 엔진 팀이 client stub 생성 → `cookie_t` 흐름에 통합.
5. **(4)** Redis Sentinel/Cluster 셋업 + 두 mci-price active-active 운영
   체크리스트.
