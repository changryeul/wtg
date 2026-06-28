# 고객별 접속 관리 — 한 권 가이드 (예시 포함)

WTG 가 외부 고객의 ws/HTTP 연결을 **누구로 식별하고, 어디에 두고, 무엇을
보내고, 어떻게 격리하는지** 를 한 권으로 정리한다. 신규 운영자/개발자가
"고객 한 명이 로그인부터 quote 수신·종료까지 거치는 모든 지점" 을 머릿속에
그릴 수 있게 한다.

대상 독자: 운영자 / 신규 개발자.
범위: 시세 ws (`mci-edge-price`) 가 중심. push (`mci-edge-push`) 와 HTTP
RPC (`mci-edge-api`) 는 동일 패턴이라 차이점만 짚는다.

## 한 줄 요약

**1 고객 = 1 ws 연결 = 1 `Subscriber` 객체.** edge 단일 프로세스의
`Registry.subs` map 에 모두 들어있고, mci-price 에서 도착한 tick 을
fan-out 한다. 인증은 JWT, fan-out 매칭은 Profile + CustomerID, 격리는
per-conn send queue + slow consumer 자동 Close.

## 1. 한 그림으로

```
┌─────────┐  ① POST /v1/login              ┌────────────┐
│ 고객 SW │ ──────────────────────────────▶│ mci-edge-  │ ──▶ mci-api ──▶ broker
│ (브라우저│  username/password             │   api      │     (Site/Tier resolve + JWT 발급)
│ /앱)    │ ◀──────── { access_token (JWT) }│            │
└────┬────┘                                 └────────────┘
     │ ② GET /v1/subscribe                  ┌──────────────────────────────┐
     │   Authorization: Bearer <jwt>        │ mci-edge-price (DMZ, 1 프로세스)│
     ├─────────────────────────────────────▶│                              │
     │   (ws upgrade)                       │  Registry { subs map[id]*Sub │
     │ ◀── ws established ─────────────────▶│   ├─ Subscriber #1 (alice)   │
     │                                      │   ├─ Subscriber #2 (bob)     │
     │                                      │   ├─ Subscriber #3 (charlie) │
     │ ③ {"action":"subscribe","pairs":[...]│   └─ ...                      │
     │ ─────────────────────────────────────│                              │
     │                                      │ ┌─ gRPC SubscribeQuote ────┐ │
     │ ④ tick JSON 수신 (fan-out)            │ │ ┌─ gRPC SubscribeCust... ▼│
     │ ◀────────────────────────────────────│ │ ┌─ gRPC RegisterCustomer ▼│
     │                                      │ └────────────────────────────┘
     └──────────────────────────────────────└───────┬──────────────────────┘
                                                   │ upstream gRPC (1개)
                                                   ▼
                                              mci-price (Internal)
```

## 2. 핵심 식별자 — 헷갈리지 말 것

| 이름 | 출처 | 용도 | 예시 |
|---|---|---|---|
| **`Usid`** (User ID) | login body / JWT `sub` claim | 운영자가 사람을 가리키는 일상 ID. log/metric 의 label. | `alice123` |
| **`Principal`** | JWT 검증 직후 edge 미들웨어가 채움 (`middleware.PrincipalFromContext`) | 현 요청의 인증된 주체. Usid + Profile 보유. | `Principal{Usid:"alice123", Chan:"WEB", Site:"BRANCH", Tier:"VIP"}` |
| **`ProfileKey`** | `Principal.ProfileKey()` = `Chan.Site.Tier` | quote fan-out 의 매칭 키. 마진 테이블의 routing key. | `"WEB.BRANCH.VIP"` |
| **`CustomerID`** | `Principal.Usid` (Phase 4c — customer-specific 마진 활성 시) | per-customer 마진 + 5-Layer (HQ/Site/Customer/Window) 적용 대상 식별. | `"alice123"` |
| **`Subscriber.id`** | edge 내부 atomic 증가 counter | 한 ws 연결의 내부 ID. log / Snapshot 의 sub_id. | `42` (재시작 후 다시 1부터) |

