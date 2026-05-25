# WTG QuoteID C SDK

C 매칭 엔진이 WTG (Winway Trading Gateway) 의 QuoteValidationService 를
호출하기 위한 thin client. gRPC-C++ (~수십 MB) 대신 HTTP REST + libcurl
사용 — engine 시스템 운영 footprint 최소.

## 파일

- `quoteid_client.h` — 공용 C API.
- `quoteid_client.c` — 구현 (libcurl + cJSON).
- `example_fix_flow.c` — FIX NewOrderSingle handler 예제.
- `Makefile` — 빌드 (pkg-config 우선, fallback `-lcurl -lcjson`).

## 의존성 설치

| OS | 명령 |
|----|------|
| Ubuntu / Debian | `sudo apt install libcurl4-openssl-dev libcjson-dev` |
| RHEL / CentOS | `sudo yum install libcurl-devel cjson-devel` |
| macOS | `brew install curl cjson` |
| Alpine | `apk add curl-dev cjson-dev` |

## 빌드

```bash
cd etc/sdk/c
make                    # quoteid_client.o + example_fix_flow
make lib                # libquoteid.a (engine 시스템에 정적 링크)
```

## API 요약

```c
qid_client_t* qid_client_new(const qid_client_options_t* opts);
void          qid_client_free(qid_client_t* c);

qid_err_t qid_validate     (qid_client_t* c, const char* quote_id,
                            qid_validate_result_t* out);
qid_err_t qid_mark_consumed(qid_client_t* c, const char* quote_id,
                            const char* consumer_id, qid_mark_result_t* out);

/* Batch — FIX NewOrderList */
qid_err_t qid_batch_validate     (qid_client_t* c,
                                  const char* const* quote_ids, size_t n,
                                  qid_validate_result_t* out, size_t* nout);
qid_err_t qid_batch_mark_consumed(qid_client_t* c,
                                  const char* const* quote_ids,
                                  const char* const* consumer_ids, size_t n,
                                  qid_mark_result_t* out, size_t* nout);
```

전체 시그니처 / 데이터 모델은 `quoteid_client.h` 주석 참조.

## 전형적 흐름 — FIX NewOrderSingle ('D') handler

```c
/* 0) 부팅 — 1회만 */
curl_global_init(CURL_GLOBAL_DEFAULT);
qid_client_options_t opts = {
    .base_url   = "https://mci-price.internal:8443",
    .engine_id  = "matching-A",                       /* allowlist 등록 식별자 */
    .ca_file    = "/etc/wtg/wtg-ca.crt",              /* mTLS */
    .cert_file  = "/etc/wtg/engine.crt",
    .key_file   = "/etc/wtg/engine.key",
    .timeout_ms = 1000,
};
qid_client_t* qid_cli = qid_client_new(&opts);

/* 1) order handler 안 */
qid_validate_result_t vr;
qid_err_t err = qid_validate(qid_cli, fix_tag117_quote_id, &vr);
if (err != QID_OK) {
    /* 네트워크 / TLS / 5xx — 운영 정책: 신규 주문 거절 (fail-safe). */
    send_exec_reject(order, OrdRejReason_BROKER_OPTION);
    return;
}
if (vr.status != QID_STATUS_OK) {
    send_exec_reject(order, vr.ord_rej_reason);  /* tag 103 그대로 */
    return;
}

/* 2) engine 자체 정책 — slippage / side / tier 한도 */
if (!engine_policy_ok(&vr.record, order)) {
    send_exec_reject(order, OrdRejReason_EXCEEDS_LIMIT);
    return;
}

/* 3) MarkConsumed — atomic 1회 표시 */
qid_mark_result_t mr;
err = qid_mark_consumed(qid_cli, fix_tag117_quote_id, order->order_id, &mr);
if (err != QID_OK || mr.status != QID_STATUS_OK) {
    /* ALREADY_CONSUMED 면 mr.consumed_by 가 먼저 잡은 OrderID — audit */
    send_exec_reject(order, mr.ord_rej_reason);
    return;
}

/* 4) Fill */
fill_order(order, mr.record.bid, mr.record.ask);
```

