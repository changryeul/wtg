/*
 * quoteid_client.h — WTG QuoteValidationService 의 C 클라이언트 헤더.
 *
 * WTG (Winway Trading Gateway) 의 QuoteID 검증 / 표시 API 를 매칭 엔진 (C) 이
 * 호출하기 위한 thin wrapper. gRPC 대신 HTTP REST (protojson) 사용 — libcurl +
 * cJSON 의존성만 필요해 gRPC-C++ (~수십 MB) 가 없는 trading-engine native
 * 환경에 적합.
 *
 * 대응 RPC (모두 /v1/quoteid/ prefix, 자세한 의미는 docs/quoteid-validation-rfc.md):
 *   POST /v1/quoteid/validate              → qid_validate
 *   POST /v1/quoteid/mark-consumed         → qid_mark_consumed
 *   POST /v1/quoteid/batch-validate        → qid_batch_validate
 *   POST /v1/quoteid/batch-mark-consumed   → qid_batch_mark_consumed
 *
 * 스레드 안전성: qid_client_t 인스턴스는 단일 스레드에서만 사용 권장
 * (libcurl easy handle 의 표준 정책). 다중 스레드는 인스턴스를 스레드별로
 * 분리해 사용한다. multi-handle 사용 모델은 v2 후속.
 *
 * 빌드:
 *   gcc -c quoteid_client.c -lcurl -lcjson
 *
 * 사용 흐름 (FIX NewOrderSingle handler 안):
 *   1. qid_client_new — 부팅 시 1회.
 *   2. qid_validate — engine 자체 정책 통과 시
 *   3. qid_mark_consumed — 체결 직전 atomic 표시
 *   4. fill or reject 분기
 */

#ifndef WTG_QUOTEID_CLIENT_H
#define WTG_QUOTEID_CLIENT_H

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ─── 결과 상태 ─────────────────────────────────────────────────────────── */

/*
 * qid_status_t — proto enum ValidationStatus 와 1:1 매칭.
 * docs/quoteid-validation-rfc.md §4.1 참조.
 */
typedef enum {
    QID_STATUS_UNSPECIFIED    = 0,
    QID_STATUS_OK             = 1,
    QID_STATUS_NOT_FOUND      = 2,
    QID_STATUS_EXPIRED        = 3,
    QID_STATUS_ALREADY_CONSUMED = 4
} qid_status_t;

/*
 * qid_err_t — RPC 호출 자체의 결과 (네트워크 / JSON 파싱 / HTTP 응답 등).
 * status_t 와 별도 — qid_err_t == QID_OK 일 때만 status_t 의미 있음.
 */
typedef enum {
    QID_OK             = 0,
    QID_ERR_TRANSPORT  = 1,  /* libcurl 오류 (네트워크 / TLS 등) */
    QID_ERR_HTTP       = 2,  /* HTTP 응답 코드가 2xx / 4xx 매핑된 status 외 */
    QID_ERR_JSON       = 3,  /* JSON 파싱 실패 */
    QID_ERR_DENIED     = 4,  /* 403 PermissionDenied (engine_id allowlist) */
    QID_ERR_BAD_REQUEST = 5, /* 400 (예: 빈 quote_id / batch 상한 초과) */
    QID_ERR_INTERNAL   = 6   /* 500 (mci-price 측 Registry / Redis 오류) */
} qid_err_t;

/* ─── 데이터 모델 ───────────────────────────────────────────────────────── */

/*
 * qid_record_t — 발행 시점의 권위 데이터. proto QuoteRecord 와 1:1.
 *
 * 모든 string 은 NUL terminated, 호출자가 buffer 를 제공 (필드 크기 cap).
 * len 필드는 호출자가 sizeof(buffer) 로 초기화 → 반환 시 실제 길이.
 * (cap 보다 길면 truncate + len 은 cap-1.)
 */
typedef struct {
    char quote_id[64];
    char pair[32];
    char channel[16];
    char site[16];
    char tier[16];
    char tenor[16];
    double bid;
    double ask;
    int64_t issued_unix_nano;
    int64_t valid_until_unix_nano;
    uint64_t sequence;
    char issuer[16];
} qid_record_t;

/*
 * qid_validate_result_t — Validate / BatchValidate 응답 1건.
 */
