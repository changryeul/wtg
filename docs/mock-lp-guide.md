# mock-lp 테스트 가이드 — 시세 파이프라인 로컬 검증

> `cmd/mock-lp` 로 LP별 결정적 호가/체결을 UDP FIX 로 쏴서 시세 경로 전체
> (per-source → BEST 산정 → cross → forward swap → AlgoStream)를 broker/etcd
> 없이 로컬에서 검증한다. 랜덤 부하는 `load-gen`, **시나리오 검증은 mock-lp**.

## 0. 경로

```
                                                          ┌─(SubscribeAlgo)──────────▶ algo-tester   [algo 봇] raw BEST
mock-lp ─UDP FIX─▶ quote-forwarder ─gRPC PublishTick─▶ mci-price
 (LP별 결정적 호가)  (--publish-mode grpc)   (BEST/cross/swap) └─(SubscribeQuote, Profile 마진)▶ mci-edge-price ─ws─▶ web/HTS  [고객]
```

**두 소비 경로가 다르다** — algo 봇은 raw BEST/per-source(`SubscribeAlgo`, §3~5),
고객(web/HTS)은 Profile 마진 적용 quote(`mci-edge-price /v1/subscribe` ws, §6).

- **quote-forwarder 가 필수** — mock-lp 는 UDP 를 forwarder 로 보내고, forwarder 가
  mci-price gRPC 로 중계한다. forwarder 없이 mock-lp 만 쏘면 아무 데도 안 들어간다.
- broker/etcd 불필요 (`mci-price --no-broker`). cross(예: CNH/KRW)만 etcd PairMaster
  formula 의존이라 통합 테스트로 커버 (§6).

## 1. 사전 준비

```bash
cd ~/mywork/wtg
make build      # build/bin/{mci-price,quote-forwarder,mock-lp,algo-tester}
```

## 2. 방법 A — 자동 검증 (권장)

빌드 + 격리 스택 부팅 + mock-lp 송신 + 기대값 대사까지 한 방:

```bash
./scripts/mock-lp-verify.sh
```

→ 끝에 `✅ mock-lp e2e 검증 통과 (BEST + per-source + forward swap)`.
BEST 산정 / per-source(SMB) / forward tenor swap 을 **값까지** 자동 assert 한다.
tickloop 이 없어 값이 안 섞인다. "잘 도나?" 확인은 이거면 충분.

## 3. 방법 B — 직접 관찰 (터미널 4개, 순서 중요)

값을 눈으로 보며 반복 자극하고 싶을 때. **반드시 mci-price 부터.**

### 터미널 1 — mci-price (먼저!)
```bash
./build/bin/mci-price --no-broker --dev \
  --listen :8082 --grpc 127.0.0.1:50051 \
  --algo-stream --symbols etc/symbols.json
```
→ 로그에 `PriceService gRPC listen 시작 addr=:50051` 확인 후 다음 단계.
빠른 확인: `lsof -nP -iTCP:50051 -sTCP:LISTEN` (비어있으면 아직 안 뜬 것).

### 터미널 2 — quote-forwarder
```bash
./build/bin/quote-forwarder --publish-mode grpc \
  --price-grpc 127.0.0.1:50051 \
  --multi "SMB:30044,KMB:30045" --bind 127.0.0.1 --metrics 127.0.0.1:9091
```
> `--multi` 로 SMB/KMB 두 원천을 각각 UDP 포트에 매핑 (다중소스 BEST 검증에 필수).
> 단일피드로 충분하면 `--listen 127.0.0.1:30044` 만 써도 된다.

### 터미널 3 — algo-tester (수신 관찰)
```bash
./build/bin/algo-tester --target 127.0.0.1:50051 --symbols USDKRW --duration 60s
```

### 터미널 4 — mock-lp (시세 송신)
```bash
# 내장 시나리오 반복 (0.5초마다)
./build/bin/mock-lp --feeds "SMB:127.0.0.1:30044,KMB:127.0.0.1:30045" --interval 500ms
# 1회만: --once
```

**순서**: mci-price → forwarder → algo-tester → mock-lp.

## 4. 내장 시나리오 & 기대값

`mock-lp` 는 `--scenario` 없이도 내장 데모를 쏜다:

| LP | pair | bid | ask | last |
|---|---|---|---|---|
| SMB | USDKRW | 1380.10 | 1380.25 | 1380.15 |
| KMB | USDKRW | 1380.05 | 1380.20 | — |
| SMB | USDCNH | 7.1000 | 7.1030 | — |
| KMB | USDCNH | 7.0995 | 7.1025 | — |

