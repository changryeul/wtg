# 라우팅 (Routing) 구조

> WTG 의 **transaction alias → MyMQ (exchange, routing_key)** 매핑 계층 명세.
> 코드 진입점: `pkg/routing/`, `internal/api/transform/envelope.go`, `internal/admin/routes.go`.
> 단일 출처 카탈로그(채널/exchange/rkey 상수)는 `docs/conventions.md` + `pkg/mymq/conventions.go`.

---

## 1. 왜 alias 인가

매매 transaction 한 건은 broker 입장에서 **(exchange, routing_key)** 한 쌍이다.
클라이언트(웹/모바일/CS/봇)에 raw `(exchange, routing_key)` 를 그대로 노출하면 다음이 깨진다:

- **운영 유연성** — blue-green / canary 시 클라이언트 코드 변경 필요.
- **명명 안정성** — 매매 엔진 내부 transaction code 가 외부에 그대로 박힌다.
- **권한 단축** — 운영자가 "이 alias 막아!" 같은 단일 키로 즉시 차단할 수 없다.

WTG 는 클라이언트가 **짧고 안정적인 alias** (예: `WECHO_PING`, `ORDER_NEW`) 만 알고,
운영팀이 alias → broker 라우팅을 동적으로 관리하도록 한다.

> **passthrough 원칙은 유지** — alias 가 등록되어 있지 않으면 envelope 의
> `exchange`/`routing_key` 가 그대로 broker 로 전달된다 (transaction별 핸들러 X).

---

## 2. 데이터 모델 — `routing.Rule`

`pkg/routing/registry.go`

```go
type Rule struct {
    Alias      string    `json:"alias"`        // 클라이언트가 보는 이름 (≤64B, 공백/슬래시 금지)
    Exchange   string    `json:"exchange"`     // ≤ mymq.LXchg
    RoutingKey string    `json:"routing_key"`  // 필수, ≤ mymq.LRkey
    Active     bool      `json:"active"`       // false 면 Resolve 시 미등록과 동일 처리
    Comment    string    `json:"comment,omitempty"`
    UpdatedAt  time.Time `json:"updated_at"`
    UpdatedBy  string    `json:"updated_by,omitempty"` // admin Usid (감사용)
}
```

**검증 (`Rule.Validate`)** 은 transport-level 만:
`alias` 필수/형식, `routing_key` 필수, exchange/rkey 길이 한계.
**비즈니스 규칙(어떤 exchange 가 허용 가능한가) 은 모른다** — 운영자가 admin endpoint 로
직접 입력하므로 폼 검증만. 매매 엔진의 권한 검증과 별개.

---

## 3. 요청 흐름 — `POST /v1/tx`

`internal/api/handlers/transaction.go` + `internal/api/transform/envelope.go`

```
클라이언트 → mci-api                              broker → 매매 AP
─────────────────────────────────────             ──────────────────
{                                                 mqhdr {
  "alias": "WECHO_PING",            ───┐            xchg = "ECHOSVC"
  "data": "..."                        │  Resolve   rkey = "PING"
}                                      └──────────► ckey = N
                                                  }
또는 raw:                                         + body = data
{ "exchange":"ECHOSVC",
  "routing_key":"PING",
  "data": "..." }
```

`Envelope.BuildFrame` (`transform/envelope.go:108`) 의 결정 트리:

| envelope                        | Registry 조회                   | 결과                                                    |
| ------------------------------- | ----------------------------- | ----------------------------------------------------- |
| `alias` 채워짐 + 활성 룰 존재           | `routing.Resolve(reg, alias)` | 룰의 `exchange`/`routing_key` 사용 (envelope 의 raw 값은 무시) |
| `alias` 채워짐 + 미등록/비활성           | —                             | `ErrUnknownAlias` → HTTP 404 `unknown_alias` (보수적 거부) |
| `alias` 비어있음 + `routing_key` 있음 | —                             | envelope 의 raw 값 그대로 (passthrough)                    |
| 둘 다 비어있음                        | —                             | HTTP 400 (`ValidateRequest` 실패)                       |

이후 frame 은 `Func=FCTran / Subc=SubTranMsg / Dirf=DirForward` 로 구성되어
`mq.Call(ctx, frame)` 로 전송된다. `pkg/mymq` 가 ckey 를 자동 부여하고 응답 매칭.

**clarification**: alias 가 명시됐는데 미등록이면 fallback 하지 않고 거부한다.
"클라이언트가 명시한 alias 는 신뢰할 수 없으면 거부" 가 의도된 동작 (보수적).

---

## 4. Registry 인터페이스

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

모든 메서드 goroutine-safe. 호출자는 어느 구현체인지 모르고 인터페이스만 본다.

