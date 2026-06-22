# WTG 시현 시나리오 (로컬 환경)

> macOS 로컬 dev 환경에서 WTG 의 핵심 기능을 시연하기 위한 단계별 시나리오.
> **시현 도중에 막힘이 없도록** 각 단계의 명령 / 화면 / 예상 결과 / 함정 대응을 미리 박아둔다.

---

## 0. 시현 개요

### 0.1 청중 / 목표

본 시나리오는 다음 청중 중 어느 한 그룹을 가정 (택 1) :

| 청중 | 시현 시간 | 강조 포인트 |
|---|---|---|
| 경영진 / 사업 | 20~30분 | 비즈니스 가치 — 마진 차등 / 사고 대응 / 단순화 효과 |
| 운영팀 / SRE | 45~60분 | 운영 화면 / 사고 시뮬레이션 / 자동화 |
| 매매 AP / C 엔진 팀 | 45~60분 | wire 호환 / cside C SDK / 검증 RPC |
| 신규 개발자 | 60~90분 | 전체 그림 + 코드 구조 + DevMode |

기본 시나리오는 **운영팀 (60분)** 기준. 청중별 단축/확장 안내는 §11.

### 0.2 사전 가정

- macOS + Apple Silicon 또는 Intel
- 이미 빌드된 `build/bin/*` 존재 — 없으면 `make build` 10분
- brew + Python3 + Prometheus + watch 설치 완료
- dev 환경에서 모든 서비스가 한 번 이상 정상 동작한 적 있음

### 0.3 청중에게 보여주지 말 것

- 시현 중 코드 편집기 화면 (집중 분산)
- 본 README 또는 가이드 문서 (시현이 아니라 강의가 됨)
- 1초 이상 검은 터미널 (시현 무게 떨어짐 — 미리 명령 준비)
- broker 끊김의 진짜 에러 (시뮬레이션 외에는 mock 으로 우회)

---

## 1. 시현 30분 전 — 사전 점검

### 1.1 dev 스택 일괄 부팅

```bash
cd ~/mywork/wtg
./scripts/wtg-stack-up.sh --with-prom
```

→ mci-price / mci-edge-price / mci-admin / Prometheus + dev tickloop 자동 시작.

### 1.2 상태 확인

```bash
./scripts/wtg-status.sh
```

기대 — 모든 줄 ● UP 녹색, HTTP 헬스체크 200 모두 :

```
mci-admin              ● UP
mci-price              ● UP
mci-edge-price         ● UP
prometheus             ● UP
wtg-dev-tickloop       ● UP
...
시세 카운터 received > 0 (tickloop 가 흐름)
```

❌ 한 줄이라도 DOWN 이면 §10 트러블슈팅으로.

### 1.3 시현용 swap-lock 활성화 (선택)

시현에서 swap-lock 데모를 하려면 mci-price 를 `--enable-swap-lock` 으로 재시작 :

```bash
# 기존 mci-price 종료
pkill -f "build/bin/mci-price"; sleep 1

# swap-lock 활성으로 재시작
./build/bin/mci-price \
    --dev --no-broker --listen :8082 --grpc :50051 \
    --enable-swap-lock \
    --symbols etc/symbols.json \
    --pricing etc/pricing.json \
    --profiles etc/profiles.json \
    > logs/mci-price.log 2>&1 &

# 5초 후 endpoint 확인
sleep 5
curl -s http://127.0.0.1:8082/v1/quote/swap/stats
```

기대 — `{"requests":0,"successes":0,...}` (404 가 아니면 OK).

### 1.4 시현 노트북 준비

- 브라우저 탭 미리 열어두기 :
  - `http://127.0.0.1:9090/` — admin UI (DevMode 로그인 화면)
  - `http://127.0.0.1:9095/targets` — Prometheus targets
  - `http://127.0.0.1:9095/graph` — Prometheus PromQL
- 터미널 2개 분할 :
  - 좌 : `watch -tcn 2 ./scripts/wtg-status.sh`
  - 우 : 명령 실행용

### 1.5 시현 직전 admin UI 로그인

```
브라우저  →  http://127.0.0.1:9090/
        →  "개발 모드로 진입"
        →  ID : demo  (또는 본인 이름)
        →  "ID 만으로 입장 (DevMode)"
```