## 스레드 모델

- `qid_client_t` 인스턴스는 단일 스레드 (libcurl easy handle 의 표준 규칙).
- 다중 스레드 엔진은 `qid_client_pool_t` 사용 (아래).

### qid_client_pool_t — 멀티스레드 엔진 권장

```c
qid_client_pool_t* pool = qid_client_pool_new(&opts, /*size=*/8);

/* order handler thread N */
qid_client_t* c = qid_client_pool_acquire(pool);   /* block 또는 NULL (closed) */
qid_validate(c, ...);
qid_mark_consumed(c, ...);
qid_client_pool_release(pool, c);

/* boot 시 N 개 client 미리 생성 — TLS handshake / connection 비용을 정적
   pool 에 spread. acquire/release 는 mutex + condvar 으로 thread-safe. */
```

운영 size 선택:
- pool size = 동시 in-flight 주문 thread 수의 1.5x 권장 (적당한 여유).
- size 가 부족하면 `qid_client_pool_stats_t.contended` 가 증가 — 메트릭으로
  알림. 너무 크면 mci-price 측 connection 수 늘어남 (보통 문제 없음).

비-블록 변형:
```c
qid_client_t* c = qid_client_pool_try_acquire(pool);
if (!c) { /* pool 비었음 — fast reject 또는 fallback */ }
```

테스트 binary `test_pool` 가 pool size 4 + thread 16 × 호출 50 = 800 RPC
시나리오 — race / leak / 호출 누락 검증. `make test_pool` 후 mci-price
띄워두고 `./test_pool` 실행.

## qid_async_engine_t — curl_multi 비동기 (단일 thread pipelining)

pool 이 "N thread × 각자 sync" 라면 async engine 은 "1 thread × N in-flight"
모델. 단일 thread 안에서 여러 quote 를 동시 처리 (FIX session thread 가
batch validate 하면서 다른 작업 병행).

```c
qid_async_engine_t* eng = qid_async_engine_new(&opts);

/* 50 요청 즉시 submit — 0.4ms */
qid_async_t* h[50];
for (int i=0; i<50; i++) h[i] = qid_validate_async(eng, qids[i]);

/* 다른 작업 — worker thread 가 curl_multi 로 pipeline */
do_other_work();

/* 결과 수거 — wait + parse */
for (int i=0; i<50; i++) {
    qid_validate_result_t vr;
    if (qid_async_get_validate(h[i], &vr) == QID_OK) { ... }
    qid_async_free(h[i]);
}

qid_async_engine_free(eng);
```

또는 non-blocking polling:
```c
if (qid_async_is_done(h)) {
    qid_async_get_validate(h, &vr);
    qid_async_free(h);
}
```

내부:
- 하나의 `CURLM*` multi handle + 하나의 background worker thread.
- `qid_validate_async` 는 easy handle 셋업 + 큐 enqueue → worker 신호.
- worker 가 `curl_multi_perform` + `curl_multi_wait` 루프, 완료 시 handle
  의 cv signal.
- 호출자는 thread-safe `is_done` / `wait` / `get_*` API.

테스트 binary `test_async` 가 50 요청 submit + 50ms sleep + 결과 수거
시나리오 — pipelining 효과 확인. `make test_async` 후 mci-price
띄워두고 `./test_async` 실행.

### pool vs async — 선택 기준

| 조건 | 추천 |
|------|------|
| N order handler thread, 각자 1주문 = 1 RPC | **pool** (lock-free fast path) |
| 1 thread, batch 모드 N quote 동시 검증 | **async** |
| FIX NewOrderList 다건 한 묶음에 처리 | BatchValidate RPC + sync (서버측 fan-out) |
| pool 사용 중인데 갑자기 burst 발생 | async 병행 (다른 engine 인스턴스) |

