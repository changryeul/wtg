# edge-tcp 양방향 확장 — cs raw TCP 시세 push 설계 (A안)

> **목적**: mci-price 의 broker 관계를 끊을 때 사라지는 "레거시 cs 시세 수신
> 경로(broker ExchangeQuote subscribe)" 를 대체한다. 레거시 cs 는 raw TCP 라
> mci-edge-price 의 WebSocket 을 못 받으므로, 이미 만든 `mci-edge-tcp` 의
> 지속 TCP 연결 위에 서버 능동 push (unsolicited 시세) 를 얹는다.
>
> **상태**: 설계 (구현 전). 선행 조건 = 이 경로가 서야 broker publish 를 끊을 수 있다.

## 1. 배경 — 왜 필요한가

현재 cs 시세 경로는 2가지:

```
price.PricingConsumer ─(A) broker ExchangeQuote publish → cs (broker subscribe, raw TCP mymq)
                      └(B) gRPC SubscribeQuote → mci-edge-price → WebSocket → 웹/신규
```

broker 부하(SIGABRT)·mymq 의존 제거를 위해 (A) 를 끊으려 하는데, 레거시 cs 는
raw TCP 클라이언트라 (B) 의 WebSocket 을 그대로 못 받는다. 주문 경로는 이미
`mci-edge-tcp` (raw TCP, 4B length-prefix + heartbeat) 로 대칭 이전을 마쳤으나,
**시세 배포(WTG→cs)의 raw TCP last-mile 이 비어 있다.**

A안 = edge-tcp 를 **양방향**으로 확장. 레거시 cs 가 소켓 하나로 주문(sync)과
시세(async push) 를 함께 처리하던 원형에 가장 가깝다.

## 2. 현행 edge-tcp 프로토콜 (확장 전)

- wire: `[4B big-endian length][payload]`, `length==0` = heartbeat (서버 echo).
- 요청/응답: connection 당 **직렬** — 클라가 전문 프레임을 보내면 서버가
  `/v1/tx` (raw 모드) 로 forward 하고 응답 프레임 1개를 돌려준다.
- 즉 **모든 프레임이 클라이언트-주도**. 서버가 능동으로 보내는 것은 heartbeat
  echo 뿐이다.

## 3. 확장 설계 — 프레임 타입 도입

현행은 "프레임 = 전문" 단일 의미라 서버 능동 push 를 구분할 수 없다. **1바이트
타입 태그**를 length 뒤에 추가해 프레임을 다형화한다. 레거시 순수 전문 모드와의
호환을 위해 **협상(negotiation)** 을 둔다.

### 3.1 프레임 포맷 (확장 후)

```
[4B length][1B type][payload]
   │           │        └ type 별 본문
   │           └ 0x00 TX_REQ   (클라→서버) 주문/조회 전문 (기존 payload)
   │             0x01 TX_RESP  (서버→클라) 위 응답 전문
   │             0x02 HEARTBEAT(양방향) payload 없음 (length=1, type 만)
   │             0x03 QUOTE     (서버→클라) 시세 push — payload = 시세 프레임
   │             0x04 SUB       (클라→서버) 시세 구독 요청 — payload = 구독 옵션
   │             0x05 SUB_ACK   (서버→클라) 구독 확인/거부
   └ type 1B 포함한 전체 payload 길이
```

주의: length 는 **type 1B 를 포함**한다 (length≥1). 기존 `length==0` heartbeat
와 겹치지 않게, heartbeat 도 `length=1, type=0x02` 로 승격한다 (아래 호환 참조).

### 3.2 레거시 호환 — 협상

기존 cs 는 type 태그를 모른다. **접속 직후 첫 프레임으로 모드를 확정**:

- 클라가 `0x04 SUB` 또는 명시적 `HELLO`(별도 정의) 를 보내면 → **확장 모드**
  (type 태그 사용, 시세 push 가능).
- 그 외 (기존처럼 `length==0` 빈 프레임 heartbeat 또는 곧바로 전문) → **레거시
  모드** (type 태그 없음, 주문 전용, 시세 push 안 함).

→ 기존 cs 는 코드 변경 없이 주문만 계속 되고, 시세를 받으려는 cs 만 확장 모드로
포팅한다. 점진 마이그레이션 가능.

### 3.3 시세 payload (QUOTE 0x03)

`SubscribeQuote` 의 `CustomerQuote` (proto) 를 cs 가 파싱하기 쉬운 형태로 직렬화.
두 옵션:

1. **고정폭 전문** — 기존 cs 전문 파서 재사용 (pair/bid/ask/ts 고정 오프셋).
   레거시 cs 에 가장 친화적. `docs/cooker-quote-schema.md` 의 v1 평면 envelope
   과 정합 맞추면 파서 공유.
2. **JSON 한 줄** — 유연하지만 cs 파서 신규 필요.