→ 대시보드 화면에 도착.

---

## 2. 시현 시나리오 — 60분 풀 코스

### Stage 1 : 시스템 소개 (5분)

#### 2.1 청중 인사 + 한 장으로 본 WTG

말로 :
> WTG (Winway Trading Gateway) 는 기존 C 매매 엔진 위에 web 기반 게이트웨이를 얹어
> 웹/모바일/영업점 단말을 한 단일 인프라로 통합한 시스템입니다.

`deployment-scenario-ha-channel.md` 의 §2.2 토폴로지 그림을 한 장 띄움 (slide 또는 마크다운 뷰어).

핵심 메시지 :
- **DMZ ↔ Internal 분리** : 보안 격리
- **C 엔진 무수정** : 검증된 매매 로직 그대로
- **단순한 API 표면** : 사용자는 `/v1/tx` 한 endpoint, 내부에서 broker/AP/매칭엔진으로 라우팅

### Stage 2 : 운영 대시보드 투어 (10분)

#### 2.2 admin UI 대시보드

브라우저 → `📊 대시보드`.

보여줄 카드 :
- broker 상태 — DevMode 에선 `disconnected` (`--no-broker` 정상 — 청중에게 설명)
- 시세 rate — 약 30 tick/s
- subscribers — 3 (tick / quote / customer)
- connections — 0 (아직 ws 연결 안 함)

#### 2.3 운영 모니터링

브라우저 → `📊 운영 모니터링`.

보여줄 :
- 8 카드 (HTTP rate / 5xx / RateLimit / Broker / QuoteID)
- 모두 정상 (0 또는 평소 값)
- sparkline 살아있음 (Prometheus scrape 정상)

말로 :
> 운영자가 출근 후 5분 안에 어제 밤 사이 사고가 있었는지 확인하는 첫 화면입니다.
> 한 카드라도 spike 가 보이면 상세 페이지로.

#### 2.4 시세 통계

브라우저 → `📊 시세 통계 (mci-price)`.

보여줄 :
- received / matched / dropped 카운터 (실시간 갱신)
- conflation 효과 (Updates ≫ Symbols)
- per-pair best 호가 표 (6 pair)

말로 :
> dev tick 이 6 통화쌍 × 5 tick/s 로 흐르고 있습니다. 운영에선 UDP FIX 4.4 가 들어와요.

### Stage 3 : 라이브 시세 + 마진 (10분)

#### 2.5 라이브 ws 시세

브라우저 → `💱 시세 (라이브 ws)`.

```
- Profile : WEB.BRANCH.VIP (default)
- ▶ 연결 클릭
```

기대 — 상태 `● live`, 호가창 6 pair 가 200ms 마다 갱신.

청중에게 강조 :
- **BEST** (raw 호가) vs **Profile customer quote** (마진 적용) vs **5L customer quote** 세 줄
- spread 가 BEST < Profile < 5L 로 점점 넓어짐
- 통계 카드 4종 (total / quotes / quotes-5l / trades) 1초마다 ~87 누적

#### 2.6 마진 계산기 — 5-Layer 분해

브라우저 → `🔬 마진 계산기 (5-Layer)`.

입력 :
```
pair     : USD/KRW
tenor    : SPOT
profile  : WEB.BRANCH.VIP
customer : demo
raw bid  : (비움 — 현재 BEST 사용)
```

[▶ 계산] 클릭.

기대 — 5 단계 카드가 순서대로 나옴 :
1. Inputs (echo)
2. Active TimeWindows (단순화 v3 라 빈 배열 — "Window layer off" 안내)
3. Swap (SPOT 이라 변화 없음)
4. HQ (±0.02 적용 — VIP 라 좁음)
5. Site (WEB.HQ ±0.01 — 본점이라 추가 좁음)
6. Customer (단순화 v3 라 빈 배열 — "Customer layer off" 안내)
7. Final 산식

말로 :
> 사용자에게 보이는 호가 1380.40 / 1381.30 이 이 5 단계를 거쳐 나왔습니다.
> 운영자가 "왜 이 사용자 호가가 이 값이지" 추적할 때 본 페이지로 진단합니다.

### Stage 4 : QuoteID + swap-lock (10분)

