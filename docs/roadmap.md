# MCI 구현 로드맵

마지막 갱신: 2026-05-02
대상: web 기반 외환 MCI 솔루션 (FX 주문/체결/시세/push, FIX/CS/REST 채널)

---

## 0. 현재 위치

### 완료
- **Phase 0** — MyMQ 분석, wire protocol 명세, 통합 전략 결정 ([phase0-analysis.md](phase0-analysis.md))
- **Phase 1 (50%)** — libmymq-go 핵심:
  - frame/codec/types/pubsub: 메시지 인코딩/디코딩 + 단위 테스트 ✅
  - client + handshake + ckey 멀티플렉싱 ✅
  - 자동 heartbeat ✅
  - mci-test (검증 CLI) ✅
  - mci-price 프로토타입 ✅
  - 통합 테스트 스캐폴드 ✅
  - cooker 패치 명세 ([cooker-patch.md](cooker-patch.md)) ✅

### 진행 중 / 차단
- mymqd 실 검증 대기 — ckey echo 분기점 (Phase 1 GO/NO-GO)
- cooker 패치 운영팀 적용 대기

---

## 1. 단계 개요 (Phase 1 → 9)

| Phase | 목표 | 결과물 | 기간 추정 | 의존 |
|-------|------|--------|----------|------|
| **1** | libmymq-go 완성 | 재사용 가능한 Go 클라이언트 | 1.5주 | mymqd 접근 |
| **2** | mci-api | REST → mymq_call sync RPC | 2주 | Phase 1 |
| **3** | mci-push | WebSocket + unsolicited fan-out | 2주 | Phase 1 |
| **4** | mci-price | 시세 수신 + edge fan-out 준비 | 2주 | Phase 1, cooker 패치 |
| **5** | DMZ edge 3종 | mci-edge-{api,push,price} | 3주 | Phase 2~4 |
| **6** | mci-admin | 운영 6대 기능 control plane | 3주 | Phase 1 |
| **7** | 운영 기능 통합 | Session/Routing/Policy/Monitoring/RateLimit/Failover | 3주 | Phase 6 |
| **8** | 보안 / HA / 프로덕션 | 인증/감사/이중화/관제 | 3주 | Phase 1~7 |
| **9** | 마이그레이션 + 인계 | 운영팀 인수, 점진 도입 | 2주 | 전체 |

**합계 추정**: ~22주 (5개월). 병렬화 시 단축 가능.

---

## 2. Phase별 상세

### Phase 1 — libmymq-go 완성 (잔여 1.5주)