`routing.Resolve(reg, alias)` 는 mci-api 의 transaction 핸들러용 편의 함수 —
`Get` + `Active` 검사를 묶고, 비활성 룰은 `ErrRouteNotFound` 와 동일하게 처리한다.

### 4.1. `InMemoryRegistry`

`sync.RWMutex` 기반 단일 프로세스용. 단위 테스트, dev/단일 인스턴스.

### 4.2. `EtcdRegistry`

`pkg/routing/etcd.go` — etcd v3 backed.

| 동작 | 메커니즘 |
|---|---|
| 룰 저장 | `<prefix><alias>` 키, JSON value (default prefix `wtg/routes/`) |
| 부팅 시 | prefix `Get` 으로 로컬 캐시 채움 |
| 변경 감시 | 백그라운드 goroutine 이 `Watch(prefix)` → 캐시 갱신 |
| `Get`/`List` | 로컬 캐시 (etcd round-trip 없음, 핫패스 빠름) |
| `Put`/`Delete`/`SetActive` | etcd 에 직접 쓰기 → watch 가 self & 다른 인스턴스에 전파. 일관성 위해 로컬 캐시도 즉시 반영 |

**운영 토폴로지**: 다중 `mci-api` + `mci-admin`(들) 환경에서 admin 의 변경이
모든 api 인스턴스에 자동 전파.

### 4.3. `routing.New` 팩토리

`pkg/routing/factory.go` — `Endpoints` 비어있으면 `InMemoryRegistry`, 채워져 있으면
`EtcdRegistry`. 호출자(`mci-api`/`mci-admin`)가 운영 옵션 분기를 한 곳에서 처리한다.

---

## 5. 관리 API — `mci-admin`

`internal/admin/routes.go` + `internal/admin/server.go:242-246`

| Method | Path | 동작 |
|---|---|---|
| GET | `/v1/admin/routes` | 모든 룰 정렬 반환 (`{"rules":[...]}`) |
| GET | `/v1/admin/routes/{alias}` | 단건 조회 (404 가능) |
| PUT | `/v1/admin/routes/{alias}` | 생성 또는 수정 — body: `{exchange?, routing_key, active, comment?}` |
| DELETE | `/v1/admin/routes/{alias}` | 삭제 |
| POST | `/v1/admin/routes/{alias}/active` | 활성/비활성 토글 — body: `{active: bool}` |

응답에는 갱신된 `UpdatedAt`/`UpdatedBy` 가 포함된다. `UpdatedBy` 는
JWT/세션의 `Principal.Usid` 에서 자동 채워진다 (`principalUsid`).

### 5.1. 변경 부수효과 — audit + ws stream

모든 변경(`PUT_ROUTE` / `DELETE_ROUTE` / `SET_ROUTE_ACTIVE`)은:

1. **logger.Info** — `auth.md §10` 의 `ADMIN_ACTION` 카테고리.
2. **AuditRing** (200건 ring buffer, UI 표시용) — `internal/admin/audit_ring.go`.
3. **`Hub.Broadcast("route", ...)`** — admin 콘솔 ws stream 에 실시간 푸시.

### 5.2. 인증

`/v1/admin/*` 는 admin 인증 (JWT + `ChannelAdmin`) 통과 후 핸들러에 도달.
인증 실패는 middleware 에서 거부되므로 핸들러는 `principalRequired` 만 의존하지 않는다.

---

## 6. 시드 / Hot reload (DevMode 전용)

`pkg/routing/dev_seed.go` — 운영(etcd 백엔드)에선 **시드 안 함**. etcd 가 진실의 원천.

DevMode (`mci-admin --dev`) 에선 `internal/admin/server.go:139-144` 에서:

```go
policy, _ := routing.ParseSeedPolicy(s.cfg.DevRoutesPolicy)
routing.SeedDevRoutesExPolicy(s.routes, s.logger, s.cfg.DevRoutesFile, policy)
routing.WatchRoutesFilePolicy(ctx, s.routes, s.logger, s.cfg.DevRoutesFile, 2*time.Second, policy)
```

### 6.1. 시드 소스 우선순위

1. `s.cfg.DevRoutesFile` (예: `~/mymq/etc/wtg-routes.json`) — 있으면 사용.
2. 파일 부재/파싱 실패 → hardcode `defaultDevSeeds` (broker entrypoint 가 자동 기동하는
   `TSTSVC_PING`, `WECHO_PING`/`ECHO`/`UPPER`/`TIME`/`INFO`).

### 6.2. JSON 형식

```json
[
  {"alias":"ORDER_NEW","exchange":"ORDER","routing_key":"NEW","active":true},
  ...
]
```
또는 `{"routes":[...]}` wrapping.

### 6.3. SeedPolicy

