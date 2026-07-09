# 라우팅 (Routing) 구조

> WTG 의 **transaction alias → MyMQ (exchange, routing_key)** 매핑 계층 명세.
> 코드 진입점: `pkg/routing/`, `internal/api/transform/envelope.go`, `internal/admin/routes.go`.
> 단일 출처 카탈로그(채널/exchange/rkey 상수)는 `docs/conventions.md` + `pkg/mymq/conventions.go`.

이 문서는 처음 읽는 사람도 따라올 수 있도록 비유 + 용어 사전을 앞에 두고,
기술 디테일은 뒤로 갈수록 깊어지는 순서로 정리되어 있다.

---

## 0. 한 장 요약 (먼저 큰 그림)

WTG 시스템을 식당으로 비유하면:

```
손님(클라이언트 앱)  →  카운터(WTG)  →  홀매니저(broker, mymqd)  →  주방(매매 엔진)
```

| 비유           | 실제                              |
| ------------ | ------------------------------- |
| 손님           | 웹/모바일/CS 앱 사용자                  |
| 카운터(WTG)     | mci-api, mci-admin 등 Go 서비스     |
| 홀매니저(broker) | C 로 만든 mymqd. 주문을 어느 주방에 넘길지 분류 |
| 주방(매매 엔진)    | 실제 거래를 처리하는 C 프로그램들             |

**이 문서가 다루는 것**: 손님이 주문한 메뉴 별칭("불고기 정식")을, 카운터(WTG)가
홀매니저가 알아들을 수 있는 내부 코드("KOREAN/BULGOGI") 로 번역하는 과정.

---

## 1. 용어 사전 (처음 보는 단어 정리)

| 단어                 | 한 줄 설명                                                                 |
| ------------------ | ---------------------------------------------------------------------- |
| **broker (mymqd)** | 모든 메시지를 받아서 어디로 보낼지 분류해주는 C 프로그램. 식당 홀매니저.                             |
| **exchange**       | broker 내부의 "주방" 이름. 예: `ORDER`(주문 주방), `ECHOSVC`(테스트 주방).              |
| **routing_key**    | 그 주방의 "메뉴 코드". 예: `NEW`(신규주문), `PING`(핑 인사).                           |
| **transaction**    | 매매 엔진이 처리하는 기능 한 건. 신규주문, 취소, 조회 등.                                    |
| **alias**          | 클라이언트가 보는 짧은 별칭. 예: `WECHO_PING`. WTG 가 진짜 코드로 번역.                     |
| **envelope**       | mci-api 가 받는 JSON 요청 본문 형식. `{alias?, exchange?, routing_key?, data}`. |
| **registry**       | 별칭 ↔ 코드 매핑 사전을 보관하는 저장소.                                               |
| **etcd**           | 분산 key-value 저장소. 여러 서버가 같은 사전을 공유할 때 씀.                               |
| **watch**          | etcd 의 "값 바뀌면 알려주기" 기능. 사전 변경이 모든 서버에 자동 전파됨.                          |
| **passthrough**    | 별칭 없이 raw `(exchange, routing_key)` 로 들어오면 그대로 통과시키는 정책.               |
| **frame**          | broker 와 주고받는 wire 포맷 (84B 헤더 + body).                                 |
| **mqhdr**          | 그 frame 의 84바이트 헤더. exchange/rkey/ckey 등이 들어감.                         |
| **ckey**           | 요청-응답 매칭용 ID. broker 가 응답에 echo back 해서 동시 RPC 처리 가능.                  |
| **DevMode**        | broker 없이 UI/시드만 띄워보는 개발 모드. `mci-admin --dev`.                        |

> 더 깊은 wire 포맷 / mqhdr 필드는 `docs/phase0-analysis.md`, 채널/exchange 카탈로그는
> `docs/conventions.md` 참조.

---

## 2. 레이어 경계 — broker 가 아는 것 vs WTG 만 아는 것

이 문서를 읽다 보면 **alias / Registry / policy / audit / DevMode / 시드…** 등 여러 개념이
나오는데, **이 중 broker 가 아는 것은 단 둘뿐**이다 — `exchange` 와 `routing_key`.
나머지는 모두 WTG (mci-*) 레이어가 broker 앞단에서 처리하고 끝낸다.

