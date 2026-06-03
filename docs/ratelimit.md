# WTG Rate Limit 정책 명세

WTG 의 path-aware rate limit 룰 카탈로그 + tuning 가이드.

코드 참조: `pkg/ratelimit/ratelimit.go` (Limiter), `pkg/ratelimit/ruleset.go`
(RuleSet / glob 매칭), `internal/edge/api/ratelimit_defaults.go` (default 룰).

---

## 1. 기본 모델

token bucket (golang.org/x/time/rate) 기반 — 키 단위로 별도 버킷.

```
Rule (Pattern, Rate, Burst)
        ↓ 매칭 시
   별도 Limiter 인스턴스
        ↓ 요청마다 Allow(key)
   true → 통과
   false → 429 + Retry-After: 1
```

**Pattern** 형식:

| 형태 | 의미 |
|------|------|
| `POST /v1/tx` | method + 정확 path |
| `GET /v1/chart/*` | glob — `*` 은 `/` cross X (path.Match) |
| `/v1/admin/*` | 모든 method, glob path |
| `*` | 모든 method + 모든 path (fallback 룰) |

룰 순서대로 첫 매칭. **좁은 룰을 위에**.

---

## 2. 키 추출 (`UserOrIPKey`)

| 우선순위 | 키 |
|---------|-----|
| 1 | `X-WTG-User` 헤더 (인증된 사용자) → `user:<usid>` |
| 2 | RemoteAddr 의 IP → `ip:<addr>` |

NAT 뒤의 여러 사용자가 같은 IP 라도 인증 후엔 별도 버킷.
- 한 명 abuse 가 다른 사용자에 영향 없음
- 인증 안 된 트래픽만 IP 단일 풀

metric label `kind ∈ {user, ip}` 로 분리 카운트.

---

## 3. mci-edge-api default 룰 카탈로그

`internal/edge/api/ratelimit_defaults.go` 의 `DefaultRateLimitRules()`.

| 룰 | Rate | Burst | 이유 |
|-----|------|-------|------|
| `POST /v1/login` | 5/s | 10 | brute force 방지 — 한 user/IP 분당 300 시도 한계 |
| `POST /v1/refresh` | 20/s | 40 | 토큰 갱신은 빈번하지 X |
| `POST /v1/tx` | 50/s | 100 | 매매 — 매매 엔진 보호. 정상 거래 보다 충분 |
| `POST /v1/admin/*` | 10/s | 20 | 관리자 한 명 기준 |
| `PUT /v1/admin/*` | 10/s | 20 | 관리자 한 명 기준 |
| `DELETE /v1/admin/*` | 10/s | 20 | 관리자 한 명 기준 |
| `GET /v1/admin/*` | 50/s | 100 | 목록/조회는 빈번 가능 |
| `GET /v1/quote/*` | 200/s | 400 | 시세 조회 |
| `GET /v1/ping` | 1000/s | 2000 | health check — 거의 무제한 |
| (fallback) | `IPRatePerSec` | `IPBurst` | 매칭 안 된 path. `--ip-rate 0` 이면 통과 |

### 룰별 독립 버킷

한 사용자가 `GET /v1/quote/*` polling 으로 200/s 한도 소진해도
`POST /v1/tx` 의 버킷은 그대로 — 새 토큰 가용.

### Cardinality

label 조합: `service (1~4) × kind (2) × rule (~10) = ~80 시계열` — 안전.

---

## 4. Tuning 가이드

### 4.1 한도가 너무 빡빡 (정상 사용자가 429)

증상:
- `wtg_ratelimit_denied_total{rule="X"}` 가 갑자기 증가
- 사용자 신고 → "갑자기 거부됨"

조치:
1. Grafana 에서 해당 룰의 denied / allowed 비율 확인
2. `ratelimit_defaults.go` 의 해당 룰 Rate/Burst 증가
3. 재배포

### 4.2 한도가 너무 느슨 (공격 통과)

증상:
- `wtg_quoteid_op_total{status="denied"}` 폭증 (RBAC 잡힘) 또는
- 매매 엔진 부하 알람
- 인증 실패 로그 폭증

조치:
1. 어느 룰의 allowed rate 가 비정상인지 확인
2. Rate/Burst 낮춤 + 또는 새 strict 룰 추가 (예: `POST /v1/login` 으로 분리)
3. **즉시 차단 필요시** IP allowlist (`--allow-cidrs`) 로 임시 우회 후 룰 조정

### 4.3 cheap path polling 으로 critical path 영향

`path-aware` 도입 전엔 같은 IP 단일 한도라 polling 이 매매 거부.
**path-aware 도입 후 자동 분리됨** — 별도 조치 X.

### 4.4 신규 path 추가 시

