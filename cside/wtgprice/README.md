# cside/wtgprice — 매칭 엔진용 mci-price swap/lock C SDK

Phase S3-d. C 매칭 엔진이 FX swap 거래 직전에 `POST /v1/quote/swap/lock` 을
호출해 두 leg quote_id + swap_id 를 받아오는 hot-path 용 클라이언트.

## 설계 원칙

- **외부 의존 0** — POSIX socket + HTTP/1.1 minimal + 간이 JSON 파서. mymq
  운영 환경 (AIX/Solaris/HPUX/Linux/Darwin) 에서 그대로 빌드. cside/wtgpush
  와 동일.
- **retry 금지** — swap_lock 의 quote_id 가 unique 이므로 retry 시 중복 발급
  위험. 본 SDK 는 단 1회 시도. timeout / 4xx / 5xx 모두 호출자에게 그대로
  전달 — 정책은 매칭 엔진이 결정 (보통 거래 거부).
- **TLS 없음** — Internal 망 전용. 향후 mTLS 도입 시 `wtgprice_tls.c` 별도.
- **thread-safe** — 각 호출이 자체 socket open/close. `wtg_price_client_t` 의
  `last_*` 필드는 호출별 갱신 — 다중 스레드에서 cli 공유 시 호출자가 mutex.

## 빌드

```bash
make                 # libwtgprice.a + sample
make clean
```

루트에서:

```bash
make wtgprice        # cside/wtgprice 빌드
make wtgprice-clean
```

## 매칭 엔진 통합

```c
#include "wtgprice.h"

/* 프로세스 시작 시 1회. */
wtg_price_client_t cli;
wtg_price_init(&cli, "mci-price.internal", 8082, /*timeout_ms=*/1000);

/* 체결 직전. */
wtg_swap_req_t req = {
    .pair        = "USD/KRW",
    .near_tenor  = "SPOT",         /* or .near_value_date = "2026-06-15" */
    .far_tenor   = "1M",
    .profile     = "WEB.BRANCH.VIP",
    .customer_id = "C12345",
    .side        = "buy_sell",
    .amount      = 1000000,
};
wtg_swap_result_t res;
int rc = wtg_price_swap_lock(&cli, &req, &res);
if (rc != WTGPRICE_OK) {
    /* timeout / 4xx / 5xx 어느 쪽도 거래 거부. retry 금지. */
    log_error("swap_lock: %s (http=%d body=%s)",
              wtg_price_strerror(rc), cli.last_http_status, cli.last_error_body);
    reject_trade();
    return;
}

/* res.swap_id 를 매매 transaction payload 에 첨부. */
submit_trade(res.swap_id, res.near.bid, res.far_.ask, /*...*/);
```

## 응답 구조 (`wtg_swap_result_t`)

| 필드 | 의미 |
|---|---|
| `swap_id` | 두 leg 묶음 권위 ID. 매매 transaction routing 단위. |
| `pair`, `issued_unix_nano`, `valid_until_unix_nano`, `table_version` | 거래 시점 메타 |
| `near.quote_id`, `near.tenor`, `near.bid/ask` | near leg 권위 가격 |
| `near.raw_bid/raw_ask` | 시장 BEST 원본 (매칭 엔진의 자체 BEST 와 이격 모니터링용) |
| `far_.*` | 동상. (C 식별자 충돌 회피를 위해 필드명은 `far_`) |
| `bid_diff`, `ask_diff` | `far.bid - near.bid` 등 customer-applied 차이 |

상세 spec: `docs/swap-trade-spec.md`.

## 미지원 필드 (audit-only — 본 SDK 가 추출 생략)

- `interpolation.{from,to,weight,swap_bid,swap_ask}` — broken-date 보간 audit.
  매칭 엔진은 사용하지 않음. 운영 로그에 보존이 필요하면 `raw_body` 보존
  옵션을 후속 phase 에 추가.
- `swap_bid` / `swap_ask` (far leg) — 마진 분해.

필요 시 `wtgprice.c` 의 `extract_leg` 에서 추출 항목을 추가.

## 에러 코드

| 코드 | 의미 |
|---|---|
| `WTGPRICE_OK` | 성공 |
| `WTGPRICE_E_INVALID` | 인자 NULL / 필수 필드 누락 |
| `WTGPRICE_E_RESOLVE` | DNS 실패 |
| `WTGPRICE_E_CONNECT` | connect 실패/timeout (`last_errno` 참조) |
| `WTGPRICE_E_SEND` / `WTGPRICE_E_RECV` | 송수신 실패 |
| `WTGPRICE_E_PARSE` | HTTP/JSON 파싱 실패 |
| `WTGPRICE_E_HTTP_4XX` | 400 (validation), 404 (BEST snapshot 없음) 등 |
| `WTGPRICE_E_HTTP_5XX` | 503 (pricing 미로드 / partial failure) 등 |

4xx/5xx 시 `cli.last_error_body` 가 응답 본문 일부 (`{"error":"..."}`) 를 보존.

## 권장 timeout

| 값 | 비고 |
|---|---|
| 800ms ~ 1000ms | swap_lock 은 forward_lock 보다 약간 더 — 두 leg `ApplyForCustomer` + Registry.Put 2회 + SwapIndex.PutSwap 1회 |

`spec §5` 참조.

## 검증

`make` 후 `./sample <mci-price-host> <port>` 로 단발 호출 가능.

본 SDK ↔ Go side handler 의 wire 호환성은 `test/cside/wtgprice/` 의 e2e
테스트로 검증 (build tag `wtgprice`):

```bash
make test-wtgprice
```