### 2.1. broker 가 아는 것 — 84B mqhdr 공통 헤더

`pkg/mymq/types.go:336` 의 `Header` 구조체. broker(mymqd) 가 wire 에서 직접 읽는 프로토콜 필드.

| 필드 | 역할 |
|---|---|
| `Xchg`, `Rkey` | **목적지 라우팅** — broker 가 어느 큐로 분류할지 결정 |
| `Ckey` | request/response 매칭 (correlation id) |
| `Chan[4]` | origin channel (`WEB`/`MOB`/`ADM`/…) |
| `Func`/`Subc`/`Dirf`/`Keyc` | operation type |
| `Navi` | multi-hop 경로 |
| `Pkey`/`Nkey` | pagination |
| `Errn` | 에러 코드 |
| `Wkey` | UI window id |

> **broker 가 인식하는 라우팅 정보는 정확히 `Xchg + Rkey` 두 개.** 나머지 필드는
> broker 가 그냥 운반하거나 운영에 쓰는 메타.

### 2.2. WTG (mci) 만 아는 것 — broker 는 모름

| WTG 개념                                  | broker 인식 | 어디서 처리                                                           |
| --------------------------------------- | --------- | ---------------------------------------------------------------- |
| **alias**                               | ❌         | WTG 가 broker 보내기 *전*에 `exchange/rkey` 로 번역. mqhdr 에 alias 필드 없음. |
| **Registry / Rule**                     | ❌         | mci-admin 이 etcd 에 저장, mci-api 가 watch. broker 무관.               |
| **policy.Engine** (kill switch / 차단 종목) | ❌         | mci-api 가 broker 보내기 *전*에 자체 거부. broker 입장에선 요청이 안 옴.            |
| **AuditRing / ws Hub**                  | ❌         | mci-admin UI 운영 콘솔 부속. broker 와 무관.                              |
| **DevMode / SeedPolicy**                | ❌         | mci-admin 부팅 시 in-memory registry 채우는 dev 편의.                    |
| **Envelope (JSON)**                     | ❌         | HTTP 클라이언트 ↔ mci-api 사이 형식. broker 는 JSON 모름.                    |

### 2.3. 흐름으로 다시 보기

```
[클라이언트]
   │  HTTP + JSON envelope (alias="WECHO_PING")
   ▼
[mci-api]  ← 여기까지가 WTG 전용 레이어:
   │         · policy 차단 검사 (kill switch / 차단 종목)
   │         · alias → exchange/rkey 번역 (Registry)
   │         · 84B mqhdr 빌드
   ▼
[broker, mymqd]  ← 여기부터는 mqhdr 만 봄
   │  Xchg/Rkey/Ckey 로 라우팅
   ▼
[매매 엔진]
```

이 경계가 보이면 다음 섹션들이 더 이해하기 쉬워진다:

- §3 `Rule` 의 필드는 모두 WTG 레이어 자체 메타 (broker 와 안 섞임).
- §6 mci-admin 의 audit/ws push 는 운영 콘솔 부속 — wire 프로토콜과 무관.
- §8 정책(차단)은 broker 가 아는 게 아니라 mci-api 가 broker 앞에서 거르는 것.

---

## 3. 왜 alias 를 도입했나

**문제 시나리오**: 모바일 앱이 `"ORDER" / "NEW"` 같은 broker 내부 코드를 그대로 들고 있다고 가정.

1. **운영 유연성** — 주방 이름을 `ORDER` → `ORDER2` 로 옮기고 싶을 때, 모든 앱 사용자가 업데이트해야 함.
2. **명명 안정성** — 매매 엔진 내부 transaction code 가 외부에 그대로 노출됨.
3. **권한 단축** — "이 alias 잠깐 막아!" 같은 단일 키 차단을 운영자가 한 곳에서 못함.

**해결**: 클라이언트는 **짧고 안정적인 alias** (예: `WECHO_PING`, `ORDER_NEW`) 만 알고,
운영팀이 alias → broker 라우팅을 동적으로 관리.