typedef struct {
    qid_status_t status;
    qid_record_t record;          /* status != NOT_FOUND 일 때 채워짐 */
    int32_t ord_rej_reason;       /* FIX tag 103 매핑. status != OK 일 때 의미 */
    char reject_text[128];
} qid_validate_result_t;

/*
 * qid_mark_result_t — MarkConsumed / BatchMarkConsumed 응답 1건.
 */
typedef struct {
    qid_status_t status;
    qid_record_t record;
    char consumed_by[64];          /* ALREADY_CONSUMED 시 먼저 잡은 consumer_id */
    int32_t ord_rej_reason;
    char reject_text[128];
} qid_mark_result_t;

/* ─── 클라이언트 생성 ───────────────────────────────────────────────────── */

typedef struct qid_client qid_client_t;

/*
 * qid_client_options — qid_client_new 옵션. NULL 인 옵션 field 는 default.
 *
 * mTLS 권장 (운영 mci-price 의 --http-tls-client-ca 와 짝). dev 환경 (자체발급)
 * 만 insecure_skip_verify=true 허용.
 */
typedef struct {
    const char* base_url;          /* "https://mci-price.internal:8443" (필수) */
    const char* engine_id;         /* allowlist 등록 식별자 — 모든 호출에 동봉 */

    /* TLS 옵션 (모두 NULL 이면 plain HTTP). */
    const char* ca_file;           /* 서버 인증서 검증용 CA bundle PEM */
    const char* cert_file;         /* mTLS 클라이언트 cert (CA + key 매칭) */
    const char* key_file;          /* mTLS 클라이언트 key */
    bool insecure_skip_verify;     /* dev 만. 운영 금지 */

    long timeout_ms;               /* default 1000ms */
    long connect_timeout_ms;       /* default 500ms */
} qid_client_options_t;

/*
 * qid_client_new — 클라이언트 생성. 실패 시 NULL 반환.
 * libcurl_global_init 은 호출자가 외부에서 1회 호출 책임 — multi-threaded
 * 환경의 표준 패턴.
 */
qid_client_t* qid_client_new(const qid_client_options_t* opts);

/*
 * qid_client_free — 클라이언트 자원 해제.
 */
void qid_client_free(qid_client_t* c);

/* ─── RPC ──────────────────────────────────────────────────────────────── */

/*
 * qid_validate — 단일 QuoteID 검증 (read-only).
 *
 * out 은 호출자가 stack 또는 heap 으로 제공. 반환이 QID_OK 면 out->status
 * 로 분기.
 *
 * 호출 예 (FIX NewOrderSingle handler 안):
 *   qid_validate_result_t r;
 *   qid_err_t err = qid_validate(client, fix_tag117_quoteid, &r);
 *   if (err == QID_OK && r.status == QID_STATUS_OK) {
 *       // engine 자체 정책 (slippage / side / tier) 검증
 *   } else if (err == QID_OK && r.status == QID_STATUS_EXPIRED) {
 *       // OrdRejReason = r.ord_rej_reason  (= 13, Stale)
 *   }
 */
qid_err_t qid_validate(qid_client_t* c,
                       const char* quote_id,
                       qid_validate_result_t* out);

/*
 * qid_mark_consumed — atomic 사용 표시 (write).
 *
 * consumer_id 는 보통 엔진의 OrderID — audit 추적용. 동시 호출 atomic 보장은
 * 서버 측 (Lua script). 정확히 한 호출만 QID_STATUS_OK.
 *
 * 호출 예 (qid_validate 후 engine 정책 통과 + 체결 직전):
 *   qid_mark_result_t r;
 *   qid_err_t err = qid_mark_consumed(client, qid, order_id, &r);
 *   if (err == QID_OK && r.status == QID_STATUS_OK) {
 *       // fill
 *   } else if (r.status == QID_STATUS_ALREADY_CONSUMED) {
 *       // race 충돌 — OrdRejReason=6, consumed_by 가 먼저 잡은 OrderID
 *   }
 */
qid_err_t qid_mark_consumed(qid_client_t* c,
                            const char* quote_id,
                            const char* consumer_id,
                            qid_mark_result_t* out);