**중요한 관계**: 같은 고객이 두 번 로그인 → 같은 `Usid` 라도 **두 개의 다른 `Subscriber.id`**.
ws 가 끊겼다 재연결돼도 `Subscriber.id` 는 새로 할당. 운영자가 "현재 alice 의 연결 수" 를
세려면 Usid 로 grep.

## 3. 한 명의 라이프사이클

### 단계 1 — 로그인 + JWT 발급 (`mci-api`)

`POST /v1/login` (`internal/api/handlers/login.go`):

```bash
# 고객 SW 측
curl -X POST https://wtg.example.com/v1/login \
  -H 'Content-Type: application/json' \
  -d '{"usid":"alice123","passwd":"...","channel":"WEB"}'
```

응답:

```json
{
  "ok": true,
  "session_id": "sess-7f3a...",
  "cookie_t": "...",                  // 매매 엔진이 발급한 cookie (passthrough)
  "access_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",  // RS256 JWT
  "expires_in": 900                   // 15분
}
```

JWT payload (`pkg/auth/jwt.go:33` `Claims`):

```json
{
  "sub": "alice123",
  "chan": "WEB",
  "site": "BRANCH",        // 권위 출처: UserProfileResolver (운영 DB / etcd)
  "tier": "VIP",           //                  ↑ 클라이언트의 hint 신뢰 X
  "exp": 1751234567
}
```

**Site/Tier 는 클라이언트가 보내는 게 아니라 서버측 `UserProfileResolver`
가 채움** (`pkg/auth/userprofile.go:14`). 클라이언트의 hint 는 무시 — VIP
참칭 방지.

### 단계 2 — ws upgrade (`mci-edge-price /v1/subscribe`)

`internal/edge/price/server.go:762` `subscribeHandler`:

```bash
# 고객 SW (또는 wscat)
wscat -c 'wss://wtg.example.com/v1/subscribe' \
  -H 'Authorization: Bearer eyJhbGciOi...'
```

edge 측에서 일어나는 일 (순서대로):
1. `middleware.PrincipalFromContext` 가 JWT 검증 후 `Principal` 주입 (Auth 미들웨어).
2. `Principal.Usid` 가 비었으면 `401 unauthorized`.
3. `profileKey := p.ProfileKey()` — `"WEB.BRANCH.VIP"`. 빈값이면 quote 미수신 (raw broadcast 만).
4. `EnableCustomerStream` 활성 시 `customerID := p.Usid` — customer-specific 마진 path 진입.
5. `upgrader.Upgrade(w, r, nil)` — HTTP → ws 프로토콜 전환.
6. `NewSubscriber(ws, SubscriberOptions{SendQueueSize: 256, ProfileKey: "WEB.BRANCH.VIP", CustomerID: "alice123", OnClose: ...})`.
7. `s.registry.Add(sub)` — `Registry.subs[42] = sub` (lock 짧게).
8. `customerRegMgr.Register("alice123", "WEB.BRANCH.VIP")` — mci-price 에 비동기 알림 (gRPC `RegisterCustomer` long-lived stream).
9. `go writeLoop(sub)` + `go readLoop(sub)` — goroutine 2개 가동.

이 시점부터 alice 는 **3 트랙의 quote 후보**:
- (a) raw broadcast — 모든 ws 가 받음
- (b) `WEB.BRANCH.VIP` profile 의 마진 적용 quote
- (c) alice 의 customer-specific 마진 quote (5-Layer 적용)

### 단계 3 — 클라이언트의 `subscribe` 메시지 (pair 필터)

ws 가 열린 직후 클라이언트가 보내는 첫 메시지:

```json
{"action":"subscribe","pairs":["USD/KRW","EUR/USD","JPY/KRW"]}
```

`readLoop` 가 받아 `Subscriber.SubscribePairs([...])` 호출. 이제 alice 의
`Subscriber` 는 **3 pair 만 통과** 시킴. 빈 list (default) 는 **"all 모드"** —
모든 pair 수신.