`ratelimit_defaults.go` 의 룰 리스트에 추가 + 단위 테스트.
catch-all 룰 (`*`) 또는 fallback 으로 인해 무조건 어딘가 매칭됨 —
의도된 한도가 아닐 수 있으므로 명시적 룰 추가 권장.

---

## 5. 알람 권장

### `wtg_ratelimit_denied_total` 모니터링

```promql
# 룰별 거부율 (login brute force 의심)
sum by (rule) (rate(wtg_ratelimit_denied_total[1m]))
```

| 룰 | 임계 | 의도 |
|----|------|------|
| `POST /v1/login` | denied > 1/s | 한 user/IP 가 분당 60회 → brute force 의심 |
| `POST /v1/tx` | denied > 5/s | 매매 봇 공격 또는 한도 너무 빡빡 |
| 전체 | 비율 (denied / allowed) > 5% | 한도 일반적 부적정 |

알람 발화 시:
1. label `kind`, `rule` 로 원인 분류 (사용자 1명 vs 다수)
2. label `kind="user"` 면 해당 user audit log 확인
3. label `kind="ip"` 면 해당 IP 차단 검토 (`--allow-cidrs` 또는 firewall)

---

## 6. etcd 기반 hot reload

### 동작 흐름

```
운영자 → admin UI "Rate Limit 정책" 페이지 또는 REST
       → PUT /v1/admin/ratelimit/<service>
       → etcd wtg/ratelimit/<service>
       → mci-edge-* 의 ratelimit.EtcdWatcher 가 즉시 hot-swap
       → 새 룰셋으로 다음 요청부터 적용 (재배포 X)
```

### 운영자 액션

| UI | REST |
|----|------|
| 사이드바 → "Rate Limit 정책" | `PUT /v1/admin/ratelimit/edge-api` |
| service 선택 (edge-api / push / price / chart) | 본문은 `PolicyDoc` JSON |
| 룰 추가 / 수정 / 순서 변경 / 삭제 | version 자동 증가 |
| fallback 한도 토글 + 입력 | audit ring 에 PUT_RATELIMIT 기록 |
| "💾 저장 (etcd PUT)" | |

### PolicyDoc JSON 스키마

```json
{
  "version": 7,
  "rules": [
    {"pattern": "POST /v1/login", "rate": 5,  "burst": 10},
    {"pattern": "POST /v1/tx",    "rate": 50, "burst": 100}
  ],
  "fallback": {"rate": 100, "burst": 200}
}
```

- `version` 은 admin REST 가 자동 증가 (기존 doc.version + 1)
- `rules` 빈 배열 가능 — fallback 만 작동
- `fallback` 생략 가능 — 매칭 안 된 path 는 통과

### edge-api flag

```
mci-edge-api --etcd=10.0.0.50:2379 --etcd-ratelimit-key=wtg/ratelimit/edge-api
```

`--etcd` 비면 컴파일 default 룰 + 단일 `--ip-rate`/`--ip-burst` fallback 으로
정적 동작 (재시작해야 한도 변경).

### 운영 중 안전망

| 시나리오 | 동작 |
|----------|------|
| 운영자가 잘못된 JSON PUT | EtcdWatcher 가 거절 → 기존 룰 유지 + warn 로그 |
| 잘못된 룰 (음수 burst, 빈 pattern) | admin REST 가 400 거부 |
| etcd 끊김 | 기존 룰 유지 (마지막 hot-swap 후 상태). watch 재연결 시 동기화 |
| `DELETE /v1/admin/ratelimit/<svc>` | edge 가 컴파일 default 로 복원 |

### audit 추적

```
emitAudit(resource="ratelimit", action="PUT_RATELIMIT",
  service=edge-api, version=7, rules=9, fallback=true)
```

admin UI 의 "감사 로그" 페이지에서 모든 변경 시점 + 운영자 식별 가능.

### 정책 plugin 향후 후보

| 항목 | 현재 | 후속 |
|------|------|------|
| 룰 검증 | pattern + 양수만 | semantic (예: tx 의 한도가 너무 빡빡하면 warn) |
| diff 로그 | audit 의 count 만 | before/after 룰 diff 시각화 |
| rollback | 수동 (이전 PolicyDoc 재PUT) | 자동 N개 history 보관 |
| canary | 모든 인스턴스 동시 적용 | 일부 instance label 만 우선 적용 |

---

## 7. 응답 본문

```json
HTTP/1.1 429 Too Many Requests
Retry-After: 1
Content-Type: application/json

{
  "error": "rate_limited",
  "message": "요청 한도 초과",
  "rule": "POST /v1/tx"
}
```

`rule` 필드로 어느 룰이 발화했는지 운영자가 즉시 파악. log 도 동일 룰명.
