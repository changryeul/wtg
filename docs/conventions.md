# WTG 명명 컨벤션

WTG (Winway Trading Gateway) 의 채널 구분 / 식별자 / 메시지 라우팅 컨벤션.
운영팀 합의 후 변경 가능하며, 기본값은 `pkg/mymq/conventions.go` 에 코드화되어 있다.

---

## 1. 식별 차원 4가지

WTG 의 메시지 흐름은 다음 4개 식별자로 구분된다:

| 차원 | 식별자 | 누가 채우나 | 위치 | 용도 |
|-----|-------|------------|-----|-----|
| 서비스 | `ApplName` | DECLARE_SESSION | broker session | 모니터링, whois |
| 트래픽 | exchange + rkey | 매 요청 | mqhdr.xchg/rkey | 라우팅, 구독 |
| 사용자 단말 | `Channel` | DECLARE_SESSION + 매 프레임 | mqhdr.chan[4] | 감사, 정책 |
| 개별 사용자 | `cookie.usid` | LOGON | cookie 영역 | push 대상, 권한 |
| 요청 매칭 | `ckey` | 매 요청 | mqhdr.ckey | option C 멀티플렉싱 |

---

## 2. ApplName 컨벤션

**위치**: `DECLARE_SESSION.appl_name` (최대 16바이트)
**상수**: `pkg/mymq/conventions.go` 의 `ApplMci*`

### 형식

```
<service>           단일 인스턴스
<service>-NN        다중 인스턴스 (NN: 01..99)
```

호스트/PID 는 broker 가 connect IP 로 알 수 있으므로 ApplName 에 포함하지 않는다.

### 서비스 카탈로그

| 상수 | 값 | 영역 | 역할 |
|-----|---|-----|-----|
| `ApplMciAPI` | `mci-api` | Internal | REST + sync RPC |
| `ApplMciPush` | `mci-push` | Internal | 체결/주문상태/알림 fan-out |
| `ApplMciPrice` | `mci-price` | Internal | FX 시세 fan-out |
| `ApplMciAdmin` | `mci-admin` | Internal | 관리/Control plane |
| `ApplMciEAPI` | `mci-eapi` | DMZ | edge REST 프록시 |
| `ApplMciEPush` | `mci-epush` | DMZ | edge WebSocket 게이트웨이 |
| `ApplMciEPrice` | `mci-epric` | DMZ | edge 시세 fan-out |

DMZ edge 서비스는 16바이트 한계 + 인스턴스 NN suffix 여유분(3바이트)을 위해
약어를 쓴다. 예: `mci-eapi-01` (11바이트) → broker 에 등록.

### 사용 예시

```go
mymq.Open(ctx, host, port, mymq.Options{
    ApplName: mymq.ApplMciAPI,    // "mci-api"
    Instance: 1,                  // → 최종 "mci-api-01"
    Channel:  mymq.ChannelWeb,
})
```

---

## 3. Channel 코드 (4바이트)

**위치**: `mqhdr.chan[4]` — 매 프레임에 자동 첨부
**상수**: `ChannelCode` 타입의 `Channel*` 값

### 인코딩

문자열을 4바이트로 right-pad with space. 예:
- `"WEB"` → `[]byte{'W','E','B',' '}`
- `"ADM"` → `[]byte{'A','D','M',' '}`

### 카탈로그

| 상수 | 값 | 의미 |
|-----|---|-----|
| `ChannelWeb` | `WEB` | 웹 브라우저 |
| `ChannelMobile` | `MOB` | 모바일 |
| `ChannelCS` | `CS` | 기존 CS 클라이언트 (presto / vivace) |
| `ChannelAdmin` | `ADM` | 직원 어드민 |
| `ChannelFix` | `FIX` | 외부 FIX 카운터파티 |
| `ChannelAPI` | `API` | 외부 REST API 통합 |
| `ChannelBot` | `BOT` | 자동매매 봇 |

### 자동 첨부

`Client.Call()` / `Client.Send()` 호출 시 `FrameInput.Chan` 이 비어있으면
`opts.Channel.Bytes()` 가 자동으로 들어간다. 명시적 override 도 가능.

---

## 4. Exchange / Routing Key 카탈로그

### Exchange

| 상수 | 이름 | 타입 | Producer | Consumer |
|-----|-----|------|---------|---------|
| `ExchangeOrder` | `ORDER` | DIRECT | mci-api | 매매 엔진 |
| `ExchangeExec` | `EXEC` | FANOUT | 매매 엔진 | mci-push |
| `ExchangePrice` | `PRICE` | FANOUT | cooker | mci-price |
| `ExchangeAlert` | `ALERT` | DIRECT | 리스크 엔진 | mci-push |
| `ExchangeSignal` | `SIGNAL` | FANOUT | mci-admin | 모든 mci-* |
| `ExchangeAdmin` | `ADMIN` | DIRECT | mci-admin | mymqd |
| `ExchangeAudit` | `AUDIT` | FANOUT | 모든 mci-* | mci-admin |

### Routing Key (DIRECT exchange 만)

#### ORDER

| 상수 | 값 | 의미 |
|-----|---|-----|
| `RKeyOrderNew` | `NEW` | 신규 주문 |
| `RKeyOrderCancel` | `CANCEL` | 취소 |
| `RKeyOrderModify` | `MODIFY` | 정정 |
| `RKeyOrderQuery` | `QUERY` | 조회 |

#### ADMIN

| 상수 | 값 | 의미 |
|-----|---|-----|
| `RKeyAdminStatus` | `STATUS` | 브로커 상태 |
| `RKeyAdminReload` | `RELOAD` | 정책 리로드 |
| `RKeyAdminShutdown` | `SHUTDOWN` | 그레이스풀 셧다운 |