운영 admin override:
- `PUT /v1/admin/disallow-pair` (mci-edge-price `admin.go:48`) — 모든 subscriber 의
  필터에서 강제 제거 (예: 사고로 GBP/KRW 송신 차단). `RevokePairFromAll(pair)`.

### 단계 4 — fan-out 도착

서버 측에서 tick 이 도착하면 `Registry` 가 어떤 트랙으로 보낼지 결정.
**3 트랙 동시 운영** (`registry.go:348` `Broadcast`, `:436` `SendByProfile`,
`:480` `SendByCustomerID`):

```
gRPC SubscribeQuote stream
   tick 도착 → Registry.Broadcast(payload)       // raw broadcast 트랙
                  └─ 모든 sub 의 send chan 에 enqueue (pair filter 통과 시)

gRPC SubscribeCustomerQuote stream
   customer-tagged quote 도착 → Registry.SendByCustomerID(customerID, pair, payload)
                  └─ subs 중 customerID 일치 1명만
                     없으면 ProfileKey 매칭으로 fallback (Phase 4c)
```

`Subscriber.Send(p)` 는 **non-blocking enqueue**:
```go
select {
case s.send <- p:
    return nil
default:
    return ErrSendQueueFull   // 큐가 가득 — slow consumer 격리 트리거
}
```

`writeLoop` 가 직렬 송신. write deadline 10초.

### 단계 5 — 종료

ws 가 끊기는 4 가지 경로:
1. **클라이언트가 close** — readLoop 가 ws read error → `sub.Close()` → `OnClose` 콜백 → `Registry.Remove(sub)` + `customerRegMgr.Unregister(customerID)`.
2. **slow consumer 격리** — send chan 가득 → `Broadcast` 가 `s.Close()` → 위와 동일.
3. **admin shutdown** — `Registry.CloseAll()` 가 일괄 종료.
4. **JWT 만료** — 다음 요청 401, 클라이언트가 refresh 후 재연결 (Subscriber.id 새로 할당).

## 4. quote fan-out 3 트랙 — 무엇이 누구에게

### 트랙 A — raw broadcast (모든 ws)

mci-price 가 BEST tick 을 fan-out (`gRPC SubscribeQuote stream`).
edge 의 `consumeQuoteOnce` 가 `Registry.Broadcast(payload)` 호출 →
**모든 subscriber 에게 동일 payload**. pair 필터 적용. 마진 0.

**용도**: 운영 대시보드 / 시장가 모니터 / 마진 없는 raw 시장가가 필요한
백오피스 클라이언트.

### 트랙 B — Profile 별 마진 적용 quote (`SubscribeQuote` stream + ProfileKey 매칭)

`PricingConsumer` (mci-price) 가 BEST tick 에 `PricingTable.Apply(profile)`
적용 → ProfileKey 별 quote. `MultiQuotePublisher` 가 fan-out.
edge 가 `Registry.SendByProfile("WEB.BRANCH.VIP", pair, payload)` 호출.

`SendByProfile` 안의 매칭:
```go
for _, s := range r.subs {
    if s.profileKey != profileKey { continue }
    if !s.MatchesPair(pair) { continue }
    s.Send(payload)
}
```

**용도**: 같은 Tier 그룹의 모든 고객 (예: 모든 VIP) 이 동일 마진의 quote.

### 트랙 C — Customer-specific quote (`SubscribeCustomerQuote` stream + CustomerID 매칭)

5-Layer (HQ + Site + Customer + Window) 마진 적용된 per-customer quote.
edge 가 `Registry.SendByCustomerID("alice123", pair, payload)` 호출 →
**alice 1명에게만** (또는 같은 customerID 로 등록된 모든 connection).

**용도**: VVIP / 협상 환율 / 임시 캠페인 마진. profile 위에 customer 단위
미세 조정.