| 클라이언트가 보내는 별칭 | WTG 가 변환한 진짜 주문                        |
| ------------- | -------------------------------------- |
| `WECHO_PING`  | `exchange=ECHOSVC`, `routing_key=PING` |
| `ORDER_NEW`   | `exchange=ORDER`, `routing_key=NEW`    |
|               |                                        |

> **passthrough 원칙은 유지** — alias 가 등록되어 있지 않으면 envelope 의
> `exchange`/`routing_key` 가 그대로 broker 로 전달된다 (transaction별 핸들러 X).

---

## 4. 데이터 모델 — `routing.Rule`

별칭 사전의 한 줄 = `Rule` 구조체 1개.

`pkg/routing/registry.go`

```go
type Rule struct {
    Alias      string    `json:"alias"`        // 클라이언트가 보는 이름 (≤64B, 공백/슬래시 금지)
    Exchange   string    `json:"exchange"`     // ≤ mymq.LXchg
    RoutingKey string    `json:"routing_key"`  // 필수, ≤ mymq.LRkey
    Active     bool      `json:"active"`       // false 면 Resolve 시 미등록과 동일 처리
    Comment    string    `json:"comment,omitempty"`
    UpdatedAt  time.Time `json:"updated_at"`
    UpdatedBy  string    `json:"updated_by,omitempty"` // 마지막에 바꾼 admin 의 사용자 ID (감사용)
}
```

읽는 법: "**Alias** 라는 별칭으로 들어오면 **Exchange/RoutingKey** 로 번역해서 보내라.
**Active=false** 면 잠시 비활성. **UpdatedBy** 가 누가 마지막에 손댔는지 기록."

**검증 (`Rule.Validate`)** 은 transport-level 만 한다 — 형식·길이 한계 정도.
**비즈니스 규칙(어떤 exchange 가 허용 가능한가) 은 모른다** — 운영자가 admin endpoint 로
직접 입력하므로 폼 검증만. 매매 엔진의 권한 검증과 별개.

---

## 5. 요청이 흐르는 모습 — `POST /v1/tx` 따라가기

핸들러: `internal/api/handlers/transaction.go`
변환 로직: `internal/api/transform/envelope.go`

손님이 `{"alias":"WECHO_PING", "data":""}` 를 보냈다고 하자. 단계별로:

1. **HTTP 도착** — `POST /v1/tx` 로 mci-api 가 받음.
2. **JWT/세션 검증** — middleware 가 "이 손님 누구야?" 확인 (라우팅과 별개 레이어).
3. **JSON 파싱 + 검증** — `Envelope.ValidateRequest()` — 형식 검증.
4. **정책 차단 검사** — `policy.Engine.Check()` — 점검중/차단 종목 등 거부할 사유?
5. **별칭 번역** — `Envelope.BuildFrame()` 안에서 `routing.Resolve(reg, "WECHO_PING")` 호출 → 사전에서 `ECHOSVC/PING` 찾음.
6. **frame 만들기** — 84B mqhdr (`Func=FCTran / Subc=SubTranMsg / Dirf=DirForward`) + body.
7. **broker 전송** — `mq.Call(ctx, frame)`. `pkg/mymq` 가 ckey 를 자동 부여하고 응답 매칭.
8. **응답 그대로 클라이언트** — WTG 는 응답 본문을 해석하지 않고 그대로 전달.

### 별칭 처리 결정 트리

`Envelope.BuildFrame` (`transform/envelope.go:108`) 의 분기:

| envelope                        | Registry 조회                   | 결과                                                    |
| ------------------------------- | ----------------------------- | ----------------------------------------------------- |
| `alias` 채워짐 + 활성 룰 존재           | `routing.Resolve(reg, alias)` | 룰의 `exchange`/`routing_key` 사용 (envelope 의 raw 값은 무시) |
| `alias` 채워짐 + 미등록/비활성           | —                             | `ErrUnknownAlias` → HTTP 404 `unknown_alias` (보수적 거부) |
| `alias` 비어있음 + `routing_key` 있음 | —                             | envelope 의 raw 값 그대로 (passthrough)                    |
| 둘 다 비어있음                        | —                             | HTTP 400 (`ValidateRequest` 실패)                       |

