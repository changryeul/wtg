# WTG 단순화 가이드 — 자르거나, 덮거나

> "처음엔 단순했는데 복잡해졌다" 는 정상이다. 외환 도메인 + HA + 보안 + 관측이 누적되면 어쩔 수 없다.
> 본 문서의 전제 — **코드는 안 바꾼다**. 새 코드 = 새 사고. 대신 두 방향으로 단순화한다 :
>
> - **Part B (자르기)** — 끌 수 있는 컴포넌트를 flag 로 꺼서 운영 surface 를 줄임
> - **Part D (덮기)** — 운영 UX 의 default / 자동화 / 체크리스트로 운영자 부담을 낮춤
>
> 새 코드 변경이 필요한 단순화 (예: 5-Layer → 3-Layer 통합) 는 본 문서 범위 밖.

---

## 0. 두 갈래의 의미

| | Part B (자르기) | Part D (덮기) |
|---|---|---|
| 대상 | 시스템 구성 | 운영자 경험 |
| 효과 | 컴포넌트 / 설정 / 사고 surface 축소 | 같은 시스템도 운영 부담 ↓ |
| 위험 | 자른 부분의 기능 상실 | 0 (코드 안 바꿈) |
| 회복 | flag 다시 켜기 (≤ 5분) | 그대로 |
| 본 문서 비중 | §1~5 | §6~9 |

→ 두 갈래를 동시에 진행하는 게 정석. **자르기로 surface 줄이고, 남은 것은 덮기로 운영자 부담 ↓**.

---

# Part B — 끌 수 있는 컴포넌트 카탈로그

각 항목은 **(끄는 방법 / 잃는 것 / 운영 대안 / 회복 절차)** 4칸 표.

## 1. 컴포넌트 끄기 (서비스 단위)

### 1.1 mci-chart — 차트 서비스 통째로 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-chart` / `mci-edge-chart` 안 띄움. `cmd/mci-chart/` 도 빌드 안 함 (`make build` 는 자동 빌드하니 systemd unit 만 비활성). |
| **잃는 것** | historical 봉 차트, 라이브 SubscribeBar ws, TimescaleDB 의존 |
| **운영 대안** | 외부 TradingView 위젯 embed (브라우저에서 직접 거래소 차트). 사내는 무위 — 매매 화면에 호가창만. |
| **회복** | mci-chart 다시 띄우면 끝. `etc/sql/quote_bars.sql` 가 한 번이라도 적용됐다면 봉 데이터는 그대로 (Archiver 가 적재). |
| **추가 효과** | TimescaleDB / PostgreSQL 인프라 부담 0 (다른 서비스가 안 씀) |

### 1.2 mci-push — push 서비스 통째로 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-push` / `mci-edge-push` 안 띄움. |
| **잃는 것** | 매매 체결 알림 / 시스템 공지의 ws push, broker rep receiver fan-out |
| **운영 대안** | 이메일 / SMS / 모바일 푸시 (FCM/APNs) 또는 클라이언트의 정기 polling. 라이브 알림이 매매 차질로 직접 이어지지 않으면 OK. |
| **회복** | 다시 띄우면 즉시 동작. broker rep receiver 등록 자동. |
| **추가 효과** | mci-push 의 consistent hash ring / push-secret 회전 / mTLS 부담 0 |

### 1.3 mci-edge-* — DMZ 분리 통째로 끄기 (작은 사이트만)

| 항목 | 내용 |
|---|---|
| **끄는 방법** | mci-edge-{api,price,push,chart} 안 띄우고, 외부에 nginx / HAProxy 1대만 두고 mci-{api,price,push,chart} 로 직접 reverse proxy. JWT 검증은 nginx 의 auth_request 또는 mci-api 가 직접. |
| **잃는 것** | DMZ 격리 (외부 트래픽이 내부 네트워크 직접 도달), N6 다중 인스턴스 fan-out |
| **운영 대안** | 작은 사이트 (사내 직원 + 폐쇄형 고객만) 또는 보안 요구가 낮은 환경. 외부 인터넷 노출 시는 비추 — DMZ 격리는 보안 기본. |
| **회복** | mci-edge-* 다시 띄우고 nginx → edge 로 routing 전환. |
| **추가 효과** | 바이너리 4개 ↓, 인스턴스 8대 → 2~4대 |