**기대 결과 (algo-tester)**:
- **BEST 모드** (`--sources` 없음): `source=BEST bid=1380.10`(max bid) `ask=1380.20`(min ask) `mid=1380.15` `last=1380.15`
- **per-source** (`--sources SMB`): `source=SMB bid=1380.10 ask=1380.25` (SMB 원본 그대로)

## 5. 관찰 변형

### per-source 원천별 호가
```bash
./build/bin/algo-tester --target 127.0.0.1:50051 --symbols USDKRW --sources SMB --duration 60s
```

### forward tenor (swap 반영)
로이터 수신값 + 운영자 delta 를 주입한 뒤 tenor 구독. `effective = received + delta`,
`forward = spot + effective` (add 규약).
```bash
curl -s -XPOST localhost:8082/v1/pricing/swap-received -H "X-WTG-User: op" \
  -d '{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":2.50,"ask":2.70}]}'
curl -s -XPOST localhost:8082/v1/pricing/swap-delta -H "X-WTG-User: op" \
  -d '{"updates":[{"pair":"USD/KRW","tenor":"M01","bid":0.05,"ask":-0.03}]}'
./build/bin/algo-tester --target 127.0.0.1:50051 --symbols USDKRW --tenors M01 --duration 60s
# → tenor=M01, bid=1380.10+2.55, ask=1380.20+2.67
```

### 내 시나리오 파일
```bash
cat > /tmp/my.json <<'JSON'
{"quotes":[
  {"lp":"SMB","pair":"USDKRW","bid":1381.00,"ask":1381.20,"last":1381.10,"last_qty":100000},
  {"lp":"KMB","pair":"USDKRW","bid":1381.05,"ask":1381.25}
]}
JSON
./build/bin/mock-lp --feeds "SMB:127.0.0.1:30044,KMB:127.0.0.1:30045" --scenario /tmp/my.json --interval 1s
```

### REST 스냅샷 (algo-tester 대신)
```bash
curl -s localhost:8082/v1/best-stats  | jq .
curl -s localhost:8082/v1/price-stats | jq .
```

## 6. client 경로 (mci-edge-price ws) — 고객이 받는 quote

§3~5 의 algo-tester 는 raw BEST(`SubscribeAlgo`)다. **실제 web/HTS 고객**은
mci-edge-price 의 ws(`/v1/subscribe`)로 **Profile 마진 적용 quote**(`SubscribeQuote`)를 받는다.

**조건 2가지**:
1. mci-price 를 `--pricing` + `--profiles` 로 띄워야 Profile quote 가 생성된다
   (§3 처럼 `--symbols` 만 주면 edge ws 는 붙어도 **빈** 상태).
2. mci-edge-price 를 `--quote-stream` 으로 upstream(:50051)에 붙인다.

### 터미널 1 — mci-price (pricing/profiles 포함)
```bash
./build/bin/mci-price --no-broker --dev --listen :8082 --grpc 127.0.0.1:50051 \
  --algo-stream --symbols etc/symbols.json \
  --pricing etc/pricing.json --profiles etc/profiles.json
```
→ 로그에 `PricingConsumer 활성 ... profile_count:7` 확인. (터미널 2 forwarder, 터미널 4 mock-lp 는 §3 그대로)

### 터미널 5 — mci-edge-price (client hop)
```bash
./build/bin/mci-edge-price --dev --listen :8083 --upstream 127.0.0.1:50051 --quote-stream
```

### 터미널 6 — client ws (websocat 로 web/HTS 흉내)
```bash
websocat "ws://127.0.0.1:8083/v1/subscribe?x_wtg_user=alice01&profile=WEB.BRANCH.VIP"
```
- 붙기만 하면 전체 pair 수신. 필터하려면 stdin 으로 제어 프레임:
  `{"type":"subscribe","pairs":["USD/KRW"]}` → 서버가 `{"type":"subscribed","pairs":[...]}` echo.
- 운영은 `?access_token=<JWT>`, dev 는 `?x_wtg_user=<id>` (+ dev 한정 `?profile=` override).

### 기대값 (client 가 받는 것)
BEST `1380.10 / 1380.20` 에 **Profile 마진**이 먹어 스프레드가 벌어진다:

| profile | USD/KRW 마진 | client bid / ask |
|---|---|---|
| `WEB.BRANCH.VIP` | 0.02 | 1380.08 / 1380.22 |
| `WEB.BRANCH.GOLD` | 0.05 | 1380.05 / 1380.25 |
| `WEB.BRANCH.STD` | 0.10 | 1380.00 / 1380.30 |

→ profile(Tier)별로 값이 달라지는 게 마진이 먹은 증거. (마진 카탈로그 `etc/pricing.json`)

### VIP quote 확인 — 프레임에 raw_bid/raw_ask + tier 가 같이 온다