> **왜 명시한 별칭이 미등록일 때 거부하나?** — 클라이언트가 모르는 별칭을 들고 와서 raw 값으로
> fallback 시키는 시도를 막기 위함. "명시한 alias 는 신뢰할 수 없으면 거부" 가 의도된 동작 (보수적).

### 요청 형태 두 가지 (눈으로 확인)

```
클라이언트 → mci-api                              broker → 매매 AP
─────────────────────────────────────             ──────────────────
{                                                 mqhdr {
  "alias": "WECHO_PING",            ───┐            xchg = "ECHOSVC"
  "data": "..."                        │  Resolve   rkey = "PING"
}                                      └──────────► ckey = N
                                                  }
또는 raw (passthrough):                           + body = data
{ "exchange":"ECHOSVC",
  "routing_key":"PING",
  "data": "..." }
```

### 요청 형태 세 번째 — raw 전문 모드 (emp/hts 레거시 채널)

JSON envelope 없이 고정폭 전문 바이트를 **그대로** 주고받는 모드.
레거시 cs-native 클라이언트 (EMP/HTS) 가 기존 전문 조립 코드를 유지한 채
WTG 를 경유하기 위한 경로 — 정책 (kill switch / rate-limit / 감사) 은
JSON 모드와 동일한 핸들러를 지나므로 자동 적용된다.

```
POST /v1/tx
Content-Type: application/octet-stream
Authorization: Bearer <jwt>
X-WTG-Alias: W3100T01                  # 또는 X-WTG-Exchange + X-WTG-Routing-Key
<body = COMHDR+Input 전문 바이트 그대로>

응답 200
Content-Type: application/octet-stream
X-WTG-Errn: 0                          # broker errn (비즈니스 에러여도 body 는 전문 그대로)
<body = 엔진 output 전문 바이트 그대로>
```

- **무변형 통과**: 요청/응답 body 를 WTG 가 해석·변환하지 않는다 — CP949 등
  레거시 인코딩도 무손상. svc I/O 자동 조립 (svcio) 은 이 모드에서 비활성.
- **에러 규약**: 엔진 output 전문이 있으면 errn≠0 이어도 HTTP 200 + body 그대로
  (레거시는 COMHDR 의 eflg/mesg 로 판단). 전문 자체가 없는 transport 에러
  (MB-1002 등) 만 HTTP status 매핑 + text/plain 본문 + `X-WTG-Errn` 헤더.
- **채널**: 로그인 시 `channel: "EMP" | "HTS"` (pkg/session). COMHDR 의 chnl[2]
  업무 코드는 클라이언트가 전문 안에 직접 채운다 — 엔진이 헤더값을 신뢰
  (W3100A01.pc "Header확인" 패턴).

---

## 6. 별칭 사전 = Registry

별칭 ↔ 코드 매핑을 보관하는 저장소.

### 6.1. 인터페이스

`pkg/routing/registry.go`

```go
type Registry interface {
    Get(alias string) (*Rule, error)            // ErrRouteNotFound 가능
    List() []*Rule                              // alias 정렬
    Put(rule *Rule, updatedBy string) error     // 생성 또는 수정 (UpdatedAt/By 자동)
    Delete(alias string) error                  // 미존재 시 ErrRouteNotFound
    SetActive(alias string, active bool, updatedBy string) error
}
```

모든 메서드 goroutine-safe. 호출자(mci-api/mci-admin)는 어느 구현체인지 모르고 인터페이스만 본다.

`routing.Resolve(reg, alias)` 는 mci-api 의 transaction 핸들러용 편의 함수 —
`Get` + `Active` 검사를 묶고, 비활성 룰은 `ErrRouteNotFound` 와 동일하게 처리한다.

### 6.2. 두 가지 구현체

|            | InMemoryRegistry    | EtcdRegistry                  |
| ---------- | ------------------- | ----------------------------- |
| 비유         | 내 노트북 메모장           | 사무실 공용 화이트보드                  |
| 저장 위치      | 프로세스 메모리            | etcd (외부 분산 저장소)              |
| 재시작 시      | 사라짐                 | 보존                            |
| 다중 인스턴스 공유 | ❌                   | ✅                             |
| 사용처        | 단위 테스트, dev/단일 인스턴스 | 운영 (mci-api 여러 대 + mci-admin) |

