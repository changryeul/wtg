# FX swap 거래 지원 명세 (Phase S3, 초안)

> 상태: **DRAFT**. 본 문서는 forward/spot 처리(Phase 5) 와 동일한 합의
> 절차를 거쳐 GA 전 freeze 한다. 합의 전 코드는 `internal/price/swap_lock.go`
> 의 stub 만 존재 — wire/응답 schema 는 본 문서를 단일 출처로 본다.

## 1. 범위

본 명세는 매칭 엔진이 **FX swap (near leg + far leg 동시 거래)** 의 원가/잠금을
mci-price 에 요청하는 endpoint 와, 그에 따른 broker 트랜잭션의 quote_id 검증
정책을 다룬다. 단순 forward(=far leg 단독) 는 기존 `POST /v1/quote/forward/lock`
을 그대로 사용 — 본 endpoint 의 책임이 아니다.

비범위:
- **재정통화 (cross-rate) 합성** — 이미 `pkg/pricing/crossrate.go` +
  `internal/price/crossrate_consumer.go` 로 자동 처리. swap 응답에서도
  near/far 양쪽이 동일 base direct pair 에서 합성된다 (라운드트립 일관).
- **FX 옵션 / NDF / FRA** — 별도 phase.

## 2. 용어

| 용어 | 정의 |
|---|---|
| swap | 동일 통화쌍의 **두 leg 거래** — near (가까운 결제일) + far (먼 결제일) 반대 방향. 예: USD/KRW spot 매수 + 1M forward 매도. |
| near leg | 결제일이 빠른 쪽. tenor=SPOT 가 흔하지만 forward-forward swap (예: 1W ↔ 1M) 도 가능. |
| far leg | 결제일이 늦은 쪽. |
| swap_id | 두 leg 를 묶는 권위 ID — Registry 에 인덱스로 저장, broker 트랜잭션 routing key. |
| leg quote_id | leg 별 quote_id. 매매 AP 가 두 leg 를 동시 검증할 때 단위. |
| swap_diff | far - near 호가 차이. swap_point 의 결과값 (forward swap point margin 과 다름 — 본 차이는 customer-applied 후 값). |

## 3. 설계 원칙

1. **단일 시점 snapshot** — 한 호출 내에서 두 leg 모두 **동일한 `now`** 와
   **동일한 `BestConsumer.Stats()` 결과** 를 사용한다. leg 간 ts skew 금지.
2. **단일 `table_version`** — 호출 진입 시점에 `pricingStore.Load()` 1회만 한다.
   처리 중 운영자가 swap_point/마진 변경해도 응답은 일관.
3. **원자 발급** — near/far quote_id 둘 다 Registry.Put 성공해야 응답.
   둘 다 실패는 거래 거부. 한쪽만 실패는 본 명세 4.5절 정책.
4. **swap_id 는 leg 와 별개 ID** — leg 의 quote_id 둘로 swap_id 를 합성하지
   않는다 (인덱스 단순화 + 매매 AP 가 두 leg 묶음을 single 단위로 인지).
5. **retry 금지** — 클라이언트 측에서도. quote_id 중복 발급은 audit 오염.
   timeout/네트워크 실패는 거래 거부 + 운영 alert.
6. **재정통화 영향 X** — `lookupSpotRaw` (BEST → cross fallback) 패턴을
   재사용. near/far 모두 동일 raw spot 에서 출발하므로 cross 일 때도 정합.

## 4. Endpoint

### 4.1 `POST /v1/quote/swap/lock`

DMZ edge 비노출 — 내부 매칭 엔진만 호출. 인증은 망 격리. 향후 mTLS 도입
시 `/v1/quote/forward/lock` 과 동일 방식으로 확장.

가용성 게이트 (404/503):

- `pricingStore == nil` 또는 `best == nil` → 404 (등록 안 됨).
- `PricingTable 미로드` → 503.
- `BEST consumer 미활성` → 503.
- `quoteIDGen / quoteIDReg` 미주입 → 등록 자체 안 됨 (forward/lock 과 동일).

### 4.2 Request 본문

```json
{
  "pair": "USD/KRW",
  "near": { "tenor": "SPOT" },
  "far":  { "tenor": "1M" },
  "profile": "WEB.BRANCH.VIP",
  "customer_id": "C12345",
  "side": "buy_sell",
  "amount": 1000000
}
```