### FANOUT exchange

`EXEC`, `PRICE`, `SIGNAL`, `AUDIT` 는 routing key 무시. 모든 바인드된 큐에 전달.
`FrameInput.Rkey` 비워두면 됨.

---

## 5. Queue 이름

| 상수 | 값 | 사용처 |
|-----|---|-------|
| `QueueMciAPI` | `mci_api` | mci-api 가 트랜잭션 reply 받을 때 |
| `QueueMciPush` | `mci_push` | mci-push 가 EXEC/ALERT 구독 |
| `QueueMciPrice` | `mci_price` | mci-price 가 PRICE 구독 |
| `QueueMciAdmin` | `mci_admin` | mci-admin 이 AUDIT 등 구독 |

`mymqd.cfg` 에 동일 이름으로 선언되어야 한다.

---

## 6. mymqd.cfg 권장 설정

```
# Exchange 정의
exchange ORDER  DIRECT
exchange EXEC   FANOUT
exchange PRICE  FANOUT
exchange ALERT  DIRECT
exchange SIGNAL FANOUT
exchange ADMIN  DIRECT
exchange AUDIT  FANOUT

# Queue 정의
queue mci_api    PUBLIC SHARED
queue mci_push   PUBLIC SHARED
queue mci_price  PUBLIC SHARED
queue mci_admin  PUBLIC SHARED

# Bind 정의
bind ORDER  *  mci_api      # mci-api 가 보낸 주문은 매매 엔진이 받음 (역방향은 reply)
bind EXEC   *  mci_push     # 체결 broadcast → mci-push
bind ALERT  *  mci_push
bind PRICE  *  mci_price    # 시세 broadcast → mci-price
bind SIGNAL *  mci_admin
bind AUDIT  *  mci_admin
```

(정확한 cfg 문법은 운영팀 형상에 맞춰 조정)

---

## 7. 사용 예시 (서비스별)

### mci-api

```go
c, _ := mymq.Open(ctx, host, port, mymq.Options{
    ApplName: mymq.ApplMciAPI,
    Channel:  mymq.ChannelWeb,
    // unsolicited 미수신 — 요청/응답만 주고받음
})

reply, err := c.Call(ctx, &mymq.FrameInput{
    Func: mymq.FCTran,
    Subc: mymq.SubTranMsg,
    Dirf: mymq.DirForward,
    Keyc: mymq.KeySend,
    Xchg: mymq.ExchangeOrder,
    Rkey: mymq.RKeyOrderNew,
    Body: orderJSON,
})
```

### mci-push

```go
c, _ := mymq.Open(ctx, host, port, mymq.Options{
    ApplName:   mymq.ApplMciPush,
    Channel:    mymq.ChannelWeb,
    QueueFlags: mymq.QfUnsolMsg | mymq.QfUnsolHdr,  // broadcast prefix 포함
})

for msg := range c.Subscribe() {
    if msg.Prefix == nil { continue }
    switch msg.Prefix.ExchangeString() {
    case mymq.ExchangeExec:    fanoutToUser(msg.Prefix.LogonIDString(), msg.Body)
    case mymq.ExchangeAlert:   fanoutToAll(msg.Body)
    }
}
```

### mci-price

```go
c, _ := mymq.Open(ctx, host, port, mymq.Options{
    ApplName:   mymq.ApplMciPrice,
    Channel:    mymq.ChannelWeb,
    QueueFlags: mymq.QfUnsolMsg | mymq.QfUnsolHdr,
})

for msg := range c.Subscribe() {
    if msg.Prefix == nil { continue }
    if msg.Prefix.ExchangeString() != mymq.ExchangePrice { continue }
    handleTick(msg.Body)
}
```

### mci-admin

```go
c, _ := mymq.Open(ctx, host, port, mymq.Options{
    ApplName: mymq.ApplMciAdmin,
    Channel:  mymq.ChannelAdmin,
})

reply, _ := c.Call(ctx, &mymq.FrameInput{
    Func: mymq.FCAdmin,
    Subc: mymq.SubGetStatus,
    Dirf: mymq.DirIoctl,
    Keyc: mymq.KeySend,
    Xchg: mymq.ExchangeAdmin,
    Rkey: mymq.RKeyAdminStatus,
})
```

---

## 8. 운영팀 합의 필요 항목 (Phase 1 마무리 전)

| # | 항목 | 디폴트 | 검토 |
|---|-----|-------|-----|
| 1 | ApplName 명명 (mci-eapi 등) | 본 문서 | broker 콘솔에서 어떻게 보일지 |
| 2 | Channel 4바이트 코드값 | WEB/MOB/CS/ADM/FIX/API/BOT | 더 필요한 코드가 있는지 |
| 3 | Exchange 이름 | ORDER/EXEC/PRICE 등 | 사내 컨벤션과 충돌 없는지 |
| 4 | Routing key 영문 대문자 vs 소문자 | 대문자 권장 | 기존 transaction code 와 정합성 |
| 5 | Queue 이름 (`mci_*`) | 본 문서 | 기존 queue 와 충돌 없는지 |
| 6 | 다중 인스턴스 NN 시작값 | 01 부터 | 또는 호스트 IP 마지막 옥텟 등 |
| 7 | 비즈니스 트랜잭션 매핑 | (TBD) | 기존 PRESTO/VIVACE transaction → ORDER 의 rkey 매핑 |

이 항목들 합의 후 본 문서를 갱신하고, 합의된 값은 `pkg/mymq/conventions.go`
와 `etc/mymqd.cfg` 양쪽에 동일하게 반영한다.
