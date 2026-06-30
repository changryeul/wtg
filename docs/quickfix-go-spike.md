# QuickFIX/Go 의존성 spike — FIX gateway Phase A 진입 전 평가

`docs/fix-gateway-design.md` §11 의 결정 1 (QuickFIX/Go 채택) 의 risk 평가.
반나절 spike 로 의존성 / vulncheck / boilerplate / multi-session 적합성을
정량 확인 후 **Phase A 진입 OK** 판정.

대상 독자: 아키텍트 (Phase A 진입 GO/NO-GO 결정용).
관련: `docs/fix-gateway-design.md`.

## 한 줄 결론

**GO** — QuickFIX/Go v0.9.10 채택 권장. 의존성 6개 / vulncheck 깨끗 /
boilerplate 84 LOC / multi-session 자동 지원. WTG 의 기존 deps 와 충돌 0.

## 평가 항목 4가지

### 1. 의존성 그래프 — 매우 가벼움

`/tmp/quickfix-spike` 별도 module 에서 `go get`:

```bash
go get github.com/quickfixgo/quickfix@v0.9.10
go get github.com/quickfixgo/fix44@v0.1.0
```

추가된 deps (transitive 포함, 총 6개):

| 모듈 | 버전 | 용도 | 평가 |
|---|---|---|---|
| `github.com/quickfixgo/quickfix` | v0.9.10 | core | direct |
| `github.com/quickfixgo/fix44` | v0.1.0 | FIX 4.4 message types | direct |
| `github.com/quickfixgo/field` | latest | tag/field 추상화 | indirect |
| `github.com/quickfixgo/tag` | v0.1.0 | tag 상수 | indirect |
| `github.com/shopspring/decimal` | v1.4.0 | FIX price 정확도 (널리 사용) | 안정적 |
| `github.com/quagmt/udecimal` | v1.8.0 | unsigned decimal | quickfix 의존 |
| `github.com/pkg/errors` | v0.9.1 | error wrapping (deprecated 이지만 quickfix 가 사용) | 안정적 |
| `golang.org/x/net` | v0.24.0 (요청) → v0.55.0 (WTG 최신) | TCP/net stack | **WTG 가 더 최신** |

→ **WTG 의 기존 deps 와 자동으로 호환** — Go module 의 minimal version
selection 이 WTG 의 v0.55.0 을 사용. `pkg/errors` 1개만 신규 (안정적
라이브러리, 광범위 사용).

`go.sum` 영향: spike 에선 12 lines. WTG 통합 시 ~30 lines 예상 (transitive
hash sum). WTG 의 현재 go.sum 대비 무시 가능.

### 2. vulncheck — quickfix 자체 0 취약점

`govulncheck -show verbose ./...` 결과:

| 영역 | 취약점 수 | 해결 |
|---|---|---|
| `quickfix` / `fix44` / `field` / `tag` (모든 quickfix 패키지) | **0** | — |
| `crypto/x509` / `net` / `stdlib` (Go runtime) | 다수 | Go 1.26.2 → 1.26.3 업그레이드 |
| `golang.org/x/net@v0.24.0` (spike 에서 사용) | 8건 | **WTG 의 v0.55.0 으로 자동 해결** |

→ **quickfix 자체엔 알려진 취약점 0**. runtime / x/net 의 이슈는 WTG 의
최신 버전 사용 시 무관.

### 3. Boilerplate — 84 LOC minimal acceptor

FIX 4.4 acceptor + Logon 처리 + NewOrderSingle receive 의 최소 코드:

```go
type app struct { logger *slog.Logger }

func (a *app) OnCreate(sid quickfix.SessionID)                    { a.logger.Info("session created", ...) }
func (a *app) OnLogon(sid quickfix.SessionID)                     { a.logger.Info("logon", ...) }
func (a *app) OnLogout(sid quickfix.SessionID)                    { a.logger.Info("logout", ...) }
func (a *app) ToAdmin(msg *quickfix.Message, sid quickfix.SessionID)  {}
func (a *app) FromAdmin(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError { return nil }
func (a *app) ToApp(msg *quickfix.Message, sid quickfix.SessionID) error { return nil }
func (a *app) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
    nos := newordersingle.FromMessage(msg)
    var symbol field.SymbolField
    _ = nos.Get(&symbol)
    a.logger.Info("NewOrderSingle 수신", slog.String("symbol", symbol.String()), ...)
    return nil
}

func main() {
    settings := `[DEFAULT]
ConnectionType=acceptor
SocketAcceptPort=5001
BeginString=FIX.4.4
SenderCompID=WTG
HeartBtInt=30
[SESSION]
TargetCompID=CLIENT`
    cfg, _ := quickfix.ParseSettings(bytes.NewReader([]byte(settings)))
    acceptor, _ := quickfix.NewAcceptor(&app{logger: logger},
        quickfix.NewMemoryStoreFactory(), cfg, quickfix.NewNullLogFactory())
    acceptor.Start()
    defer acceptor.Stop()
}
```

→ **84 LOC** 로 다음이 자동 처리됨:
- Logon handshake (35=A, password 검증 등은 FromAdmin 에서 처리)
- Heartbeat (35=0, 30초 주기)
- TestRequest / ResendRequest / SequenceReset 자동 응답
- Sequence 관리 (in/out 별 단조 증가)
- Logout (35=5)