| 필드 | 필수 | 설명 |
|---|---|---|
| `pair` | ✅ | "USD/KRW" 형식. cross pair (예: "100JPY/KRW") 가능. |
| `near.tenor` | (택1) | "SPOT" \| "1W" \| "1M" \| ... — far 와 다른 tenor 만. 같으면 400. |
| `near.value_date` | (택1) | "YYYY-MM-DD" — broken-date. tenor 와 동시 지정 시 value_date 우선. |
| `far.tenor` | (택1) | 동상. |
| `far.value_date` | (택1) | 동상. |
| `profile` | ✅ | `Site.Tier.Pair` 형식 Profile key. |
| `customer_id` | ⛔ | 5-Layer 마진 적용. 미지정 시 base profile 마진만. |
| `side` | ⛔ | "buy_sell" (near=buy, far=sell) \| "sell_buy". audit 용. |
| `amount` | ⛔ | base 통화 수량. audit 용. |

검증:
- near 와 far 의 (tenor or value_date) 가 동일하면 400 (`same leg`).
- near 의 결제일 > far 의 결제일이면 400 (`leg order inverted`).
  - SPOT 일수는 1차 T+2 고정. value_date 환산 후 비교.

### 4.3 Response

```json
{
  "swap_id": "SW-l9m4-7k",
  "pair": "USD/KRW",
  "profile": "WEB.BRANCH.VIP",
  "customer_id": "C12345",
  "side": "buy_sell",
  "issued_unix_nano": 1796102345123456789,
  "valid_until_unix_nano": 1796102345623456789,
  "table_version": 4271,
  "near": {
    "quote_id": "QN-l9m4-7k1",
    "tenor": "SPOT",
    "value_date": "",
    "bid": 1373.12, "ask": 1373.22,
    "raw_bid": 1373.19, "raw_ask": 1373.19,
    "swap_bid": 0.0, "swap_ask": 0.0,
    "interpolation": null
  },
  "far": {
    "quote_id": "QF-l9m4-7k2",
    "tenor": "1M",
    "value_date": "",
    "bid": 1374.05, "ask": 1374.18,
    "raw_bid": 1373.19, "raw_ask": 1373.19,
    "swap_bid": 0.86, "swap_ask": 0.96,
    "interpolation": null
  },
  "swap_diff": {
    "bid_diff": 0.93,
    "ask_diff": 0.96
  }
}
```

`swap_diff` 는 `far.bid - near.bid`, `far.ask - near.ask` — customer-applied
이후 값이라 정책상 swap_point 와 다를 수 있다 (5-Layer 적용 잔차).

### 4.4 swap_id 형식

`SW-<base36 ms>-<base36 seq>` — 발급자(`Issuer`) 가 mci-price 인스턴스 prefix.
leg quote_id 와 prefix 만 다르고 발급 로직(`quoteid.Generator`) 공유.

Registry 인덱스:

```
key:   swap:<swap_id>
val:   { near_qid, far_qid, issued, valid_until }

key:   quote:<near_qid>   → leg Record (Tenor 에 "SWAP_NEAR" 메타 추가)
key:   quote:<far_qid>    → leg Record (Tenor 에 "SWAP_FAR" 메타 추가)
```

매매 AP 는 swap_id 만 받아도 두 leg 검증 가능 — broker 트랜잭션 payload 단순화.

### 4.5 부분 실패 정책

| 상황 | 응답 | 후속 |
|---|---|---|
| near.Put 성공 → far.Put 실패 | 503 + body `{"error":"swap registry partial: far"}` | near quote_id revoke 시도 (best-effort). 매매 AP 는 swap_id 미존재 시 거부. |
| near.Put 실패 (전제) | 503 (far 시도 안 함) | — |
| swap_id Put 실패 (둘 다 leg 성공 후) | 503 + revoke near/far 시도 | — |

revoke 는 best-effort — Registry 의 `Delete` 또는 expire 자연 처리. 자세히는
Phase S3-b 에서 인덱스 + revoke 트랜잭션 RFC.

## 5. 매칭 엔진 호출 정책

- **체결 직전 1회**. swap quote → broker 트랜잭션 → 응답 한 사이클.
- **`valid_until_unix_nano` 안전마진** — 권장 100ms. 그 안에 매매 트랜잭션
  submit 못하면 quote 폐기 + 재요청.
- **retry 금지** — timeout/네트워크 실패도 즉시 거부. spot 처럼 idempotent
  아니다.
- **자체 BEST 와 raw 이격 모니터** — `near.raw_bid` = `far.raw_bid` 가 보장됨
  (단일 snapshot). 매칭 엔진 자체 BEST 와 차이가 크면 시계 skew 의심.

## 6. broker 측 변경 (매매 AP)