#### 잔여 작업
- [ ] **Options.Unsolicited** — `qu_flag` 옵션 노출. 현재 미노출.
- [ ] **자동 재연결** — heartbeat 타임아웃 시 reconnect + 핸드셰이크 재시도 + pending 에러 통보 + subscription 재등록
- [ ] **압축 디코딩** — ZLIB 우선, MLZO 차순. 송신측 압축은 선택적
- [ ] **에러 핸들링 표준화** — mymq.Error` 타입, errn 매핑
- [ ] **Logger 인터페이스** — 구조화 로깅 hook (slog 호환)
- [ ] **mci-test 확장** — `--load N` 으로 N개 동시 호출 부하 검증

#### Acceptance criteria
- ckey echo 검증 PASS
- 동시 호출 1000개 부하 테스트 통과
- 재연결 시나리오 (서버 재시작) 자동 복구
- 단위 테스트 커버리지 80%+
- `go vet`, `staticcheck` 클린

#### 리스크
- ckey echo FAIL 시 → connection pool 모드로 전환 (1주 추가)

---

### Phase 2 — mci-api (2주)

REST 기반 sync RPC. 가장 단순한 채널이라 우선 구현.

#### 모듈
```
cmd/mci-api/main.go
internal/api/
├── server.go         chi router + middleware
├── handlers/
│   ├── order.go      POST /v1/orders, GET /v1/orders/{id}
│   ├── execution.go  GET /v1/executions
│   ├── position.go   GET /v1/positions
│   └── ping.go       GET /v1/ping
├── auth/
│   └── jwt.go        JWT 검증 (운영팀 SSO/IdP 연동)
├── transform/
│   ├── inbound.go    JSON → mymq content
│   └── outbound.go   mymq reply → JSON
└── config.go         YAML 설정 파싱
```

#### 동작
1. JSON POST 수신
2. JWT/cookie 검증 → mymq.Cookie 구성
3. transform.Inbound: JSON → MyMQ content (xchg/rkey 매핑 테이블 기반)
4. `client.Call(ctx, frameInput)` → reply 대기
5. transform.Outbound: reply → JSON
6. 적절한 HTTP status로 회신

#### Acceptance criteria
- 핵심 transaction 5개 (예: NewOrder/CancelOrder/QueryOrder/QueryPosition/Ping) 동작
- 응답 시간 P99 < 200ms (ms 단위 broker 응답 + JSON 변환)
- 인증 실패 시 401, 시간 초과 시 504, 비즈니스 에러 시 4xx + errn payload
- OpenAPI 스펙 자동 생성 (chi + huma 추천)
- `wrk` 부하 테스트 1000 RPS sustained

#### 합의 필요
- JSON 메시지 스키마 (운영팀의 기존 transaction 매핑)
- 인증 방식 (JWT vs 세션 토큰)
- 라우팅 키 매핑 테이블 (transaction code 결정)

---

### Phase 3 — mci-push (2주)

WebSocket 기반 단방향 push. 체결, 주문 상태 변경, 알림.

#### 모듈
```
cmd/mci-push/main.go
internal/push/
├── server.go         WebSocket upgrade + 인증
├── session/
│   ├── registry.go   logon_id ↔ ws.Conn 매핑 (sync.Map + ringbuffer 큐)
│   └── lifecycle.go  ping/pong, idle 종료
├── dispatcher.go     mymq.Unsolicited → 사용자 fan-out
├── transform/
│   └── outbound.go   broadcast prefix → JSON
└── config.go
```

#### 동작
1. mymq.Client 1개 (또는 소수) — unsolicited 모드 connect
2. WebSocket 클라이언트 접속 → 로그인 → `logon_id` 저장
3. Subscribe 채널에서 메시지 받음 → `Prefix.LogonID` 또는 `Channel` 보고 매칭
4. 매칭된 ws session에 JSON 직렬화해서 push
5. 미매칭 메시지는 metric만 카운트

#### Acceptance criteria
- 동시 ws 접속 5,000개 (1대 호스트 기준)
- 사용자별 push 지연 P95 < 100ms (broker 수신 → 클라이언트 도달)
- ws 클라이언트 끊김 시 자동 cleanup
- registry 메모리 누수 없음 (24시간 soak)
- backpressure: 느린 ws 클라이언트 격리, 다른 사용자 영향 없음

#### 합의 필요
- ws JSON envelope 포맷 (e.g., `{ "event": "execution", "data": {...} }`)
- 사용자 ↔ logon_id 매핑 정책
- 끊김 후 재접속 시 미수신 메시지 보충 정책 (snapshot + delta)

---

### Phase 4 — mci-price (2주)

대량 시세 fan-out. 옵션 A-1 채택 가정.

#### 사전 조건
- cooker 패치 적용 ([cooker-patch.md](cooker-patch.md))
- mymqd.cfg 에 PRICE exchange 추가
- 운영팀 staging 환경에 배포

#### 모듈
```
cmd/mci-price/main.go     (Phase 1 프로토타입을 본격화)
internal/price/
├── server.go             mymq Subscribe + filter
├── ring/
│   └── conflation.go     심볼별 latest 보관, 폭주 시 drop intermediate
├── decoder.go            pushdata → Tick (정규화)
├── fanout.go             gRPC stream으로 mci-edge-price에 전송
├── metrics.go            tick rate, conflation drop, fan-out lag
└── config.go
```

#### Tick 모델
```go
type Tick struct {
    Symbol    string
    Bid       float64
    Ask       float64
    BidSize   int64
    AskSize   int64
    Timestamp time.Time
    SeqNum    uint32
}
```

(실제 필드는 `pushdata` 구조 + 운영 사양 합의 후 확정)

#### Acceptance criteria
- 초당 5만 tick sustained 처리
- conflation 적용으로 edge 부하 최소화 (느린 구독자에게도 latest만 전달)
- 주요 통화쌍 50개 동시 broadcast
- 메모리 사용 안정화 (24시간 soak)
- 시세 누락 검출: SeqNum 불연속 시 alert

#### 리스크
- broker 라우팅이 시세 fan-out 부하를 못 견딤 → mymqd 튜닝 또는 옵션 B 검토
- pushdata 정규화 로직 복잡도 → 사양 명세 우선 확정

---

### Phase 5 — DMZ Edge 3종 (3주)

DMZ 분리 기준에 따라 mci-{api,push,price} 각각의 edge 프록시.

#### 공통 책임
- TLS termination
- JWT 검증 (Internal에서 발급된 토큰의 서명만 확인)
- IP/사용자 단위 rate limit
- 로깅 (audit + 비즈니스)
- HTTP/HTTPS만 외부 노출, Internal과는 mTLS gRPC

#### mci-edge-api (REST 프록시)
```
cmd/mci-edge-api/
internal/edge/api/
├── proxy.go      Internal mci-api로 gRPC 호출 또는 REST 프록시
├── auth.go       JWT 검증
├── ratelimit.go  토큰 버킷 (Redis backed)
└── headers.go    request_id 주입, server header 제거
```

#### mci-edge-push (WS 게이트웨이)
- 클라이언트 ws 유지
- Internal mci-push와는 gRPC bidirectional stream
- Internal → DMZ stream으로 push 메시지 받아서 ws에 전달
- DMZ → Internal stream으로 ws에서 받은 명령 (대부분 ping/subscribe) 전달

#### mci-edge-price (시세 fan-out edge)
- mci-price에서 gRPC stream으로 tick 수신
- 클라이언트별 ws session에 fan-out
- conflation 재적용 (느린 ws 클라이언트별)
- 다수 클라이언트 (수만) 동시 처리 → goroutine + channel 패턴

#### Acceptance criteria
- DMZ → Internal 연결만 정방향 (Internal → DMZ는 reverse stream)
- 보안: TLS 1.3, mTLS, HSTS, CSP, X-Frame-Options
- 부하: edge-price 1대당 ws 10,000 동시
- 장애 격리: edge 죽어도 Internal 영향 없음, edge 재시작 시 자동 reconnect
- ddos 대응: 비정상 트래픽 자동 차단

#### 합의 필요
- DMZ ↔ Internal 사이 방화벽 룰 (포트 1개만 열기)
- 인증서 발급 절차 (Let's Encrypt vs 사내 CA)
- DDoS 보호 layer (Cloudflare 등)

---

### Phase 6 — mci-admin (3주)

직원용 control plane. 운영 6대 기능을 관리하는 단일 진입점.

#### 모듈
```
cmd/mci-admin/main.go
internal/admin/
├── api/                  Admin REST API (Internal-only)
│   ├── session.go        세션 조회/강제종료/통계
│   ├── routing.go        라우팅 룰 CRUD
│   ├── policy.go         정책 CRUD (주문 한도, 슬리피지, 거래시간 등)
│   ├── monitoring.go     실시간 KPI, 트랜잭션 활동
│   ├── ratelimit.go      rate limit 룰 + 현재 상태
│   └── failover.go       FIX 세션 상태, SeqNum, 재연결 trigger
├── store/                config 영속화
│   ├── postgres.go       또는 etcd/consul
│   └── pubsub.go         설정 변경 → 데이터플레인 전파
├── web/                  Next.js 빌드 결과물 embed
└── auth/
    ├── sso.go            사내 SSO (LDAP/SAML/OIDC)
    └── mfa.go            MFA 강제