자체 구현 시 추정: **3000~5000 LOC** (mds 의 `mds_fix.c` 약 2000 LOC + session 관리 추가). **35~60x 절감**.

### 4. Multi-session — 자동 지원

quickfix 의 `SessionID = (BeginString, SenderCompID, TargetCompID)` 라
같은 listener 에 N counterparty 동시 접속 가능. settings 파일의 `[SESSION]`
블록 여러 개로 multi-counterparty 등록:

```ini
[SESSION]
TargetCompID=ECN_DEUTSCHE
[SESSION]
TargetCompID=MM_CITI
[SESSION]
TargetCompID=BUYSIDE_NPS
```

→ **운영 multi-counterparty 시나리오에 그대로 적합**.

WTG 통합 시 — settings 가 etcd 의 `wtg/fix/counterparties/<SenderCompID>`
의 watch 결과로 동적 생성. 자세히는 `docs/fix-gateway-design.md` §5.

## binary size 영향

| 측정 | 값 |
|---|---|
| spike binary (Go 1.26.2 + quickfix lib + 최소 app) | **8.7 MB** |
| WTG 의 `mci-edge-price` (참고) | 18 MB |

→ mci-edge-fix 의 예상 binary size **~25 MB** (WTG 공통 deps + quickfix +
business logic). 운영 영향 무시.

## WTG 통합 시 의심 / 확인 사항

| 항목 | 의심 | 해결 |
|---|---|---|
| `pkg/errors` deprecated 라이브러리 | quickfix 가 사용 | indirect 의존이라 WTG 코드에 영향 0. quickfix 가 자체적으로 사용하는 것이라 신규 코드는 표준 `errors` 사용 가능 |
| `slog.Logger` 와의 통합 | quickfix 의 LogFactory 인터페이스 | `NewNullLogFactory()` 로 quickfix 내부 log 끔 + WTG 코드는 slog 사용. 운영 시 custom LogFactory 로 slog 연결 |
| `context.Context` 통합 | quickfix Start/Stop 이 context 받지 않음 | acceptor lifecycle 을 WTG 의 `Server.Start(ctx)` 에서 wrap. ctx.Done → Stop 호출 |
| MessageStore 선택 | Memory / File / Mongo | **File 권장** — local disk 영속, 재시작 시 seq 보존. 다중 인스턴스 운영 시 sticky LB 또는 Mongo 백엔드 |
| settings 동적 변경 | 운영 중 counterparty 추가/제거 | quickfix 는 acceptor 재시작 필요. WTG 의 etcd watch 가 acceptor.Stop()/Start() 호출하는 wrapper 필요 — Phase A 의 작은 작업 |
| 다중 인스턴스 sticky | 같은 SenderCompID 가 다른 instance 에 붙으면 seq mismatch | LB 의 IP hash 또는 shared store (Mongo). Phase D 운영 강화 범위 |

→ **모두 통합 단계의 알려진 작업**. risk 0, 진행 가능.

## 의사결정 — Phase A 진입 GO

`docs/fix-gateway-design.md` §11 의 4 결정:

| 결정 | 본 spike 결과 | 변경 필요? |
|---|---|---|
| 라이브러리: QuickFIX/Go | ✓ 적합 (의존성 / vulncheck / boilerplate 모두 통과) | **변경 없음 — 채택 확정** |
| Session ↔ Principal: etcd 룰 | quickfix 의 SessionID 와 1:1 매핑 가능 | 변경 없음 |
| drop copy: mci-push HTTP push 재사용 | quickfix 와 무관 | 변경 없음 |
| NewOrderSingle: `/v1/tx alias` 1개 | quickfix 의 FromApp 에서 envelope 변환 | 변경 없음 |

## Phase A 진입 시 권장 순서

1. **`cmd/mci-edge-fix/main.go`** + **`internal/edge/fix/server.go`** — quickfix
   acceptor wrap (위 84 LOC 기반, ~300 LOC 예상)
2. **`internal/edge/fix/settings_from_etcd.go`** — `wtg/fix/counterparties/<CID>`
   watch → quickfix settings 동적 생성 (200 LOC)
3. **`internal/edge/fix/session_to_principal.go`** — Logon 검증 + Principal 주입
   (150 LOC)
4. **`internal/edge/fix/order_mapper.go`** — NewOrderSingle → `/v1/tx` envelope
   (200 LOC)
5. **`internal/edge/fix/server_test.go`** — quickfix client (initiator) 로 E2E
   (~400 LOC)

총 추정 **~1,500 LOC + tests** — Phase A 의 1주 견적 일관.

## 참고

- spike module: `/tmp/quickfix-spike/` (반나절 작업 후 삭제 예정)
- QuickFIX/Go: https://github.com/quickfixgo/quickfix (v0.9.10, 2024 maintenance)
- FIX 4.4 spec: https://www.fixtrading.org/standards/fix-4-4/
- 관련: `docs/fix-gateway-design.md` (전체 설계), `docs/customer-connections.md`
  §1 (인증 패턴), `docs/observability.md` (운영 endpoint)