## 2. 기능 끄기 (같은 서비스 안에서)

### 2.1 swap-lock — FX swap 거래 미지원

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-price --enable-swap-lock=false` (default false). |
| **잃는 것** | `POST /v1/quote/swap/lock` endpoint 미등록, `ValidateSwap`/`ConsumeSwap` gRPC 미동작 |
| **운영 대안** | swap 을 spot 2건으로 분리 발행 (운영상 분쟁 위험 ↑ — 추천 X) 또는 swap 거래 자체를 안 함. |
| **회복** | `--enable-swap-lock` + Registry 가 SwapIndex 구현 (MemoryRegistry / RedisRegistry 둘 다 구현) → endpoint 자동 등록 |
| **확인** | admin UI 의 `🔁 FX swap 잠금 통계` 페이지가 "endpoint 미응답" 안내로 부드럽게 표시 |

### 2.2 5-Layer Customer quote — 5L 마진 적용 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-edge-price --customer-stream=false`. Profile-only quote (HQ+Site 2-Layer) 만 흐름. |
| **잃는 것** | customer_id 별 차등 마진 (Customer/Window layer). RegisterCustomer / SubscribeCustomerQuote 미동작 |
| **운영 대안** | Profile (Tier) 단위 차등만으로 운영. 개별 우대가 필요한 고객은 별도 Tier 신설로 대응. |
| **회복** | `--customer-stream` 다시 켬. |
| **추가 효과** | mci-price 의 PricingConsumer 계산 부담 ↓ (Profile 별 1회만 계산) |

### 2.3 Profile-routed quote — Profile customer quote 까지 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-edge-price --quote-stream=false`. raw BEST tick 만 흐름. |
| **잃는 것** | 마진 적용된 호가 — 모든 사용자가 raw 호가 직접 받음 |
| **운영 대안** | 마진을 매매 직전 (매매 AP) 에서 적용. 외환 운영상 거의 드문 케이스 — 보통은 사용자 화면에 마진 호가 표시 필요. |
| **회복** | `--quote-stream` 다시 켬. |
| **언제 의미** | broker / DB 마이그레이션 시 raw tick 만 확인하고 싶을 때, 매매 비활성 정비창 |

### 2.4 mci-price gRPC — gRPC 서버 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-price --grpc=""` (빈값). default 가 비활성. |
| **잃는 것** | SubscribeQuote / SubscribeBar / SubscribeCustomerQuote / RegisterCustomer / PublishTick / QuoteValidationService — 즉 mci-edge-price + mci-chart + 매매 AP 의 검증 RPC 까지 전부 동작 안 함. |
| **운영 대안** | broker 만으로 시세 fan-out (legacy path). 매매 AP 의 quote 검증은 mymq wire 로 대체 필요. |
| **회복** | `--grpc :50051` 켬. |
| **언제 의미** | Internal 전용 단순 환경 (mci-edge-* 없이 broker fan-out 만), 또는 매매 AP 가 quote_id 검증 안 함 |

### 2.5 Archiver — 봉 영속 끄기 (라이브 차트는 살려둠)

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-price --chart-dsn=""` (빈값). |
| **잃는 것** | TimescaleDB.quote_bars INSERT 0건 — historical 차트 빈 화면 |
| **운영 대안** | mci-chart 의 라이브 SubscribeBar 만 활성 — 사용자가 화면 열어있는 동안만 봉 누적. 영속은 외부 시계열 DB (InfluxDB / Timestream) 로 옮기거나 안 함. |
| **회복** | `--chart-dsn postgres://...` 다시 줌. |
| **언제 의미** | TimescaleDB 운영 부담 회피, 차트는 라이브만 충분한 경우 |