### 어떤 트랙이 켜져 있는가

- 트랙 A 는 항상 ON.
- 트랙 B 는 ProfileKey 가 채워진 ws 만 (JWT 의 Site/Tier 둘 다 있음).
- 트랙 C 는 `cfg.EnableCustomerStream=true` 시. `--enable-customer-stream` flag.

## 5. backpressure / slow consumer 격리

### Send Queue (per Subscriber)

`SendQueueSize=256` (default, `--send-queue` flag). 256 message
buffered channel. 평균 message 100B 가정 → 25 KB / subscriber.

```
send chan ┌─[msg1][msg2]...[msg256]─┐
          └────── cap 256 ──────────┘
            ↑                    ↑
        writeLoop             Broadcast
        (consume)             (enqueue)
```

### 격리 단계 — 80% / 100%

`registry.go:16` + `subscriber.go:278`:

| 점유율 | 동작 |
|---|---|
| 0~80% | 정상 |
| 80%+ | **WARN 누적** — `backpressureWarnTotal++`, 1분당 1회 log (sub_id, depth/cap). `/v1/backpressure` 에 히스토리. |
| 100% | `ErrSendQueueFull` → `Broadcast` 가 `s.Close()` → **slow consumer 강제 격리**. 해당 ws 종료. |

**다른 고객엔 영향 0** — 격리는 per-Subscriber. alice 의 큐가 가득 차도
bob 은 멀쩡히 quote 수신.

### 시나리오 예시 — alice 의 네트워크가 느려짐

```
T=0     alice ws 연결, queue 0/256
T=1s    BEST quote 100 msg/s 도착, alice writeLoop 가 80 msg/s 만 송신
T=10s   alice queue: 200/256 (80% — WARN)
T=15s   alice queue: 256/256 (full → 격리)
        log: "slow consumer 격리 sub_id=42"
        alice ws RST. Subscriber.id=42 가 Registry 에서 제거.
        bob, charlie 등 다른 모든 고객은 영향 0.
T=18s   alice 재접속 → 새 Subscriber.id=99 발급, queue 0/256 부터 다시.
```

## 6. rate-limit — DMZ 의 HTTP 단계

`pkg/ratelimit` — token-bucket per-key. ws upgrade / admin endpoint 같은
HTTP 요청 단계에만 적용 (**ws 안의 메시지 율은 별개 mechanism — backpressure**).

키 결정 (`UserOrIPKey`):
- JWT 검증 후 `X-WTG-Edge-User` 헤더 채워진 요청 → `"user:alice123"` 버킷
- 로그인 전 (또는 헤더 미설정) → `"ip:203.0.113.10"` 버킷

→ 같은 NAT 뒤의 두 사용자가 같은 IP 라도 **인증 후엔 별도 버킷**. 한 명
abuse 가 다른 사용자에 영향 0.

룰 모양 (`pkg/ratelimit/ruleset.go:20`):
```json
[
  {"pattern":"POST /v1/tx",      "rate":50,  "burst":100},
  {"pattern":"GET /v1/subscribe","rate":2,   "burst":3},
  {"pattern":"*",                "rate":20,  "burst":40}
]
```

- 매칭 첫 룰의 limiter 사용. fallback 있으면 fallback. 없으면 통과.
- mci-admin 이 etcd 의 `wtg/ratelimit/edge-price` PolicyDoc 갱신 → 모든
  edge 인스턴스가 atomic Replace → **재배포 없이 즉시 반영**.

### 한계 (반복)

rate-limit 은 **단기 burst 제어 (sec/min 윈도)**. monthly quota / 누적
사용량 metering / bytes 추적은 **없음**. 필요하면 별도 시스템.

## 7. 다중 인스턴스 운영

edge-price 는 stateless 라 N대 동시 운영 가능:

```
        ┌── LB (round-robin / sticky) ──┐
        ▼                                ▼
mci-edge-price #A          mci-edge-price #B
  Registry { alice, bob }    Registry { charlie, dave }
  upstream gRPC ─┐          upstream gRPC ─┐
                 ▼                          ▼
              mci-price (Internal — N edge 가 각자 stream)
```