### 6.3. EtcdRegistry 동작 원리

`pkg/routing/etcd.go`

| 동작                         | 메커니즘                                                           |
| -------------------------- | -------------------------------------------------------------- |
| 룰 저장                       | `<prefix><alias>` 키, JSON value (default prefix `wtg/routes/`) |
| 부팅 시                       | prefix `Get` 으로 로컬 캐시 채움 (이후엔 캐시에서 읽음)                         |
| 변경 감시                      | 백그라운드 goroutine 이 `Watch(prefix)` → 캐시 갱신                      |
| `Get`/`List`               | 로컬 캐시 (etcd round-trip 없음, 핫패스 빠름)                             |
| `Put`/`Delete`/`SetActive` | etcd 에 직접 쓰기 → watch 가 self & 다른 인스턴스에 전파. 일관성 위해 로컬 캐시도 즉시 반영 |

**핵심 아이디어**: 읽기는 빠르게(메모리 캐시), 쓰기는 한 곳(etcd) 으로 — 변경이 watch 통해 모든 인스턴스에 자동 전파.

### 6.4. 팩토리 — 둘 중 어느 걸 쓸지 결정

`pkg/routing/factory.go` 의 `routing.New()` — `Endpoints` 가 비어있으면 `InMemoryRegistry`,
채워져 있으면 `EtcdRegistry`. 호출자가 운영 옵션 분기를 한 곳에서 처리.

---

## 7. 별칭 관리 API — `mci-admin`

운영자가 사전을 CRUD 하는 콘솔. 핸들러: `internal/admin/routes.go`,
라우팅 등록: `internal/admin/server.go:242-246`.

| Method | Path | 동작 |
|---|---|---|
| GET | `/v1/admin/routes` | 모든 룰 정렬 반환 (`{"rules":[...]}`) |
| GET | `/v1/admin/routes/{alias}` | 단건 조회 (404 가능) |
| PUT | `/v1/admin/routes/{alias}` | 생성 또는 수정 — body: `{exchange?, routing_key, active, comment?}` |
| DELETE | `/v1/admin/routes/{alias}` | 삭제 |
| POST | `/v1/admin/routes/{alias}/active` | 활성/비활성 토글 — body: `{active: bool}` |

응답에는 갱신된 `UpdatedAt`/`UpdatedBy` 가 포함된다. `UpdatedBy` 는
JWT/세션의 `Principal.Usid` 에서 자동 채워진다 (`principalUsid`).

### 7.1. 변경하면 자동으로 따라오는 세 가지

모든 변경(`PUT_ROUTE` / `DELETE_ROUTE` / `SET_ROUTE_ACTIVE`)은:

1. **logger.Info** — `auth.md §10` 의 `ADMIN_ACTION` 카테고리 (운영 감사 로그).
2. **AuditRing** — 최근 200건을 메모리 ring buffer 에 보관. UI 화면에서 보여주려고. `internal/admin/audit_ring.go`.
3. **WebSocket 푸시** — `Hub.Broadcast("route", ...)` 로 admin 콘솔 화면에 실시간 반영.

### 7.2. 인증

`/v1/admin/*` 는 admin 인증 (JWT + `ChannelAdmin`) 통과 후에만 핸들러에 도달.
인증 실패는 middleware 에서 거부되므로 핸들러는 `principalRequired` 만으로 의존하지 않는다.

---

## 8. 개발 모드 자동 시드 (DevMode)

운영에선 etcd 가 진실의 원천 → 시드 안 함. 개발할 때만 cfg 파일에서 자동 시드.

`pkg/routing/dev_seed.go`. DevMode (`mci-admin --dev`) 에선
`internal/admin/server.go:139-144` 가 다음을 실행:

```go
policy, _ := routing.ParseSeedPolicy(s.cfg.DevRoutesPolicy)
routing.SeedDevRoutesExPolicy(s.routes, s.logger, s.cfg.DevRoutesFile, policy)
routing.WatchRoutesFilePolicy(ctx, s.routes, s.logger, s.cfg.DevRoutesFile, 2*time.Second, policy)
```

### 8.1. 시드 소스 우선순위