- 새 transaction 알리아스 — 예: `WSWAP_LOCK` → 매매 AP 가 본 payload 수신:

  ```json
  {"swap_id":"SW-...","pair":"USD/KRW","side":"buy_sell","amount":1000000}
  ```

- 매매 AP 는 swap_id 로 Registry 조회 → near/far leg Record 동시 `ValidAt(now)`
  검증. 하나라도 만료/위조면 전체 거래 거부 (atomic).
- 비즈니스 권한 (거래 한도, 통화쌍 활성, swap 거래 자격) 은 매매 AP 가 판단
  — WTG 는 wire passthrough.

## 7. 운영 / 관측

- `mci-price` 메트릭 신설:
  - `wtg_swap_lock_requests_total{result}` (ok|err)
  - `wtg_swap_lock_duration_seconds` (histogram)
  - `wtg_swap_lock_partial_failures_total{stage}` (near|far|swap_id)
- `mci-admin` 화면:
  - "P5 swap 잠금 통계" 페이지 — 최근 swap_id 발급, partial 비율.
  - 기존 forward/lock 페이지에 swap 토글 추가 (테스트용 발급).
- `docs/observability.md` 항목 추가 — partial 비율 > 0.1% 시 alert.

## 8. Phase 분할

| Phase | 산출물 | 상태 |
|---|---|---|
| **S3-a** | spec freeze + `swap_lock.go` stub + input validation 테스트 | ✅ 완료 |
| **S3-b** | `quoteid.SwapIndex` 인터페이스 + MemoryRegistry 구현 + 원자 발급(near→far→swap_index 순차) + 부분실패 revoke + `AtomicSwapLockMetrics` | ✅ 완료 |
| **S3-e (route 부분)** | `--enable-swap-lock` flag + `Server.registerSwapLockRoutes` + `POST /v1/quote/swap/lock` + `GET /v1/quote/swap/stats` + cmd/mci-price 자동 wire | ✅ 완료 |
| **S3-c (WTG 측)** | `RedisRegistry` 에 SwapIndex 4 메서드 + proto `ValidateSwap`/`ConsumeSwap` RPC + gRPC/HTTP 핸들러 (`/v1/quoteid/validate-swap`, `/v1/quoteid/consume-swap`) + AND 정책 + atomic-skip 부분실패 + 단위 테스트 6 | ✅ 완료 |
| S3-c (mymq AP) | 매매 AP 가 ValidateSwap/ConsumeSwap 호출하도록 변경 | mymq repo 영역 (별도 작업) |
| **S3-d** | `cside/wtgprice/` 신설 — POSIX socket + 간이 JSON 파서. `wtg_price_swap_lock` + sample + `make test-wtgprice` wire 호환성 e2e | ✅ 완료 |
| **S3-e (admin UI + alert)** | mci-admin "FX swap 잠금 통계" 페이지 (`/v1/admin/price/swap-stats` 프록시) + `etc/grafana/mci-price-swaplock-alerts.yml` 4 alert (PartialFailureRate / RevokeFailure / SwapIndexFailure / ConsumeSwapPartialRace) | ✅ 완료 |
| **S3-e (Prometheus 메트릭)** | `internal/price/metrics_swap.go` — `swapLockCollector` + `swapValidationCollector` (prometheus.Collector 구현, labels 포함). 메트릭명 5종 (`wtg_swap_lock_*` / `wtg_swap_validate_total` / `wtg_swap_consume_total` / `wtg_consume_swap_partial_race_total`) + `RegisterSwapMetrics` + cmd/mci-price 자동 wire | ✅ 완료 |

## 9. 미해결 사항 (RFC 후속)

- **SPOT 일수 pair 별 컨벤션** — 현재 T+2 고정. USD/CAD T+1 같은 pair 는
  `PricingTable.SpotDays(pair) int` 확장 필요. forward/lock 과 공유.
- **value_date 양쪽 동시 broken-date** — 보간 계산이 두 번 일어남.
  cache 가능하지만 1차는 그대로 두 번 호출 (성능 측정 후 결정).
- **multi-hop cross** — `pkg/pricing/crossrate.go` 가 2-leg 까지. 3-leg 이상
  필요해지면 별도 phase.
- **swap_id 중복 발급 보호** — `quoteid.Generator` 의 seq 가 monotonic 이라
  이론상 무. multi-instance 환경에서 `Issuer` prefix 로 분리.

## 10. 참고

- 코드: `internal/price/swap_lock.go` (stub), `internal/price/forward_lock.go` (참조 패턴)
- 명세: `docs/cooker-quote-schema.md`, `docs/margin-policy.md`, `docs/quoteid-validation-rfc.md`
- 운영: `docs/observability.md`, `docs/operations.md`