- 각 인스턴스가 **자기만의 Registry** + 자기만의 upstream gRPC connection.
- mci-price 는 RegisterCustomer 의 customer 별 routing 으로 customer 가
  어느 edge 에 있는지 자동 학습 (Phase 4c).
- **고객 → edge 매핑은 LB 가 결정**. mci-push 의 consistent hash ring
  같은 sticky 보장 없음 (raw broadcast 라 어디든 같은 stream).

### customer-specific quote 의 정확성 보장

문제: alice 가 edge-A 에 ws 연결 → 그동안 mci-price 가 alice 에게 보낼
customer quote 가 생김 → edge-A 가 그걸 받아야 함.

해법: `customerRegManager` 가 ws upgrade 시 gRPC `RegisterCustomer`
stream 에 `(customerID=alice, edgeInstance=A)` 등록. mci-price 의
SubscribeCustomerQuote 가 customer 별 routing 으로 정확히 edge-A 에만
보냄. 끊김 시 self-heal (재연결 후 등록 재전송).

### rate-limit 분산

- in-memory 모드 → 각 edge 가 자기 budget. 같은 user 가 양쪽 instance 에
  나뉘면 약 2x budget (LB 의존).
- Redis 모드 (`--ratelimit-redis`) → 모든 instance 가 같은 token bucket
  공유 (atomic Lua). 진짜 분산 일관.

## 8. 운영 가시화 endpoint

운영자가 "지금 누가 어떻게 접속 중인가" 답을 1초 만에 얻는 경로.

### mci-edge-price (DMZ 측)

**`GET /v1/connections`** — 자기 인스턴스의 모든 ws 연결 detail
(`registry.go:574` `Snapshot`):

```json
[
  {
    "id": 42,
    "profile_key": "WEB.BRANCH.VIP",
    "customer_id": "alice123",
    "remote_addr": "203.0.113.10:54321",
    "queue_depth": 5,
    "queue_cap": 256,
    "pairs": ["USD/KRW","EUR/USD","JPY/KRW"],
    "closed": false
  },
  {
    "id": 43,
    "profile_key": "WEB.BRANCH.GOLD",
    "customer_id": "bob456",
    "remote_addr": "198.51.100.25:11234",
    "queue_depth": 2,
    "queue_cap": 256,
    "pairs": [],                          // all 모드
    "closed": false
  }
]
```

운영자 sanity check:
- `queue_depth / queue_cap > 0.8` 가 누적되면 slow consumer.
- `pairs == []` (all 모드) 가 너무 많으면 message 압박 — 클라이언트
  pair 명시 권장.
- 한 customer_id 가 여러 id (즉 여러 ws 동시 접속) 인지.

**`GET /v1/backpressure`** — 최근 80% WARN 히스토리.

### mci-price (Internal 측)

**`GET /v1/subscribers`** — gRPC stream 카탈로그 (어느 edge 가 어떤
profile/customer 의 stream 을 구독 중인가).

**`GET /v1/customers?customer_id=alice123`** — customer 단일 검색.
alice 가 어느 edge 에 등록돼 있는지, 어떤 quote stream 을 받고 있는지.

**`GET /v1/best-stats`** — 어제 봤듯 per-source bid/ask. customer 무관.

## 9. 시나리오 예시 — alice / bob / charlie 풀 라이프사이클

상황: 3명이 동시에 mci-edge-price #A 에 접속. 각자 다른 Profile/CustomerID.

### t=0 alice (VIP, BRANCH) 로그인

```
POST /v1/login {"usid":"alice123",...}
→ JWT { sub:"alice123", chan:"WEB", site:"BRANCH", tier:"VIP" }
→ ProfileKey="WEB.BRANCH.VIP", CustomerID="alice123"
```

### t=1s alice ws 연결