## 에러 매핑

| `qid_err_t` | 의미 | engine 측 대응 |
|-------------|------|----------------|
| `QID_OK` | RPC 성공 — `status_t` 로 분기 | 정상 흐름 |
| `QID_ERR_TRANSPORT` | libcurl (네트워크 / TLS) | 신규 주문 거절 (fail-safe) |
| `QID_ERR_DENIED` | 403 (engine_id allowlist) | 운영 alert — cert 잘못, allowlist 누락 |
| `QID_ERR_BAD_REQUEST` | 400 (빈 quote_id 등) | 코드 버그 — 조사 |
| `QID_ERR_INTERNAL` | 500 (mci-price 측 Redis 등) | 신규 주문 거절 (fail-safe) |
| `QID_ERR_JSON` | 응답 파싱 실패 | mci-price 버전 불일치 — 운영 alert |
| `QID_ERR_HTTP` | 위 외 응답 | 조사 |

## TLS 권장 (운영)

mci-price 가 `--http-tls-cert / --http-tls-key / --http-tls-client-ca` 활성화한
환경에서는 client 도 mTLS 사용:

```c
opts.ca_file   = "/etc/wtg/wtg-ca.crt";   /* mci-price 서버 검증 */
opts.cert_file = "/etc/wtg/engine.crt";   /* 엔진 클라이언트 cert */
opts.key_file  = "/etc/wtg/engine.key";
```

dev 자체발급 cert 만 `insecure_skip_verify=true` — 운영 금지.

## 운영 모니터링

- mci-price 의 `/metrics` 에서 `wtg_quoteid_op_total{op="validate",...}` 추이
  를 보면 엔진 호출량 확인.
- mci-price 의 alert (Grafana Unified Alerting, `etc/grafana/quoteid-alerts.json`)
  중 `denied` 가 page 임 — 엔진 cert / engine_id 설정 오류 즉시 발견.

### 엔진 측 Prometheus 통합

C SDK 가 자체 stats 를 Prometheus exposition 텍스트로 출력 — 엔진팀이
자기 `/metrics` HTTP 응답에 그대로 붙이거나 pushgateway 로 push.

```c
char buf[2048];
size_t n = qid_client_pool_stats_text(pool, "matching-A", buf, sizeof(buf));
/* HTTP 응답에 buf 를 그대로 write — 또는 pushgateway 로 POST. */

size_t m = qid_async_engine_stats_text(eng, "matching-A", buf, sizeof(buf));
```

출력 metric:

```
qid_pool_size{service="matching-A"}           8          (gauge)
qid_pool_available{service="matching-A"}      6          (gauge)
qid_pool_acquires_total{service="matching-A"} 12345      (counter)
qid_pool_contended_total{service="matching-A"} 23        (counter)

qid_async_submits_total{service="matching-A"} 5000       (counter)
qid_async_completed_total{service="matching-A"} 4995     (counter)
qid_async_failed_total{service="matching-A"} 0           (counter)
qid_async_in_flight{service="matching-A"} 12             (gauge)
```

권장 alert 임계:
- `rate(qid_pool_contended_total[5m]) / rate(qid_pool_acquires_total[5m]) > 0.1` —
  pool saturation, size 늘릴 시점.
- `qid_pool_available == 0` 가 지속 — 즉시 size 증가.
- `rate(qid_async_failed_total[5m]) > 0.001` — TRANSPORT / 큐 포화.
- `qid_async_in_flight > 800` (MAX_INFLIGHT=1024 기준) — 곧 포화.

## v2 후속

- `qid_client_pool_t` — 다중 스레드 엔진 thread-safe pool.
- libcurl multi-handle 기반 비동기 변형 (`qid_validate_async`).
- BatchValidate / BatchMarkConsumed 의 in-flight 캐시 (cache hit 시 RPC 절약).