/*
 * qid_batch_validate — 다건 QuoteID 검증 (FIX NewOrderList 사전 검증).
 *
 * count <= 1000. 호출자가 out_results 를 count 크기로 미리 할당.
 * out_count_returned 는 실제 응답에 포함된 항목 수 (보통 count 와 동일,
 * 서버 에러 시 0).
 */
qid_err_t qid_batch_validate(qid_client_t* c,
                             const char* const* quote_ids,
                             size_t count,
                             qid_validate_result_t* out_results,
                             size_t* out_count_returned);

/*
 * qid_batch_mark_consumed — 다건 표시. items 와 consumers 는 parallel array.
 */
qid_err_t qid_batch_mark_consumed(qid_client_t* c,
                                  const char* const* quote_ids,
                                  const char* const* consumer_ids,
                                  size_t count,
                                  qid_mark_result_t* out_results,
                                  size_t* out_count_returned);

/* ─── 헬퍼 ─────────────────────────────────────────────────────────────── */

/*
 * qid_status_name — 디버깅 / 로그용. static 문자열, free 금지.
 */
const char* qid_status_name(qid_status_t s);

/*
 * qid_err_name — 동일.
 */
const char* qid_err_name(qid_err_t e);

/* ─── Pool — multi-threaded 엔진용 ────────────────────────────────────── */

/*
 * qid_client_pool_t — 고정 크기 client pool. libcurl easy handle 의 단일
 * 스레드 규칙 회피용. 매칭 엔진의 N order handler thread 가 동시에 사용
 * 가능하도록 N 개 (또는 그 이상의 size) client 를 미리 생성.
 *
 * 흐름:
 *
 *   pool 생성 (boot 1회)
 *      ↓
 *   thread 안에서:
 *      c = pool_acquire(pool)  ← block 가능
 *      qid_validate(c, ...)
 *      qid_mark_consumed(c, ...)
 *      pool_release(pool, c)
 *
 * pool 의 free list 가 비어있으면 acquire 는 다른 thread 가 release 할
 * 때까지 block. try_acquire 는 즉시 NULL.
 *
 * 통계는 qid_client_pool_stats — 운영 모니터링용.
 */
typedef struct qid_client_pool qid_client_pool_t;

/*
 * qid_client_pool_new — size 만큼의 client 를 미리 생성. opts 는 모든
 * client 에 동일 적용 (mTLS cert / base_url 등). 실패 시 NULL.
 */
qid_client_pool_t* qid_client_pool_new(const qid_client_options_t* opts,
                                       size_t size);

/*
 * qid_client_pool_free — 모든 in-use client 가 release 된 후 호출 권장.
 * 호출 시점에 in-use 가 있어도 pool 자원은 해제되지만, 그 client 를 쓰는
 * thread 는 use-after-free 위험. graceful shutdown 절차는 호출자 책임.
 */
void qid_client_pool_free(qid_client_pool_t* pool);

/*
 * qid_client_pool_acquire — pool 에서 client 하나 받음.
 * 비어있으면 다른 thread 의 release 까지 block. pool 이 closed 면 NULL.
 */
qid_client_t* qid_client_pool_acquire(qid_client_pool_t* pool);

/*
 * qid_client_pool_try_acquire — non-blocking. pool 비어있으면 즉시 NULL.
 */
qid_client_t* qid_client_pool_try_acquire(qid_client_pool_t* pool);

/*
 * qid_client_pool_release — acquire 한 client 반환. 다른 thread 가 곧
 * 사용 가능.
 */
void qid_client_pool_release(qid_client_pool_t* pool, qid_client_t* c);

/*
 * qid_client_pool_stats_t — 운영 모니터링 카운터 snapshot.
 */
typedef struct {
    size_t size;          /* 총 client 수 */
    size_t available;     /* 현재 free (acquire 가능) 수 */
    uint64_t acquires;    /* 누적 acquire 호출 */
    uint64_t contended;   /* 누적 — block 해야 했던 acquire (saturation 지표) */
} qid_client_pool_stats_t;

/*
 * qid_client_pool_stats — 현재 카운터 snapshot.
 */
qid_client_pool_stats_t qid_client_pool_stats(const qid_client_pool_t* pool);

#ifdef __cplusplus
}
#endif

#endif /* WTG_QUOTEID_CLIENT_H */