```
GET /v1/subscribe Authorization: Bearer <jwt>
→ Subscriber{id:1, ProfileKey:"WEB.BRANCH.VIP", CustomerID:"alice123", queue 0/256}
→ Registry.subs[1] = sub
→ customerRegMgr.Register("alice123", "WEB.BRANCH.VIP")
→ mci-price 에 등록 알림 (gRPC RegisterCustomer)
```

ws message:
```json
{"action":"subscribe","pairs":["USD/KRW","EUR/USD"]}
```

### t=2s bob (GOLD, BRANCH) 로그인 + ws

```
→ ProfileKey="WEB.BRANCH.GOLD", CustomerID="bob456"
→ Subscriber{id:2, ...}, Registry.subs[2]
→ pairs:[] (all 모드)
```

### t=3s charlie (STANDARD, HQ) 로그인 + ws

```
→ ProfileKey="WEB.HQ.STANDARD", CustomerID="charlie789"
→ Subscriber{id:3, ...}
→ pairs:["USD/KRW"]   // 1 pair 만
```

### t=5s 시장 tick USD/KRW 도착 — 3 트랙 fan-out

mci-price 에서 도착:

**트랙 A — raw broadcast** (`SubscribeQuote` stream, profile 무관):
```
Broadcast(usdKrwRawPayload)
  → sub#1 alice: pairs ["USD/KRW","EUR/USD"] → ✓ enqueue
  → sub#2 bob:   pairs [] (all)              → ✓ enqueue
  → sub#3 charlie: pairs ["USD/KRW"]         → ✓ enqueue
```

**트랙 B — per-profile**: mci-price 가 각 profile 별로 별도 publish.
- `WEB.BRANCH.VIP` 의 USD/KRW (margin: spread 0.5 추가):
  → `SendByProfile("WEB.BRANCH.VIP", "USD/KRW", payload)` → sub#1 만 enqueue.
- `WEB.BRANCH.GOLD` 의 USD/KRW (margin: spread 1.5 추가):
  → sub#2 만 enqueue.
- `WEB.HQ.STANDARD` 의 USD/KRW: → sub#3 만 enqueue.

**트랙 C — customer-specific** (`EnableCustomerStream=true` + alice 가
협상 환율 있다 가정):
- alice 의 5-Layer 마진 적용된 USD/KRW (HQ + Site + Customer="alice123" rule):
  → `SendByCustomerID("alice123", "USD/KRW", payload)` → sub#1 만 enqueue.
- bob 은 customer rule 없음 → trackC 미발생 → 트랙 B 의 GOLD quote 사용.

결과: alice 는 5초 1tick 동안 **3 message 수신** (raw, VIP, customer).
bob 은 **2 message** (raw, GOLD). charlie 는 **2 message** (raw, STANDARD).

### t=60s alice 네트워크 정체 → 격리

```
T=60-65s alice queue 250/256 (98%) — WARN 누적
T=66s    queue full → ErrSendQueueFull → s.Close() → ws RST
         log: "slow consumer 격리 sub_id=1"
         Registry.Remove(sub#1) + customerRegMgr.Unregister("alice123")
T=70s    bob/charlie 영향 0, 계속 정상 수신
T=120s   alice 재접속 → Subscriber{id:4, ...}, 새로 시작
```

### 운영자가 `GET /v1/connections` 호출 (t=72s)

```json
[
  {"id":2,"profile_key":"WEB.BRANCH.GOLD","customer_id":"bob456","queue_depth":3,"queue_cap":256,"pairs":[],"closed":false},
  {"id":3,"profile_key":"WEB.HQ.STANDARD","customer_id":"charlie789","queue_depth":1,"queue_cap":256,"pairs":["USD/KRW"],"closed":false}
]
```