```

#### Admin UI (Next.js, 별도 프로젝트 또는 monorepo의 web/)
- 대시보드 (실시간 KPI)
- 세션 관리 (검색, 강제종료)
- 라우팅 룰 편집 (드래그앤드롭)
- 정책 편집 (주문 한도, 통화쌍별 활성화)
- FIX 세션 상태판
- 로그 / 감사 조회 (필터링, 다운로드)
- Rate limit 모니터링

#### Acceptance criteria
- 사내망 IP 외 접근 차단
- SSO 연동 + MFA 강제
- 모든 변경 audit log 기록 (immutable, 7년 보관 — 금융 규제)
- 설정 변경 → 데이터플레인 30초 내 전파
- UI 응답 P95 < 500ms

---

### Phase 7 — 운영 6대 기능 통합 (3주)

Phase 6의 control plane 기능들을 데이터 플레인(mci-api/push/price)에 실제 연결.

#### 7.1 Session 관리
- mci-* 서비스가 자기 세션 정보를 admin에 등록 (gRPC)
- admin → 서비스: 강제종료 명령, 통계 요청
- Redis 기반 분산 세션 저장 (cluster mode)

#### 7.2 Routing 관리
- 라우팅 룰 = "어떤 사용자/통화쌍이 어떤 백엔드로?"
- mci-api/push/price가 admin에서 룰 fetch (start) + watch (변경 시)
- 적용: 인메모리 룰 테이블, hot reload

#### 7.3 Service / Policy 관리
- Policy enforcement at boundary: mci-api에서 주문 검증 시 정책 체크
- Service registry: 각 서비스가 admin에 health 보고
- 정책 위반 시 즉시 reject + audit

#### 7.4 Monitoring & Logging
- Prometheus metrics 노출 (모든 서비스, /metrics 엔드포인트)
- Grafana 대시보드 (사전 정의 JSON)
- 구조화 로그 → Loki / ELK
- OpenTelemetry trace (REST → Internal → mymqd → 백엔드 전체 구간)
- Audit 로그 별도 저장소 (immutable, regulatory 요구)

#### 7.5 Rate Limit / Flow Control
- L1 (edge): 사용자/IP per IP 토큰 버킷
- L2 (internal): 주문 TPS 제한 (사용자별, 통화쌍별)
- L3 (broker 백프레셔): mymqd 큐 깊이 모니터링 → 임계 도달 시 incoming throttle
- 시세 conflation: 느린 구독자에게 latest만 전달

#### 7.6 Failover / Recovery
- mci-api: stateless → 다수 인스턴스 + LB
- mci-push: sticky session 또는 Redis로 세션 재분배
- mci-price: active-standby (broker 1개 의존)
- mymqd 끊김 시 자동 reconnect (Phase 1 완성)
- Edge ↔ Internal stream 끊김 시 자동 재연결

#### Acceptance criteria
- 카오스 테스트: 임의 컴포넌트 kill → 30초 내 복구
- 정책 변경 후 30초 내 모든 노드 반영
- 부하 한계 도달 시 graceful degradation (rate limit 작동, panic 없음)

---

### Phase 8 — 보안 / HA / 프로덕션 강화 (3주)

#### 8.1 보안 감사
- OWASP Top 10 / STRIDE 위협 모델링
- Pen test (외부 업체 권장)
- 취약점 스캔 (Dependabot, Trivy)
- 시크릿 관리 (Vault 또는 AWS Secrets Manager)
- 인증서 자동 갱신 (cert-manager)

#### 8.2 HA 구성
- mci-api / push / price: 다중 인스턴스 (K8s Deployment, replicas ≥ 3)
- mci-admin: active-passive (DB 단일 source of truth)
- DB: Postgres HA (Patroni 또는 RDS Multi-AZ)
- Redis: Cluster mode 또는 Sentinel
- mymqd: 운영팀 기존 HA 정책 따름

#### 8.3 모니터링 / 알람
- SLI 정의: 응답 시간, 에러율, 처리량
- SLO 합의: 예 - 99.9% 가용성, P99 응답 < 200ms
- 알람 정책 (PagerDuty 등): error budget burn rate 기반
- Runbook: 주요 incident 대응 절차

#### 8.4 부하 테스트
- k6 또는 Gatling 시나리오
- 정상 부하 (1000 RPS) + 피크 부하 (5000 RPS)
- DR 시나리오: AZ 1개 다운, broker 끊김
- 24시간 soak

#### Acceptance criteria
- Pen test 보고서 통과 (Critical/High 0건)
- 99.9% 가용성 + P99 응답 < 200ms 측정 검증
- DR 시나리오 자동 복구

---

### Phase 9 — 마이그레이션 + 운영 인계 (2주)

#### 9.1 점진 도입
- staging 환경에서 1주 운영
- 한 사업본부만 production 도입 (canary)
- 모니터링 1주 → 안정 확인 → 전체 도입
- 기존 CS 클라이언트는 병행 운영 (사라지지 않음)

#### 9.2 운영 인계
- Runbook 완성 (주요 시나리오별 대응 절차)
- On-call rotation 셋업
- 운영팀 교육 (1일 워크샵 + 1주 shadowing)
- 장애 대응 시뮬레이션 (game day)

#### 9.3 문서
- README / ARCHITECTURE / OPERATIONS / SECURITY
- API 레퍼런스 (OpenAPI)
- Admin UI 사용자 매뉴얼
- 트러블슈팅 가이드

---

## 3. 의존 관계 / 임계 경로

```
Phase 1 ───────┬──→ Phase 2 (api) ────┐
               ├──→ Phase 3 (push) ───┼──→ Phase 5 (edge) ──┐
               └──→ Phase 4 (price) ──┘                       │
                          ↑                                   │
                          (cooker patch)                      │
                                                              ▼