#### 2.7 swap-lock 호출 시연

터미널 (시현용) :
```bash
# tick 한 번 주입 (가격 살아있음 보장)
curl -s -X POST http://127.0.0.1:8082/v1/dev/tick \
    -H 'Content-Type: application/json' \
    -d '{"symbol":"USDKRW","bid":1380.50,"ask":1381.20}'

# swap_lock 호출 — USDKRW SPOT/1M, WEB.BRANCH.VIP, customer=demo
curl -s -X POST http://127.0.0.1:8082/v1/quote/swap/lock \
    -H 'Content-Type: application/json' \
    -d '{"pair":"USDKRW","near":{"tenor":"SPOT"},"far":{"tenor":"1M"},
         "profile":"WEB.BRANCH.VIP","customer_id":"demo",
         "side":"buy_sell","amount":1000000}' | python3 -m json.tool
```

기대 응답 :
```json
{
  "swap_id": "SW-dev-mqbu...",
  "pair": "USDKRW",
  "profile": "WEB.BRANCH.VIP",
  "near": { "quote_id": "...", "tenor": "SPOT", "bid": 1380.40, "ask": 1381.30 },
  "far":  { "quote_id": "...", "tenor": "1M",   "bid": 1381.27, "ask": 1382.18 },
  "swap_diff": { "bid_diff": 0.87, "ask_diff": 0.88 },
  ...
}
```

브라우저 → `🔁 FX swap 잠금 통계` 페이지 새로고침.

기대 — `requests: 1, successes: 1, fail_*: 0`, 모든 카드 emerald (정상 일치).

말로 :
> swap 거래는 near (지금) + far (1개월 후) 두 leg 를 동시에 잠궈야 합니다.
> 어느 한 leg 만 잠기면 분쟁의 원인이 됩니다.
> 본 시스템은 두 leg 의 호가 snapshot 을 atomic 하게 묶어 swap_id 한 개로 발급합니다.

#### 2.8 QuoteID 조회

위 응답의 `near.quote_id` 복사. 브라우저 → `🔍 QuoteID 조회`.

```
입력 : <복사한 quote_id>
[▶ 조회] 클릭
```

기대 — Record 상세 (status / 호가 / Profile / customer / issued / valid_until / issuer).

말로 :
> 사용자가 매매 도중 "호가가 다르다" 보고하면, quote_id 로 그 시점의 정확한
> 호가 snapshot 을 즉시 추적합니다. 분쟁 해결의 결정적 단서입니다.

### Stage 5 : 사고 시뮬레이션 (10분)

#### 2.9 Backpressure 발생 시연

dev 환경에서 load-gen 으로 부하 spike → mci-edge-price 의 ws queue 80% 도달 → WARN 누적.

터미널 :
```bash
# 별도 ws 연결 — 부하 받는 클라이언트 (브라우저 1개 + curl 1개 = 2개)
# 이미 라이브 ws 페이지에서 1개 연결 중. load-gen 으로 부하 spike :
./build/bin/load-gen \
    --feeds SMB:30044 \
    --pairs USDKRW,EURKRW,JPYKRW,GBPKRW,EURUSD,USDJPY \
    --rate 200 --duration 30s --stats "" --best "" &
```

→ 6 pair × 200 tick/s × 3 stream (BEST + Profile + 5L) ≈ 3,600 envelope/s. ws queue_cap=256 초과.

30 초 동안 브라우저 → `⚠️ Backpressure 이력`.

기대 — 5~10초 안에 WARN 행이 나타남 :
```
ts            service          kind        sub_id  customer  profile          depth/cap
14:32:25.784  mci-edge-price   ws-client   N       demo      WEB.BRANCH.VIP   205/256
```

말로 :
> queue 가 80% 도달하면 자동으로 WARN 이 누적되고, 100% 도달 시 클라이언트가
> close 됩니다. 운영자가 부하 한계를 시각적으로 추적합니다.

#### 2.10 자동 복구 시연

load-gen 종료 (30초 후 자동) → backpressure 해제 → 라이브 ws 페이지가 다시 정상 호가창.

```bash
# load-gen 자동 종료 대기 또는 강제
pkill -f load-gen 2>/dev/null
```

