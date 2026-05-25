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
- 다중 스레드 엔진은 **스레드별로 인스턴스 분리** 권장 (pool 패턴).
- v2 후속 — `qid_client_pool_t` 추상화 검토.

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

## v2 후속

- `qid_client_pool_t` — 다중 스레드 엔진 thread-safe pool.
- libcurl multi-handle 기반 비동기 변형 (`qid_validate_async`).
- BatchValidate / BatchMarkConsumed 의 in-flight 캐시 (cache hit 시 RPC 절약).
