# Cooker Patch — Option A-1 Price Bridge

이 문서는 결정된 [옵션 A-1](phase0-analysis.md#72-mci-price-통합--결정-완료-2026-05-02)의 cooker 측 변경 사항을 정의합니다.

목적: 시세를 myrqd로 발행하는 기존 흐름은 그대로 두고, **mymqd로 동시 발행**해서 mci-price (Go) 가 broker 경유로 시세를 받을 수 있게 합니다.

---

## 1. 변경 범위

| 항목 | 변경 |
|-----|------|
| Cooker 코드 | `myrq_push` 호출부에 `publish_price()` wrapper 도입 (5줄) |
| MyMQ 헤더 | 변경 없음 |
| `mymqd.cfg` | `PRICE` exchange / queue / bind 추가 |
| myrqd / 기존 클라이언트 | 변경 없음 |

기존 `presto`, `vivace`, `znet` 등의 시세 수신은 영향 없음. 신규 mci-price만 broker 경유로 시세 수신.

## 2. C 코드 변경

### 2.1 헬퍼 추가 (공통 lib에 1회)

`src/lib/mymq/` 또는 cooker 자체 lib에 다음 추가:

```c
/* publish_price.c */
#include "mymq.h"

/*
 * publish_price()
 *
 * 시세를 myrqd(SHM ring buffer) + mymqd(broker broadcast) 양쪽에 동시 발행.
 *
 *   pd     : pushdata (cooker가 만든 표준 시세 메시지)
 *   mq     : 사전에 mymq_openx()로 열어둔 broker 핸들 (NULL 허용 — broker 미사용 시 myrq_push만)
 *   xchg   : broker 측 exchange 이름 (보통 "PRICE")
 *
 * 반환: 0 = 모두 성공, -1 = 한쪽 이상 실패 (cooker는 단순 카운터만 올리고 계속)
 */
int publish_price(MyMQ *mq, const char *xchg, struct pushdata *pd)
{
    int rc1, rc2 = 0;

    /* (1) 기존 myrqd 경로 (변경 없음) */
    rc1 = myrq_push(pd);

    /* (2) 신규 broker 경로 */
    if (mq != NULL && xchg != NULL) {
        rc2 = mymq_broadcast(
            mq,
            BROADCAST,                /* sub-function */
            NULL,                     /* ipaddr — local broker만 */
            (char *)xchg,             /* exchange 이름 */
            NULL,                     /* chan */
            NULL,                     /* logon_id (NULL = 전체) */
            (char *)pd,
            sizeof(struct pushdata));
    }

    return (rc1 == 0 && rc2 == 0) ? 0 : -1;
}
```

헤더 (`include/publish_price.h` 또는 cooker 자체 헤더):

```c
int publish_price(MyMQ *mq, const char *xchg, struct pushdata *pd);
```

### 2.2 Cooker 호출부 변경

기존:
```c
struct pushdata pd;
build_pushdata(&pd, symb, tick);
myrq_push(&pd);                          /* ← 기존 */
```

변경 후:
```c
struct pushdata pd;
build_pushdata(&pd, symb, tick);
publish_price(mq_for_broadcast, "PRICE", &pd);   /* ← 변경 */
```

`mq_for_broadcast`는 cooker init 시점에 `mymq_openx()` 로 미리 열어둔 핸들. 운영팀 컨벤션에 맞춰 전역 또는 cooker context 구조체에 보관.

### 2.3 Cooker init 변경 (broker 핸들 1회 오픈)

```c
/* cooker init */
struct instance ins;
memset(&ins, 0, sizeof(ins));
strcpy(ins.my_name, "cooker");      /* 또는 운영 컨벤션 */
ins.ex_type = ET_DIRECT;
ins.qu_attr = QT_CLIENT;

mq_for_broadcast = mymq_openx(&ins);
if (mq_for_broadcast == NULL) {
    mymq_log(LL_ERROR, "broker open failed: continuing with myrqd-only mode");
    /* fallback: 전체 호출에 NULL 전달 → publish_price가 myrq_push만 수행 */
}
```

### 2.4 Cooker shutdown

```c
if (mq_for_broadcast != NULL) {
    mymq_close(mq_for_broadcast);
    mq_for_broadcast = NULL;
}
```

## 3. mymqd 설정 추가

`etc/mymqd.cfg` 에 PRICE exchange/queue/bind 정의 (정확한 문법은 운영팀 형상에 맞춰 조정):

```
exchange PRICE FANOUT
queue    mci_price PUBLIC SHARED
bind     PRICE * mci_price
```

핵심:
- `FANOUT` exchange — routing key 무시, 모든 구독자에게 전달
- `mci_price` queue — 통일된 이름, mci-price가 unsolicited 모드로 connect 시 자동 수신
- 컨벤션 변경 시 mci-price도 동일하게 맞춰야 함

## 4. mci-price (Go) 측 동작

별도 변경 없음. 다음 코드만 보장하면 됨:

```go
c, _ := mymq.Open(ctx, host, port, mymq.Options{
    ApplName: "mci-price",
    // unsolicited 모드는 (TODO Phase 1 jajakta) instance.qu_flag 옵션으로 활성화
})

for msg := range c.Subscribe() {
    if msg.Header.Func != mymq.FCCast { continue }
    if msg.Prefix == nil { continue }
    if msg.Prefix.ExchangeString() != "PRICE" { continue }
    // msg.Body 가 pushdata 페이로드
    handleTick(msg.Body)
}
```

(현재 `mymq.Options`에 `qu_flag` 노출이 없는 상태 — Phase 1 작업으로 추가 예정. 운영자가 cooker 패치를 준비하는 동안 Go 측 옵션을 보강.)

## 5. 검증 절차

1. 신규 cooker (broker publish 추가) 한 인스턴스만 staging에 배포
2. 기존 myrqd 클라이언트 (presto 등) 정상 시세 수신 유지 확인
3. mci-price 가동 → broker 경유 시세 수신 확인 (`mci-price --print=10`)
4. 두 경로의 메시지 일치성 확인 (sequence number, symbol, price 동일)
5. 트래픽 폭주 시나리오: tick 폭주 시 broker drop 없는지 확인 (mymqd 큐 깊이 모니터링)
6. 운영 안정 확인 후 모든 cooker 인스턴스에 배포

## 6. 운영 영향

| 항목         | 영향                                                |
| ---------- | ------------------------------------------------- |
| 기존 시세 트래픽  | 변동 없음 (myrqd 경로 유지)                               |
| broker 트래픽 | 시세 broadcast 추가 (대역폭 ~ tick rate × pushdata size) |
| broker 부하  | 일반적인 FX rates에서 무리 없음. 폭주 시 모니터링                  |
| 메모리        | broker 큐 깊이 ↑ (mymqd.cfg의 queue size 검토)          |
| 장애 격리      | broker 장애 시에도 myrqd 경로는 살아있음 → 기존 클라이언트 영향 없음     |
| 롤백         | publish_price 호출부를 다시 myrq_push로 환원 또는 mq=NULL 강제 |

## 7. 미해결 / 운영팀 합의 필요

| 항목 | 옵션 / 결정 |
|-----|------------|
| Exchange 이름 | "PRICE" 가 적절한가? 사내 컨벤션 확인 |
| Routing key 사용 | FANOUT(무시) vs TOPIC(통화쌍별 필터링) |
| mci-price queue 이름 | "mci_price" 가 적절한가? |
| pushdata 구조 그대로 보낼 것인가 | 또는 broker용 별도 정규화 포맷 도입 (TBD) |
| Compression 적용 여부 | 시세 메시지 ~1.5KB라 미적용 추천 (`WITH_NO_COMPRESS`) |
| HA 시 broker 다중화 | 1차 broker 장애 시 fallback 정책 |