alice (sub#1) 가 사라졌음을 확인. `GET /v1/backpressure` 에서 격리 사유
확인.

## 10. FAQ

**Q1. 같은 고객이 두 브라우저에서 동시 접속하면?**
A. 두 개의 다른 Subscriber (각자의 id, 각자의 queue). 동일 `CustomerID` 로
customerRegMgr 가 두 번 Register — mci-price 는 customer 매칭 quote 를 두
edge 에 모두 보냄 → 두 ws 다 수신. ws 가 하나 끊겨도 다른 하나는 살아있음
(Register/Unregister 가 ref count 보장).

**Q2. JWT 가 ws 도중에 만료되면?**
A. ws 자체는 유지됨 (한 번 upgrade 한 후엔 재인증 X). 만료 후 다른 HTTP
endpoint (`/v1/admin/...`) 호출은 401. 보안 정책상 더 짧은 ws TTL 이 필요
하면 별도 메커니즘 (writeLoop 에서 만료 검사 후 강제 close) 필요 — 현재
미구현.

**Q3. 고객 한 명이 1만 pair 를 subscribe 하면?**
A. `SubscribePairs` 가 set 에 추가 (idempotent). 메모리는 거의 무시 가능
(string set). 단 모든 pair fan-out 이 다 통과 → message 수 증가 → queue
빨리 참 → 격리 위험. 운영적으로는 admin 측에서 max-pair-per-sub 룰 고려할
가치.

**Q4. mci-edge-price 가 죽으면?**
A. 모든 ws 강제 종료. 고객 SW 가 재연결 시도 → LB 가 다른 인스턴스로 보냄
→ 새 Subscriber 생성. customer 등록도 자동 재구성 (`customerRegMgr` 의
self-heal).

**Q5. 같은 ProfileKey 의 두 명에게 다른 quote 를 보낼 수 있나?**
A. 트랙 B 만으로는 불가능 (Profile 단위). 트랙 C (customer-specific) 활성
+ customer rule 설정 시 가능. 운영자가 mci-admin 의 customer rule CRUD
에서 `customer_id=bob456` 에 별도 마진 추가.

**Q6. 모든 고객 강제 종료 (긴급 시장 폐쇄) 방법?**
A. (a) `Registry.CloseAll()` — admin endpoint 추가 필요 (현재 미노출).
(b) `pkg/policy` 의 kill-switch ON → `mci-edge-price` 의 `policy.IsKilled()`
검사로 ws upgrade 거부 (existing). (c) `mci-admin` 에서 모든 active pair 를
disallow 처리 — `RevokePairFromAll` 누적 호출. 운영 시나리오는 (b) + (c)
조합.

## 11. 한계 / 알려진 미보완

| 항목 | 현 상태 | 비고 |
|---|---|---|
| ws 메시지율 (msg/sec) per-customer 제한 | 없음 (backpressure 만) | 필요 시 Subscriber 단계에 rate-limit 추가 |
| 누적 사용량 metering (bytes/month) | 없음 | rate-limit 은 단기 burst 만 |
| ws 종료 후 customer rule 동기화 | self-heal 으로 충분 | mci-price 의 Register/Unregister event 가 ref count |
| 다중 인스턴스의 customer sticky | 없음 (LB 의존) | mci-push 와 달리 ws 는 stateless 라 OK — 다만 LB 가 round-robin 이면 같은 customer 가 N edge 에 분산됨 |

## 12. 참고

- `internal/edge/price/server.go:756` — `subscribeHandler` (ws upgrade)
- `internal/edge/price/registry.go` — `Registry` / `Subscriber` / fan-out
- `internal/edge/price/customer_reg.go` — `customerRegManager` (gRPC RegisterCustomer)
- `pkg/auth/jwt.go` — `Claims` + `ProfileKey()`
- `pkg/auth/userprofile.go` — `UserProfileResolver` (Site/Tier 권위 출처)
- `pkg/ratelimit/ratelimit.go` — `UserOrIPKey` + token bucket
- `docs/auth.md` — 인증·권한 분담 명세
- `docs/observability.md` — `/v1/connections` 등 운영 endpoint 카탈로그
- `docs/ratelimit.md` — rate-limit 정책 + 튜닝