브라우저 → `💱 시세 (라이브 ws)` 페이지.

기대 — ws 가 close 되었으면 ▶ 연결 다시 클릭. 안정적으로 흐름 재개.

말로 :
> 시스템은 자체 보호 (backpressure close + 자동 재연결) 로 사고를 짧게 끝냅니다.
> 운영자가 손을 안 대도 회복합니다.

### Stage 6 : 단순화 v3 정책 시연 (5분)

#### 2.11 단순화 효과 보여주기

브라우저 → `🧩 프로파일`.
기대 — **7 Profile** (WEB×5 + CS×2, MOB 없음).

브라우저 → `💰 마진 테이블` → `미리보기` 탭.
기대 — `version 3`, `time_windows = 0`, `customer_margin = 0`.

마진 테이블의 다른 탭 :
- HQ : 7 행
- Site : 5 행 (MOB 제거)
- Customer : **빈 탭** (단순화)
- Window : **빈 탭** (단순화)
- Swap : 4 행

말로 :
> 처음엔 5-Layer 마진 + MOB 채널 + swap 거래까지 모든 기능을 켜고 있었지만,
> 실제 운영에서 **꼭 필요한 것만** 켜는 게 운영 사고 대응에 결정적입니다.
> 본 시스템은 운영자가 정책 변경 한 번으로 (재배포 없이) 5-Layer 를 3-Layer 로
> 축소할 수 있습니다.

본 시현 시점의 정책 :
- Channel : WEB / CS (MOB off)
- Tier : VIP / GOLD / STD
- 마진 layer : HQ + Site + Swap (Window / Customer off)
- swap 거래 : on / off 토글 가능 (시현 시작 시 결정)

### Stage 7 : 운영 자동화 (5분)

#### 2.12 운영 SOP 한 장 보여주기

`operations-routine.md` 를 브라우저 또는 마크다운 뷰어로 띄움. 핵심 :

- **매일 5분** : 3 페이지 (대시보드 / 운영 모니터링 / 감사 로그)
- **사고 시 3단계** : Kill switch → 정보 수집 → 회복
- **자동화** : systemd 자동 재시작 / broker reconnect / Slack alert / etcd snapshot

말로 :
> 본 시스템의 운영 부담은 매일 5분입니다. 사고 시에도 3단계 체크리스트 한 장으로
> 30분 안에 회복합니다.

### Stage 8 : Q & A (5분)

준비된 답변 :

| 질문 | 답변 |
|---|---|
| C 매매 엔진은 무수정? | 네. wire schema 같은 인프라 변경만 양측 동시 deploy. |
| 운영 부하는? | mci-price 1 인스턴스가 6.4k tick/s 처리. 운영은 다중 인스턴스. |
| 보안은? | DMZ ↔ Internal 분리 + JWT + mTLS + IP allowlist + rate-limit. |
| swap-lock 의 atomic 보장? | Reg.Put(near) → Put(far) → SwapIdx.PutSwap 순차 + 부분실패 시 revoke. |
| 마진 정책 변경 시 다운타임? | 0 — etcd watch 로 hot reload. |
| 다중 사이트는? | 가능. deployment-scenario-multi-site.md 참조. |

---

## 3. 시현 직후 정리

```bash
# 부하 도구 종료
pkill -f load-gen 2>/dev/null

# tickloop 등 정리 — 정상 운영 시 그대로 두고, 시현 종료 후만 :
./scripts/wtg-stack-down.sh

# 또는 그대로 두고 watch 만 종료
```

후처리 :
- 시현 후 청중 피드백 메모 → `docs/demo-feedback-YYYY-MM-DD.md`
- 시현 중 발생한 함정이 있으면 본 문서 §10 (트러블슈팅) 에 추가

---

## 4. 시현 시간 단축 / 확장

### 4.1 30분 단축 (경영진 / 사업)

- Stage 1, 2, 3, 6 만. swap-lock + 사고 시뮬레이션 생략.
- 핵심 메시지 : "C 엔진 무수정 + 마진 차등 + 단순한 운영"

### 4.2 45분 (운영팀 짧은 시간)

- Stage 1, 2, 3, 5, 7. swap-lock 생략.

### 4.3 90분 풀 (신규 개발자)