1. `s.cfg.DevRoutesFile` (예: `~/mymq/etc/wtg-routes.json`) — 있으면 사용.
2. 파일 부재/파싱 실패 → hardcode `defaultDevSeeds` (broker entrypoint 가 자동 기동하는
   `TSTSVC_PING`, `WECHO_PING`/`ECHO`/`UPPER`/`TIME`/`INFO`).

### 8.2. JSON 형식

```json
[
  {"alias":"ORDER_NEW","exchange":"ORDER","routing_key":"NEW","active":true},
  ...
]
```
또는 `{"routes":[...]}` wrapping 도 지원.

### 8.3. SeedPolicy — 동기화 정책 (운영자가 의식적으로 선택)

플래그: `-dev-routes-policy`

| 정책              | 동작                                                                         | 사용처                             |
| --------------- | -------------------------------------------------------------------------- | ------------------------------- |
| `additive` (기본) | cfg 의 alias 가 in-memory 에 없으면 추가만. 기존 alias 변경/삭제 무시. UI 에서 즉석으로 추가한 룰을 보존 | UI 위주 작업 시                      |
| `sync`          | cfg 가 진실의 원천. cfg 의 모든 alias upsert(필드 덮어쓰기) + cfg 에 없는 in-memory alias 삭제 | `wtgctl routes set/del` 즉시 반영 시 |

> **sync 모드에서 cfg 파싱 실패는 default 로 fallback 하지 않는다.**
> "sync 약속" 이 깨지므로 안전한 additive 로 처리하고 경고 로그.

### 8.4. Watcher (파일 변경 감지)

`fsnotify` 미사용 (의존성 회피). `os.Stat` mtime polling, 2초 간격.
파일 변경 감지 시 `SeedDevRoutesExPolicy` 재실행. ctx cancel 로 종료.

---

## 9. 라우팅 vs 정책 — 헷갈리지 말 것

둘 다 "차단" 비슷해 보이지만 역할이 다르다.

| | 라우팅 (Routing) | 정책 (Policy) |
|---|---|---|
| 역할 | **번역** (별칭 → 코드) | **차단** (지금 통과시킬까 말까) |
| 비유 | 메뉴판 영문→한글 번역 | 식당 입구 경비원 |
| 변경 시점 | 주방 옮길 때 | 점검중일 때, 특정 종목 일시 차단 |
| 구현 | `pkg/routing` | `pkg/policy` |
| 통과 순서 | 정책 검사 통과 후 → 라우팅 번역 | 라우팅 전에 먼저 검사 |

`internal/api/handlers/transaction.go:64-88` 에서 alias resolve **이전에**
`pkg/policy.Engine.Check()` 가 호출된다.

| 정책 검사 항목 | 거부 시 응답 |
|---|---|
| kill switch (전 시스템 차단) | HTTP 503 `kill_switch` |
| 정비창 (maintenance window) | HTTP 503 `maintenance` |
| 차단 심볼 (envelope.data 의 `symbol` 추출) | HTTP 403 |
| 차단 routing-key | HTTP 403 |

**라우팅과 정책은 별개 레이어다.** 차단된 alias 도 룰은 그대로 존재 —
정책만 토글하면 즉시 풀린다. 비즈니스 권한(거래 한도/통화쌍 활성/거래시간 등)은
매매 엔진에 위임 (`auth.md`).

---

## 10. 다중 인스턴스 동기화 (토폴로지)

```
┌─ mci-admin (운영자) ──────────┐
│  PUT /v1/admin/routes/...     │
│        │                      │
│        ▼                      │
│  EtcdRegistry.Put             │ ──► etcd  ◄── Watch ── EtcdRegistry (mci-api #1, #2, …)
│  (audit + ws broadcast)       │                                │
└───────────────────────────────┘                                ▼
                                                       Resolve → frame → broker
```

- **단일 admin / 다중 api 환경에서 변경은 watch 로 즉시(수십ms) 전파.**
- api 가 부팅 직후엔 prefix `Get` 으로 캐시 채움 — `Resolve` 는 항상 로컬 캐시.
- admin 다중 인스턴스도 가능 (race 시 read-modify-write 충돌은 watch 가 결국 안정 상태로 수렴 — 1차 prototype 은 CAS 미사용. 빈번한 동시 수정이 문제되면 etcd `Txn` 으로 격상).