Phase 1 ──────────────→ Phase 6 (admin) ──→ Phase 7 (ops)──→ Phase 8 ──→ Phase 9
```

**임계 경로**: 1 → 2 (or 3, 4) → 5 → 8 → 9 ≈ 14주

**병렬화 가능 구간**:
- Phase 2 / 3 / 4 동시 진행 (별 팀이면 가속)
- Phase 6 (admin) Phase 1 끝나면 즉시 시작 가능
- Phase 7 (ops 통합) Phase 6 + 데이터플레인 어느 정도 진척 후

---

## 4. 결정 / 합의 대기 항목

| #   | 항목                       | 누구    | 영향 단계      | 우선순위 |
| --- | ------------------------ | ----- | ---------- | ---- |
| 1   | mymqd 접근 (staging)       | 운영팀   | Phase 1 검증 | P0   |
| 2   | cooker 패치 적용             | 운영팀   | Phase 4    | P1   |
| 3   | Exchange/queue 네이밍 컨벤션   | 운영팀   | Phase 4    | P1   |
| 4   | 인증 시스템 (SSO/IdP)         | 보안팀   | Phase 2, 6 | P1   |
| 5   | 인증서 발급 절차                | 보안팀   | Phase 5    | P2   |
| 6   | DMZ 방화벽 룰                | 인프라팀  | Phase 5    | P1   |
| 7   | DB 선택 (Postgres / etcd)  | 운영팀   | Phase 6    | P2   |
| 8   | 메시지 스키마 (transaction 매핑) | 비즈니스  | Phase 2    | P0   |
| 9   | Push JSON envelope 포맷    | 비즈니스  | Phase 3    | P1   |
| 10  | Tick 정규화 사양              | 비즈니스  | Phase 4    | P1   |
| 11  | SLO 합의 (가용성/지연)          | 사업/운영 | Phase 8    | P2   |
| 12  | 도입 범위 / 시점               | 사업    | Phase 9    | P2   |

---

## 5. 리스크 매트릭스

| 리스크            | 확률  | 영향  | 대응                                     |
| -------------- | --- | --- | -------------------------------------- |
| ckey echo FAIL | 낮음  | 높음  | connection pool fallback (1주 추가)       |
| cooker 패치 지연   | 중   | 높음  | mci-price를 mock pushdata로 개발 진행        |
| 인증 시스템 미정      | 중   | 중   | Phase 1~5 동안 stub auth, Phase 6 직전 통합  |
| broker 부하 한계   | 중   | 높음  | Phase 4에서 부하 테스트 → 옵션 B로 폴백 가능         |
| DMZ 방화벽 룰 지연   | 중   | 중   | local에서 DMZ 시뮬레이션 환경 구축                |
| FX 사양 합의 지연    | 높음  | 중   | API 우선 구현, transaction 추가는 incremental |
| 운영팀 학습 곡선      | 중   | 중   | Phase 9 shadowing + 충분한 문서             |
| 보안 audit 미통과   | 낮음  | 높음  | Phase 8 시작 전 self-audit                |

---

## 6. 마일스톤

| 시점 | 마일스톤 | 검증 |
|-----|---------|-----|
| W1 | Phase 1 GO/NO-GO | mymqd ckey echo PASS |
| W2 | Phase 1 완료 | libmymq-go production-ready |
| W4 | mci-api alpha | 첫 transaction 동작 |
| W6 | mci-push alpha | WebSocket 메시지 송수신 |
| W8 | mci-price alpha | 시세 1차 fan-out |
| W11 | DMZ 분리 동작 | edge ↔ internal 통합 |
| W14 | mci-admin 동작 | 정책 + 모니터링 데모 |
| W17 | 운영 6대 기능 통합 | E2E 시나리오 |
| W20 | 보안/HA 검증 | pen test + DR |
| W22 | Production 도입 | canary → 전체 |

---

## 7. 운영 인수 후 추가 가능한 기능 (Phase 10+)

- FIX 4.4 / FIXT 1.1 외부 카운터파티 직접 연결
- 다중 거래소 / LP aggregation
- HFT / 마켓메이커 핫패스 (옵션 B로 부분 전환)
- ML 기반 이상거래 탐지
- Mobile SDK
- WebRTC 기반 음성 거래 채널
- Multi-tenant (지점/사업본부 분리)

---

## 8. 단기 (다음 2주) 액션

운영팀 답변 대기 외에 우리가 진행할 수 있는 항목:

1. ~~Phase 1 잔여~~: Options.Unsolicited, 자동 재연결, 압축 디코딩
2. mci-api 스캐폴드 — 운영팀 답변 없이도 핵심 transaction 1개 prototype
3. mci-push 스캐폴드 — 같은 이유로 prototype
4. 보안 검토 시작 — secrets 관리, dependabot 셋업
5. CI 셋업 — GitHub Actions: build + test + vet + linter
6. K8s manifest 초안

가장 productive한 순서: **1 → 5 → 2 → 3** (Phase 1 단단해진 후 횡으로 확장).

---

— END of Roadmap —