운영자가 의식적으로 선택해야 한다 (`-dev-routes-policy`):

| 정책 | 동작 | 사용처 |
|---|---|---|
| `additive` (기본) | cfg 의 alias 가 in-memory 에 없으면 추가만. 기존 alias 변경/삭제 무시. UI 에서 즉석으로 추가한 룰을 보존 | UI 위주 작업 시 |
| `sync` | cfg 가 진실의 원천. cfg 의 모든 alias upsert(필드 덮어쓰기) + cfg 에 없는 in-memory alias 삭제 | `wtgctl routes set/del` 즉시 반영 시 |

> **sync 모드에서 cfg 파싱 실패는 default 로 fallback 하지 않는다.**
> "sync 약속" 이 깨지므로 안전한 additive 로 처리하고 경고 로그.

### 6.4. Watcher

`fsnotify` 미사용 (의존성 회피). `os.Stat` mtime polling, 2초 간격.
파일 변경 감지 시 `SeedDevRoutesExPolicy` 재실행. ctx cancel 로 종료.

---

## 7. 운영 정책과의 경계

`internal/api/handlers/transaction.go:64-88` 에서 alias resolve **이전에**
`pkg/policy.Engine.Check()` 가 호출된다.

| 검사 | 거부 시 응답 |
|---|---|
| kill switch (전 시스템 차단) | HTTP 503 `kill_switch` |
| 정비창 (maintenance window) | HTTP 503 `maintenance` |
| 차단 심볼 (envelope.data 의 `symbol` 추출) | HTTP 403 |
| 차단 routing-key | HTTP 403 |

**라우팅(alias resolve) 와 정책(차단)은 별개 레이어다.** 차단된 alias 도 룰은 그대로 존재 —
정책만 토글하면 즉시 풀린다. 비즈니스 권한(거래 한도/통화쌍 활성/거래시간 등)은 매매 엔진에 위임 (`auth.md`).

---

## 8. 토폴로지 / 동기화

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

- 단일 admin / 다중 api 환경에서 변경은 watch 로 즉시(수십ms) 전파.
- api 가 부팅 직후엔 prefix `Get` 으로 캐시 채움 — `Resolve` 는 항상 로컬 캐시.
- admin 다중 인스턴스도 가능 (race 시 read-modify-write 충돌은 watch 가 결국 안정 상태로 수렴 — 1차 prototype 은 CAS 미사용. 빈번한 동시 수정이 문제되면 etcd `Txn` 으로 격상).

---

## 9. 설정 키 (mci-admin)

`internal/admin/config.go` — 환경변수 / 플래그 우선순위는 cfg 파일 < env < flag.

| 키               | flag                 | env                           | 의미                        |
| --------------- | -------------------- | ----------------------------- | ------------------------- |
| EtcdEndpoints   | `-etcd`              | `MCI_ADMIN_ETCD`              | 비면 in-memory              |
| EtcdRoutesPath  | `-etcd-prefix`       | `MCI_ADMIN_ETCD_ROUTES_PATH`  | default `wtg/routes/`     |
| DevRoutesFile   | `-dev-routes-file`   | `MCI_ADMIN_DEV_ROUTES_FILE`   | DevMode 시드 JSON           |
| DevRoutesPolicy | `-dev-routes-policy` | `MCI_ADMIN_DEV_ROUTES_POLICY` | `additive` (기본) \| `sync` |

`mci-api` 도 동일 etcd endpoint 를 사용해 같은 키 prefix 를 watch (cfg 키 이름은 mci-api 측 config 참조).

---

## 10. 자주 하는 실수

- **alias 명시했는데 등록 누락** — 클라이언트가 신규 alias 를 `dev-routes-file` 에 넣었지만 admin 이 운영 etcd 에 안 넣음 → 운영 트래픽 404 `unknown_alias`. dev seed 는 운영에 영향 없음.
- **raw envelope 으로 우회 시도** — alias 차단을 raw `(exchange, routing_key)` 로 우회하려는 시도는 정책 엔진의 **차단 routing-key** 룰로 막아야 한다 (alias 차단만으론 부족).
- **`exchange` 빠뜨림** — DIRECT exchange 로 보낼 땐 `exchange` 도 채워야 한다. FANOUT exchange 는 broker cfg 에서 routing key 무시.
- **`UpdatedBy` 가 비어있는 변경** — middleware 가 Principal 을 context 에 안 넣은 경로 (보통 unauthenticated). 운영 endpoint 가 인증 거치는지 확인.

---

## 11. 관련 코드 / 문서

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
| `docs/conventions.md` | exchange/rkey/channel/queue 카탈로그 |
| `docs/auth.md` | 인증 위임 + admin audit 카테고리 |
| `docs/mci-architecture.md` | 컴포넌트 흐름도 |