---

## 11. 설정 키 (mci-admin)

`internal/admin/config.go` — 우선순위는 cfg 파일 < env < flag.

| 키 | flag | env | 의미 |
|---|---|---|---|
| EtcdEndpoints | `-etcd` | `MCI_ADMIN_ETCD` | 비면 in-memory |
| EtcdRoutesPath | `-etcd-prefix` | `MCI_ADMIN_ETCD_ROUTES_PATH` | default `wtg/routes/` |
| DevRoutesFile | `-dev-routes-file` | `MCI_ADMIN_DEV_ROUTES_FILE` | DevMode 시드 JSON |
| DevRoutesPolicy | `-dev-routes-policy` | `MCI_ADMIN_DEV_ROUTES_POLICY` | `additive` (기본) \| `sync` |

`mci-api` 도 동일 etcd endpoint 를 사용해 같은 키 prefix 를 watch
(cfg 키 이름은 mci-api 측 config 참조).

> 모든 서비스의 flag/env 전체 카탈로그와 운영 부트스트랩 순서는
> `docs/operations.md` 가 단일 출처. 본 문서는 라우팅 동작 원리에 집중하고,
> 좌표 일치 (mci-admin ↔ mci-api 의 `-etcd-prefix`/`-etcd-policy-key`) 같은
> 운영 사고 방지 체크리스트는 operations.md §4·§6 참조.

---

## 12. 자주 하는 실수

- **alias 명시했는데 등록 누락** — 클라이언트가 신규 alias 를 `dev-routes-file` 에 넣었지만 admin 이 운영 etcd 에 안 넣음 → 운영 트래픽 404 `unknown_alias`. dev seed 는 운영에 영향 없음.
- **raw envelope 으로 우회 시도** — alias 차단을 raw `(exchange, routing_key)` 로 우회하려는 시도는 정책 엔진의 **차단 routing-key** 룰로 막아야 한다 (alias 차단만으론 부족).
- **`exchange` 빠뜨림** — DIRECT exchange 로 보낼 땐 `exchange` 도 채워야 한다. FANOUT exchange 는 broker cfg 에서 routing key 무시.
- **`UpdatedBy` 가 비어있는 변경** — middleware 가 Principal 을 context 에 안 넣은 경로 (보통 unauthenticated). 운영 endpoint 가 인증 거치는지 확인.

---

## 13. 한 줄 요약

> WTG 의 라우팅 = "**클라이언트가 쓰는 짧은 별칭**(`WECHO_PING`) 을 **broker 가 이해하는 진짜 주소**(`ECHOSVC/PING`) 로 번역해주는 사전. 운영자가 mci-admin 으로 사전을 수정하면 etcd watch 통해 모든 mci-api 인스턴스에 즉시 동기화."

---

## 14. 관련 코드 / 문서

| 위치 | 책임 |
|---|---|
| `pkg/routing/registry.go` | `Rule`, `Registry` 인터페이스, `InMemoryRegistry`, `Resolve` |
| `pkg/routing/etcd.go` | `EtcdRegistry` (watch + 캐시) |
| `pkg/routing/factory.go` | `routing.New(ctx, FactoryOptions)` |
| `pkg/routing/dev_seed.go` | dev seed + watcher + `SeedPolicy` |
| `internal/api/transform/envelope.go` | `Envelope.BuildFrame` (alias resolve) |
| `internal/api/handlers/transaction.go` | `POST /v1/tx` 핸들러 |
| `internal/admin/routes.go` | `/v1/admin/routes/*` CRUD + audit + ws push |
| `internal/admin/server.go` | Registry 부팅 + DevMode 시드 |
| `docs/operations.md` | 서비스별 flag/env 카탈로그 + 부트스트랩 순서 + mci-admin 운영 작업 |
| `docs/conventions.md` | exchange/rkey/channel/queue 카탈로그 |
| `docs/auth.md` | 인증 위임 + admin audit 카테고리 |
| `docs/mci-architecture.md` | 컴포넌트 흐름도 |
| `docs/phase0-analysis.md` | wire 프로토콜 (mqhdr / frame / ckey) 분석 |