레거시 대상이므로 **(1) 고정폭** 권장. 필드: pair, tenor, bid, ask,
ts_unix_nano, quote_id, valid_until (CustomerQuote 에서 매핑).

## 4. 서버측 구조 (internal/edge/tcp)

### 4.1 upstream 2개

- 주문: 기존대로 `mci-api /v1/tx` (HTTP).
- 시세: **mci-price gRPC `SubscribeQuote`** 를 edge-tcp 가 직접 구독하거나,
  **mci-edge-price 를 재사용**. 후자가 낫다 — edge-price 가 이미 SubscribeQuote
  소비 + Profile/customer-pair allowlist + backpressure 격리를 구현했으므로,
  edge-tcp 가 그 fan-out 결과를 받아 tcp 프레임으로 재전송한다.
  → 결합 방식은 5절 참조.

### 4.2 connection 별 구조 변경

현행 `handleConn` 은 단일 for-loop 로 read→forward→write (직렬). 확장 후:

- **read goroutine**: 클라 프레임 수신 → type 분기 (TX_REQ → forward, SUB →
  구독 등록, HEARTBEAT → echo).
- **write goroutine (또는 write mutex)**: 시세 push 는 서버 주도라 read 와
  독립적으로 프레임을 써야 한다. **단일 conn 에 동시 write 금지** — write 를
  mutex 또는 전용 write goroutine + 채널로 직렬화.
- connection 별 **quote 구독 상태** (구독 pair/profile, ring buffer) 를 connInfo
  에 추가.

### 4.3 backpressure — 느린 cs 격리

시세는 고빈도 push 라 느린 cs 하나가 전체를 막으면 안 된다. edge-price 의
패턴(per-client 채널 + drop 카운터 + 80% WARN) 을 그대로 적용:

- conn 별 bounded 채널 (예: 256). 가득 차면 **최신 우선 drop** (시세는 최신값이
  중요, conflation 성격) + drop 카운터 stats 노출.
- drop 지속 시 연결 종료 후보 (운영 정책).

## 5. mci-price broker 분리와의 관계 (순서)

이 설계의 존재 이유. 무손실 전환 순서:

1. **edge-tcp 시세 push 구현 + cs 확장 모드 포팅** (본 문서).
2. cs 를 broker subscribe → edge-tcp QUOTE push 로 이전. 이중 수신으로 확인
   (`cmd/quote-diff` 류로 broker 경로 vs tcp 경로 envelope 비교).
3. cs 전량 이전 확인 후 **mci-price `--quote-publish-broker=false`** 로 broker
   ExchangeQuote publish 중단.
4. (별개) 시세 fan-in 도 forwarder `--publish-mode grpc` + price `--no-broker`
   로 이전하면 price 가 broker 와 완전 분리.

주의: **매매 transaction (mci-api→broker→매매AP) 의 broker 는 남는다** — 이건
sync RPC 라 별도 과제.

## 6. 관측 / admin

- edge-tcp stats(`/stats`) 에 시세 필드 추가: `quote_subs`(구독 conn 수),
  `quotes_pushed`, `quote_drops`(backpressure), conn 별 구독 pair 목록.
- admin "TCP 게이트웨이" 페이지에 컬럼 추가 (구독 여부 / push 수 / drop).
- 대시보드 토폴로지: edge-tcp 의 upstream 이 mci-api + (신규) mci-price/edge-price
  둘이 되므로 연결선 2개.

## 7. 범위 / 견적 (설계 기준, 구현 시 재산정)

| 작업 | 규모 |
|---|---|
| 프레임 타입 + 협상 + write 직렬화 | internal/edge/tcp 확장, ~중 |
| 시세 upstream 결합 (edge-price 재사용) | ~중 |
| 고정폭 시세 직렬화 (cooker-quote-schema 정합) | ~소 |
| backpressure per-conn | ~소 (edge-price 패턴 복제) |
| cs 확장 모드 클라이언트 (SDK/tcp-tester 확장) | ~중 — cside 대상 |
| stats/admin/토폴로지 | ~소 |

## 8. 열린 질문 (구현 전 확정 필요)

1. **시세 upstream**: edge-tcp 가 mci-price gRPC 를 직접 구독 vs mci-edge-price
   재사용. 재사용이 정책/backpressure 중복을 피하지만 프로세스 간 홉이 하나 늘어남.
2. **인증**: 시세 구독도 주문처럼 gateway 고정 자격(--api-user)으로? 아니면
   connection 별 LOGON(Phase B)에서 Profile 을 확정해 그 Profile quote 만 push?
   후자가 옳지만 LOGON 인증이 선행.
3. **시세 직렬화 스키마**: 레거시 cs 의 기존 시세 전문 포맷과 1:1 매핑 확인 필요
   (실제 cs 파서 스펙 입수).
4. **QUOTE 프레임 빈도**: conflation 을 edge-tcp 에서 한 번 더 할지 (심볼별 최신
   1건 유지) — 느린 cs backpressure 와 연계.