- 60분 시나리오 + 다음 추가 :
  - `../directory-structure.md` 한 페이지 투어 (5분)
  - `cside/wtgprice/sample.c` 코드 한 줄씩 설명 (5분)
  - `pkg/mymq/conventions.go` 의 ApplName / Channel / Exchange 카탈로그 (5분)
  - 부하 테스트 `./scripts/load-test.sh mid` 6.4k tick/s 실시간 보기 (10분)

---

## 5. 시현 사전 리허설 (시현 전날 권장)

본 시나리오를 **혼자 한 번 끝까지** 진행 :

- [ ] 모든 명령이 1초 안에 실행되는지 (네트워크 / 캐시 미스 없는지)
- [ ] 브라우저 페이지 로딩이 매끄러운지
- [ ] swap-lock 응답이 정상 (`successes: 1`)
- [ ] load-gen 부하가 ws backpressure 일으키는지
- [ ] 라이브 ws 가 backpressure 후 자동 재연결 되는지
- [ ] admin UI 의 운영 모니터링이 Prometheus 데이터 보여주는지

리허설에서 문제 발견 시 본 문서 §10 에 함정 추가.

---

## 6. 시현 중 청중 관심 끄는 포인트

각 stage 마다 **한 마디** :

| Stage | 한 마디 |
|---|---|
| 1 | "기존 C 엔진 그대로 + web 게이트웨이 + 단일 API" |
| 2 | "운영자가 매일 5분 안에 확인하는 첫 화면입니다" |
| 3 | "같은 호가가 사용자 등급에 따라 다르게 보입니다" |
| 4 | "swap 거래의 두 leg 가 atomic 하게 잠깁니다" |
| 5 | "시스템 스스로 사고를 짧게 끝냅니다" |
| 6 | "필요한 것만 켜는 정책 단순화 — 재배포 없이 변경 가능" |
| 7 | "운영 부담은 매일 5분, 사고 시 3단계" |

---

## 7. 시현 중 절대 하지 말 것

- ❌ 코드 편집기 화면 띄우기 (청중 집중 분산)
- ❌ "이것도 있는데..." 즉흥 추가 (시간 깨짐)
- ❌ 실패한 명령을 디버깅하면서 시간 끌기 — 즉시 다음 stage 로
- ❌ broker 끊김의 진짜 에러 메시지 — 미리 시뮬레이션
- ❌ Q&A 답변에 자신 없으면 "확인 후 답변 드리겠습니다" — 추측 X

---

## 8. 시현용 핵심 명령 모음 (인쇄해서 옆에)

```bash
# 부팅
./scripts/wtg-stack-up.sh --with-prom

# 상태
./scripts/wtg-status.sh

# swap-lock 활성 (필요시)
pkill -f "build/bin/mci-price"; sleep 1
./build/bin/mci-price --dev --no-broker --listen :8082 --grpc :50051 \
    --enable-swap-lock \
    --symbols etc/symbols.json --pricing etc/pricing.json --profiles etc/profiles.json \
    > logs/mci-price.log 2>&1 &

# 시세 tick 주입
curl -s -X POST http://127.0.0.1:8082/v1/dev/tick \
    -H 'Content-Type: application/json' \
    -d '{"symbol":"USDKRW","bid":1380.50,"ask":1381.20}'

# swap-lock 호출
curl -s -X POST http://127.0.0.1:8082/v1/quote/swap/lock \
    -H 'Content-Type: application/json' \
    -d '{"pair":"USDKRW","near":{"tenor":"SPOT"},"far":{"tenor":"1M"},
         "profile":"WEB.BRANCH.VIP","customer_id":"demo",
         "side":"buy_sell","amount":1000000}'

# 부하 시뮬레이션
./build/bin/load-gen --feeds SMB:30044 \
    --pairs USDKRW,EURKRW,JPYKRW,GBPKRW,EURUSD,USDJPY \
    --rate 200 --duration 30s --stats "" --best "" &

# 정리
./scripts/wtg-stack-down.sh
```

---

## 9. 청중별 사전 안내 메일 템플릿

시현 1일 전 청중에게 :