client quote 프레임은 **고객가(`bid/ask`)와 원본 BEST(`raw_bid/raw_ask`), `tier` 를
한 프레임에** 담는다. 프레임 하나로 마진 적용 여부를 즉시 검증:

```bash
websocat "ws://127.0.0.1:8083/v1/subscribe?x_wtg_user=alice01&profile=WEB.BRANCH.VIP" \
| jq 'select(.type=="quote" and .pair=="USD/KRW")
      | {tier, bid, ask, raw_bid, raw_ask,
         margin_bid:(.raw_bid-.bid), margin_ask:(.ask-.raw_ask)}'
```
기대 출력:
```json
{"tier":"VIP","bid":1380.08,"ask":1380.22,"raw_bid":1380.10,"raw_ask":1380.20,
 "margin_bid":0.02,"margin_ask":0.02}
```
확인 포인트: ① `tier:"VIP"` (Profile 라우팅) ② `bid/ask`(고객가) vs `raw_bid/raw_ask`(원본 BEST)
③ `margin_bid/ask=0.02` (VIP 마진 정확).

**Tier 교차 검증** — profile 만 바꿔 재접속하면 마진 폭이 달라진다:
```bash
websocat "ws://127.0.0.1:8083/v1/subscribe?x_wtg_user=bob&profile=WEB.BRANCH.STD" \
| jq 'select(.type=="quote" and .pair=="USD/KRW") | {tier, bid, ask, margin:(.raw_bid-.bid)}'
# → tier:"STD", margin:0.10.  VIP 0.02 < GOLD 0.05 < STD 0.10 이면 정상.
```

quote 프레임 주요 필드: `bid/ask`(마진 적용 고객가), `raw_bid/raw_ask`(원본 BEST),
`channel/site/tier/tenor`(Profile), `v`(pricing table version), `quote_id`/`valid_until_unix_nano`(거래 lock 용).

> **간단히**: `wtg-stack-up.sh` 는 mci-price(pricing/profiles) + mci-edge-price(:8083)를 이미
> 함께 띄운다. `--with-forwarder` 만 추가하면 위 배선이 한 번에 — 단 tickloop 이 같은
> 심볼을 섞으니, mock-lp 만 깨끗이 보려면 위 수동 방식.

## 7. cross (CNH/KRW 등) 검증

cross 합성은 etcd PairMaster formula 의존이라 shell 로컬 스택으론 어렵다 →
embedded etcd 통합 테스트로 값까지 결정적 검증:
```bash
go test -tags integration ./internal/price/ -run TestMockLP_CrossE2E
```
(mds worse-side div 산식과 값 일치 확인)

## 8. 트러블슈팅

| 증상 | 원인 | 해결 |
|---|---|---|
| forwarder `connection refused :50051` | mci-price 가 안 떠 있음 | 터미널 1 먼저. `lsof -nP -iTCP:50051 -sTCP:LISTEN` 로 확인 |
| mock-lp 쐈는데 algo-tester 무반응 | quote-forwarder 미기동 | forwarder 를 mci-price gRPC(:50051)로 띄웠는지 확인 |
| BEST 값이 계속 흔들림 | `wtg-stack-up.sh` 의 tickloop 이 같은 심볼을 랜덤 주입 | 방법 A(격리 스크립트) 사용, 또는 tickloop 없는 수동 스택 |
| KMB 가 BEST 에 안 잡힘 | forwarder 가 단일 `--listen` (30044)만 | `--multi "SMB:30044,KMB:30045"` 로 다중 원천 |
| `wtg-status` 가 UP 인데 무응답 | 이 셸의 grep 가리기로 오탐 | `pgrep -f mci-price` + `lsof` 로 실제 확인 |
| client ws(:8083) 붙었는데 quote 안 옴 | mci-price 에 `--pricing`/`--profiles` 없음 → Profile quote 미생성 | §6 대로 pricing/profiles 로드 + edge `--quote-stream` |
| client 값이 raw BEST 와 같음(마진 0) | 해당 Tier 마진이 pricing 에 없거나 profile 미매칭 | `?profile=` 를 유효 키(예: WEB.BRANCH.VIP)로, `etc/pricing.json` 확인 |

## 관련
- `cmd/mock-lp/{main.go,scenario.go}` — 시나리오 LP UDP FIX 송신
- `scripts/mock-lp-verify.sh` — 자동 e2e 대사
- `cmd/algo-tester/main.go` — AlgoQuote 관찰 (`--sources`/`--tenors`/`--json`)
- `docs/algo-quote-reconciliation.md` — algo 시세 경로 대사표
- `docs/reuters-swap-adapter-spec.md` — swap 주입 seam