### 2.6 BestConsumer — best 호가 합성 끄기

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-price --best=false`. |
| **잃는 것** | 다중 feed 합성 BEST 산정 — 각 feed 의 호가가 그대로 fan-out |
| **운영 대안** | feed 가 하나뿐 (예: dev tick 또는 단일 거래소) 이면 BEST 가 무의미 — 끄는 게 정상. |
| **회복** | `--best` 켬 (default true). |
| **언제 의미** | 단일 feed 환경 (PoC / dev / 단일 거래소 연동) |

### 2.7 broker 통째로 끄기 (PoC / dev / 시연용)

| 항목 | 내용 |
|---|---|
| **끄는 방법** | `mci-{admin,price,push} --no-broker`. broker 연결 시도 자체 skip. |
| **잃는 것** | 모든 매매 (broker 통신) — 시세 / 모니터링 / admin UI 만 동작 |
| **운영 대안** | 시연 / 회귀 테스트 / UI 단독 검증 시. 운영 절대 X. |
| **회복** | `--no-broker` 빼고 재시작. |
| **추가 효과** | 운영에서 절대 안 쓰지만 dev 환경에서 가장 자주 쓰는 단순화 |

## 3. 인프라 끄기

### 3.1 etcd 클러스터 → 단일 etcd

| 항목 | 내용 |
|---|---|
| **방법** | etcd 1 node 만 띄움. 모든 WTG 서비스가 그 1개 endpoint 사용. |
| **잃는 것** | 카탈로그 / 정책 store 의 HA. etcd 죽으면 모든 watch 끊김 → 정책 변경 불가 (기존 캐시는 유지) |
| **운영 대안** | DevMode 의 embedded etcd 그대로 운영 (`mci-admin --dev` 가 자동 띄움). 단일 사이트 + 단일 admin 이면 충분. |
| **회복** | 추후 3-node cluster 로 확장 (etcdctl member add). |

### 3.2 etcd → 정적 파일

| 항목 | 내용 |
|---|---|
| **방법** | etcd 안 띄우고 mci-price 가 `--symbols etc/symbols.json --pricing etc/pricing.json --profiles etc/profiles.json` flag 만 사용. |
| **잃는 것** | hot reload — 정책 변경 시 mci-price 재시작 필요 (수십 초 다운타임) |
| **운영 대안** | 정책 변경 빈도가 분기/월 단위면 재시작 감수. 시세 service 가 분당 1회 변경되는 경우면 비추. |
| **회복** | etcd 도입 + `EtcdSymbolWatcher` / `EtcdTableWatcher` flag 활성. |

### 3.3 Redis Sentinel → 단일 Redis

| 항목 | 내용 |
|---|---|
| **방법** | Redis 1 node + Sentinel 없음. WTG 서비스의 `--quoteid-redis` / `--auth-redis` 가 단일 `host:port`. |
| **잃는 것** | 세션 / quoteid Registry 의 HA. Redis 죽으면 모든 활성 사용자 401 + 모든 quote_id Put 실패 |
| **운영 대안** | Redis AOF + 빠른 재시작 (~수초). 단일 사이트 + 활성 사용자 수십명 이면 다운타임 감수. |
| **회복** | Sentinel 3-node cluster 도입. |

### 3.4 관측 stack 5개 → 1개 통합

| 항목 | 내용 |
|---|---|
| **방법** | Grafana Cloud / Datadog / NewRelic 같은 통합 SaaS 1개 가입. 자체 호스팅 Prometheus + Grafana + Loki + Tempo + OTel collector 모두 폐기. |
| **잃는 것** | 자체 통제 (데이터 보유 위치, 보안). 비용은 SaaS 가 보통 더 비쌈. |
| **운영 대안** | 작은 팀 (운영 인력 2~3명) 이면 운영 부담 회피가 비용보다 큼. |
| **회복** | 자체 호스팅 stack 도입. |

### 3.5 TimescaleDB → 일반 PostgreSQL

| 항목 | 내용 |
|---|---|
| **방법** | `etc/sql/quote_bars.sql` 의 `create_hypertable` / `add_compression_policy` 줄 제거 후 적용. mci-chart 의 SELECT 는 그대로 동작. |
| **잃는 것** | 자동 압축 / 자동 retention / chunk pruning. 봉 데이터 누적 시 성능 ↓ (수개월 후) |
| **운영 대안** | 정기적 수동 cleanup (`DELETE FROM quote_bars WHERE opened_at < now() - interval '30 days'`) 또는 운영 정책 retention 짧게. |
| **회복** | TimescaleDB extension 설치 + `SELECT create_hypertable('quote_bars', 'opened_at', migrate_data => true);` |

## 4. 도메인 / 카탈로그 단순화 (코드 변경 없이 정책만)

### 4.1 MOB 채널 안 씀

- `etc/profiles.json` 의 `MOB.*` 행 제거
- admin UI 의 `🧩 프로파일` 페이지에서도 삭제
- mci-edge-* 의 모든 채널 라우팅이 자동으로 MOB 무시 — 코드 변경 0

### 4.2 GOLD Tier 안 씀

- 동일하게 Profile 카탈로그에서 `GOLD` 행 제거
- `pkg/session/types.go` 의 enum 은 그대로 둬도 됨 — 사용 안 하면 그만
- PricingTable 의 `hq_margin` 에서 GOLD 행 제거하면 1/3 감소

### 4.3 Window layer 안 씀

- `etc/pricing.json` 의 `time_windows: []` 빈 배열
- PricingTable.Apply 가 Window 단계 자동 skip

### 4.4 Customer layer 안 씀

- `etc/pricing.json` 의 `customer_margin: []` 빈 배열
- 모든 사용자가 Tier 차등만 받음 (4-Layer)

### 4.5 swap 안 함 (§2.1 과 묶음)

- `etc/pricing.json` 의 `swap_point: []` 빈 배열
- 동시에 `--enable-swap-lock=false`
- 운영자 화면에서 swap 거래 메뉴 자체 숨김

## 5. 두 트랙 push 의 트랙 A 종료 (마이그레이션 단순화)

마이그레이션 중간 상태로 두 트랙이 다 살아있는 게 운영 복잡도 큰 원인. **트랙 B (HTTP push) 가 운영 안정화되면 트랙 A 종료**.

| 단계 | 동작 |
|---|---|
| 1 | 트랙 B 운영 30일 + push 카운터 비교 (트랙 A 와 트랙 B 의 fan-out 수 일치 검증) |
| 2 | 매매 AP 코드의 `publish(broker)` 호출을 `HTTP POST mci-push` 로 교체 |
| 3 | 트랙 B 단독 운영 7일 — 안정성 검증 |
| 4 | mci-push `--qf-unsol-rep=false` 또는 broker rep receiver 등록 제거 — **트랙 A 종료** |
| 5 | broker publish 부담 ↓, mci-push 의 LogonID fan-out 코드 단순화 (HTTP path 만) |

→ 자세한 절차 `docs/phase-2.7-rollout.md`.

## 6. Part B 종합 — 끄기 우선순위

가장 효과 크고 위험 작은 순 :

| 우선순위 | 항목 | surface 감소 | 위험 |
|---|---|---|---|
| 1 | MOB / GOLD / Window / Customer 미사용 (§4) | profiles.json / pricing.json 만 편집 | 0 (정책만) |
| 2 | swap-lock 끄기 (§2.1) | swap endpoint + 페이지 1개 | 0 (swap 거래 안 하면 OK) |
| 3 | mci-chart 끄기 (§1.1) | 2 바이너리 (chart + edge-chart) + TimescaleDB | 외부 차트로 대체 |
| 4 | 관측 통합 SaaS (§3.4) | 5 컴포넌트 → 1 SaaS | 비용 / 데이터 위치 |
| 5 | mci-push 끄기 (§1.2) | 2 바이너리 | 알림 채널 대체 |
| 6 | 두 트랙 push → A 종료 (§5) | mci-push 의 broker 의존 | 마이그레이션 7일 |
| 7 | DMZ 분리 끄기 (§1.3) | 4 바이너리 | 보안 ↓ |
| 8 | Redis Sentinel → 단일 (§3.3) | 5 → 1 인스턴스 | 세션 HA 손실 |
| 9 | etcd → 정적 파일 (§3.2) | etcd 자체 제거 | hot reload 손실 |

→ **상위 3개만으로도 운영 surface 가 절반** 줄어든다.

---

# Part D — 덮기 (운영 UX 단순화)

코드를 안 바꾸고 운영자 부담을 줄이는 방법. 좋은 default + 자동화 + 체크리스트.

## 7. 좋은 default 운영 패턴

### 7.1 systemd unit 의 표준 정책

```ini
[Service]
Restart=on-failure
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
TimeoutStopSec=30
```

→ 죽으면 자동 재시작. 5초 backoff. 60초 안 5회 실패하면 systemd 가 disable (무한 재시작 폭주 방지 — supervisor goroutine 외 추가 보호).

### 7.2 logrotate 자동화

```
/var/log/wtg/*.log {
    daily
    rotate 14
    compress
    missingok
    notifempty
    copytruncate
}
```

→ 운영자 손 댈 일 없음.

### 7.3 환경별 flag 하나만 다름

같은 systemd unit + `EnvironmentFile=/etc/wtg/<env>.env` 로 환경별 차이를 환경변수 1개 파일에 몰아둠.

```
# /etc/wtg/prod.env
WTG_ENV=prod
WTG_ETCD=https://etcd-sl-1:2379,...
WTG_REDIS=redis-sl-1:26379,...
WTG_BROKER=ap-sl-1:11217,ap-sl-2:11217
```

운영자는 unit 안 본다 — env 파일만 본다.

### 7.4 default flag 값을 보수적으로

| flag | dev default | 운영 권장 |
|---|---|---|
| `--quoteid-validity` | 500ms | 500ms (그대로) |
| `--quoteid-reg-timeout` | 200ms | 500ms (Redis Sentinel failover 여유) |
| `--best-staleness` | 30s | 10s (오래된 호가 빠른 제거) |
| `--ws-queue` (mci-edge-price) | 256 | **1024** ← 운영 권장 (backpressure 회피) |
| `--batch-max` (forwarder) | 14 | 14 (broker 안 바뀌면 그대로) |
| `--push-secret-rotation-grace` | — | 24h (secret 회전 시 양쪽 secret 동시 유효 기간) |

→ 운영 배포 시 위 값들 만 override. 나머지는 dev default 그대로.

## 8. admin UI 의 운영 친화 default

admin UI 의 다음 default 가 운영자 부담을 자동으로 낮춤 — 이미 들어가 있는 기능.

### 8.1 자동 갱신 (페이지별)

| 페이지 | 갱신 주기 |
|---|---|
| 대시보드 | SSE (이벤트 push) |
| 시세 통계 | 5s |
| FX swap 잠금 통계 | 2s |
| Backpressure 이력 | 2s |
| 연결 (ws) | 2s |
| 운영 모니터링 | 페이지 진입 시 + 수동 refresh |

→ 운영자는 페이지 열어두면 자동으로 최신 상태. 새로고침 누를 일 없음.

### 8.2 fail-safe 표시

- Prometheus 미구성 → `#mon-banner` 자동 표시 (red banner)
- swap-lock endpoint 미응답 → 회색 안내 + 카드는 0
- broker 끊김 → 대시보드의 broker 카드 빨강 + 매매 페이지 disabled

→ 운영자가 "왜 이게 안 나오지?" 추측할 필요 없음 — UI 가 직접 안내.

### 8.3 hot reload 안내

PricingTable / 라우팅 룰 / 정책 변경 시 :
- 저장 즉시 etcd write → mci-price / mci-api 가 watch 로 즉시 반영
- admin UI 의 "변경됨 — 1초 안에 운영 반영" toast 메시지

→ 변경 → 검증 사이클이 짧음. 운영자 확신 ↑.

## 9. 운영자 루틴 (코드 변경 없이 부담 최소화)

### 9.1 매일 5분 (출근 직후)

1. admin UI `📊 대시보드` → broker / 시세 rate / subscribers / connections 정상 범위인지
2. `📊 운영 모니터링` → 5xx / broker disconnects / ratelimit denied 모두 0 인지
3. `📜 감사 로그` → 어제 밤 사이 변경 (kill switch / pricing) 없었는지

→ 평시 5분. 어느 하나라도 어긋나면 그 페이지의 디테일로.

### 9.2 매주 30분 (주 1회)

1. `📈 매매 통계 (alias × tier)` → 주간 트래픽 분포 + 평균 latency
2. `🔁 FX swap 잠금 통계` → 부분실패 0 유지 / revoke_fail 0
3. `🔍 Customer 검색` → 활동 적은 VIP 확인 (영업 신호)
4. PricingTable 의 `swap_point` 갱신 (운영 시장 결정에 따라)

### 9.3 분기 1회 (정책 검토)

1. 마진 정책 검토 — `🪄 마진 변경 미리보기` 로 영향 시뮬레이션
2. Profile / 사용자 카탈로그 정리 (비활성 customer 정리)
3. retention 점검 (`quote_bars` 의 압축 / 삭제 정책 동작 확인)

### 9.4 사고 시 3단계 (Run 표 1장)

```
1. 즉시 거리 두기 :
   admin UI → 🛡 정책 엔진 → Kill switch ON (전체 또는 채널별)

2. 정보 수집 :
   - 📊 운영 모니터링 → 어디가 spike 인지 카드 확인
   - 📜 감사 로그 → 사고 시작 시각 변경 이력
   - 📜 매매 감사 → 마지막 transaction 들
   - ⚠️ Backpressure 이력 → 부하 spike 시각

3. 회복 :
   원인 파악 후 정책 변경 / 서비스 재시작
   Kill switch OFF
   사용자 안내 (메인 페이지 banner)
   사후 분석 → /docs/postmortem-YYYY-MM-DD.md
```

→ 사고 시 매번 같은 순서 — 운영자가 "지금 무엇 해야 하지?" 고민 0.

## 10. 운영 자동화 후보 (코드 변경 0)

| 항목 | 도구 | 기능 |
|---|---|---|
| 야간 자동 backup | cron + `etcdctl snapshot save` + `pg_dump` | 매일 03:00 |
| Grafana alert → Slack | Alertmanager webhook | 즉시 알림 |
| 정기 health snapshot | cron + `/tmp/wtg-dev-status.sh` 응용 → Slack | 매시간 요약 |
| broker reconnect 카운터 비정상 spike | Prometheus alert | 5분 안에 운영자 호출 |
| Redis master failover 알림 | Sentinel event hook | 즉시 |
| TimescaleDB chunk 압축 누락 | cron + `SELECT show_chunks(...)` 점검 | 매일 |
| etcd snapshot retention 정리 | cron 7일 후 삭제 | 매주 |

→ 운영자 직접 손 댈 일 0. 알림이 운영자에게 자동으로 옴.

---

# Part B + D 통합 — 시작 가이드

## 11. 단계별 단순화 로드맵 (4주)

### Week 1 — 자르기 (위험 0)

- [ ] `etc/profiles.json` 에서 MOB 행 제거 (안 쓰면)
- [ ] `etc/pricing.json` 의 `time_windows: []`, `customer_margin: []` 빈 배열 (안 쓰면)
- [ ] `mci-price --enable-swap-lock=false` (swap 거래 안 하면)
- [ ] admin UI 검증 — 위 변경이 정상 적용되는지

### Week 2 — 덮기 (UX 자동화)

- [ ] systemd unit 표준화 (§7.1)
- [ ] logrotate 적용 (§7.2)
- [ ] 환경별 env 파일 분리 (§7.3)
- [ ] 운영 default flag override (§7.4)

### Week 3 — 운영 SOP 정착

- [ ] 매일 5분 / 매주 30분 루틴 운영자 공유 (§9.1, 9.2)
- [ ] 사고 시 3단계 Run 표 인쇄해서 모니터 옆에 (§9.4)
- [ ] Slack / PagerDuty alert 연동 (§10)
- [ ] 야간 자동 backup cron 등록 (§10)

### Week 4 — 큰 자르기 (운영 검증 후)

- [ ] mci-chart 통째 끄기 (외부 위젯 대체) — §1.1
- [ ] 관측 통합 SaaS 도입 — §3.4 (선택)
- [ ] 두 트랙 push 중 트랙 A 단계적 종료 — §5

## 12. 단순화 시뮬레이션 — Before / After

| 항목 | Before (풀스택) | After (Part B 적용) |
|---|---|---|
| 바이너리 수 | 10 (mci-* + edge-* + forwarder) | **6** (chart/edge-chart/push/edge-push 제거) |
| Profile 카탈로그 | 10 (3×2×2 + 일부) | **6** (WEB/CS × HQ/BRANCH × VIP/STD) |
| PricingTable layer | 5 | **3** (HQ + Site + Swap) |
| 인프라 컴포넌트 | etcd cluster + Redis Sentinel + TimescaleDB + Prometheus + Grafana + Alertmanager + OTel + Tempo + Loki = 9 | **3** (etcd 1 node + Redis 1 node + Grafana Cloud) |
| 관측 endpoint | wtg_* 메트릭 100+ | 그대로 (SaaS 가 자동 처리) |
| 운영자 1일 부담 | 페이지 10+ 모니터링 | **3 페이지 5분** |
| 사고 RTO | 30s ~ 5분 | 30s ~ 5분 (같음) |
| 사고 RPO | 5초 | 5초 ~ 1분 (단일 Redis 영향) |

→ **운영 surface 절반, 일일 부담 1/3, 사고 회복 시간은 그대로**.

## 13. 안 자르는 게 좋은 것 (보호)

다음은 자르면 운영 사고 위험 ↑↑ — 자르지 말 것 :

| 항목 | 이유 |
|---|---|
| broker reconnect supervisor | 끄면 broker 끊김 시 영원 복구 안 됨 |
| ckey echo (매매 AP 측) | Phase 1 GO/NO-GO. 빠지면 모든 RPC timeout |
| cookie_t passthrough | 매매 권한 — 빼면 모든 매매 거부 |
| navi 자동채움 | 빼면 broker "no navigation" reject |
| Redis Sentinel quorum | quorum 깨지면 master 못 promote |
| etcd quorum (단일 etcd 라도) | etcd 죽으면 정책 변경 불가 |
| audit 로그 ring | 사후 분석의 마지막 보루 |
| JWT exp 검증 | 보안 — 끄면 영원 세션 |
| TLS (운영) | 외부 트래픽 평문 노출 |

→ 본 9개는 **시스템 본질** — 자르면 WTG 가 아님.

---

## 14. 참고 문서

- `docs/operations/admin-ui-manual.md` — 운영 매뉴얼 37 페이지
- `docs/operations/deployment-software.md` — 배포 소프트웨어 명세
- `docs/operations/deployment-scenario-ha-channel.md` — 단일 사이트 시나리오 + 5 단계 멘탈모델
- `docs/operations/deployment-scenario-multi-site.md` — 다중 사이트 시나리오
- `docs/directory-structure.md` — 소스 레이아웃 + 설정 파일
- `docs/phase-2.7-rollout.md` — 두 트랙 push 의 트랙 B 안정화 절차
- `docs/operations.md` — 서비스 flag/env 카탈로그