> 안녕하세요.
> 내일 (YYYY-MM-DD HH:MM) WTG 시현을 진행합니다.
> 본 시현에서는 :
> 1. WTG 의 전체 아키텍처 (5분)
> 2. 운영 대시보드 투어 (10분)
> 3. 라이브 시세 + 마진 정책 (10분)
> 4. swap 거래 잠금 + QuoteID (10분)
> 5. 사고 시뮬레이션 + 자동 복구 (10분)
> 6. 단순화 정책 + 운영 자동화 (10분)
> 7. Q&A (5분)
>
> 준비물 : 노트북 (브라우저 접근만)
> 시현 URL : (사내 admin UI 또는 본 노트북 화면 공유)
> 사전 자료 : admin-ui-manual.md 의 §1~3 (사이드바 한눈에)

---

## 10. 트러블슈팅 (시현 중 함정)

### 10.1 admin UI 가 빈 화면

원인 가능성 :
- mci-admin 미기동 → `./scripts/wtg-status.sh` 확인
- 브라우저 캐시 → Cmd+Shift+R (hard reload)
- JS syntax error → 본 사고는 이전에 fix 됨 (`swState` 충돌), 발생 시 `logs/mci-admin.log` 확인

### 10.2 시세 라이브 ws 가 안 흐름

원인 :
- mci-edge-price 미기동 → status 확인
- tickloop 죽음 → `pgrep -af wtg-dev-tickloop`, 없으면 재시작 :
  ```bash
  nohup python3 /tmp/wtg-dev-tickloop.py > logs/dev-tick.log 2>&1 &
  ```

### 10.3 swap-lock 응답이 404

원인 — mci-price 가 `--enable-swap-lock=false` (default).

해결 — §1.3 의 재시작 절차.

### 10.4 swap-lock 응답이 "no BEST/cross snapshot for USDKRW"

원인 — BestConsumer 에 호가 없음. tick 주입 안 됨.

해결 :
```bash
curl -s -X POST http://127.0.0.1:8082/v1/dev/tick \
    -H 'Content-Type: application/json' \
    -d '{"symbol":"USDKRW","bid":1380.50,"ask":1381.20}'
```

### 10.5 운영 모니터링이 모두 ERR

원인 — Prometheus 미기동 또는 mci-admin 에 `--prom-url` 없음.

해결 :
```bash
# Prometheus 살아있는지
curl -s http://127.0.0.1:9095/-/ready

# 없으면
prometheus --config.file=logs/prometheus.yml \
    --storage.tsdb.path=logs/prom-data \
    --web.listen-address=127.0.0.1:9095 \
    --storage.tsdb.retention.time=1h \
    > logs/prometheus.log 2>&1 &

# mci-admin 재시작 with --prom-url http://127.0.0.1:9095
```

### 10.6 backpressure 시뮬레이션이 안 일어남

원인 — load-gen 부하가 적거나, mci-edge-price queue_cap 이 큼.

해결 — rate 를 500 으로 증가 또는 여러 ws 클라이언트 동시 연결.

### 10.7 마진 계산기가 빈 결과

원인 — Profile 또는 customer_id 가 카탈로그 미등록.

해결 — Profile 은 `WEB.BRANCH.VIP` (확실히 존재), customer_id 는 빈값 또는 "demo".

### 10.8 시현 중 화면이 멈춤 / 답답함

화면 갱신 (단축키) :
- 브라우저 hard reload : Cmd+Shift+R
- 터미널 reset : `clear` 또는 Ctrl+L
- 모든 페이지 같은 데이터 보일 때 — 한 텝만 보여주고 다른 탭 닫기

---

## 11. 시현 후 확인 (선택)

청중 피드백 수집 후 :

- 어떤 stage 에서 청중 관심이 가장 컸나
- 어떤 질문이 가장 많이 나왔나 (다음 시현에 미리 답변 준비)
- 트러블이 있었던 stage → 본 문서 §10 보강

---

## 12. 참고 문서

- `admin-ui-manual.md` — 운영 매뉴얼 (37 페이지 상세)
- `operations-routine.md` — 운영자 SOP (인쇄용)
- `deployment-scenario-ha-channel.md` — 시현에서 보여줄 architecture
- `../simplification-guide.md` — 단순화 v3 의 이론적 배경
- `../admin-ui-test-guide.md` — 페이지별 테스트 시나리오 (시현 사전 점검용)
