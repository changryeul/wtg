/*
 * quoteid_client.c — quoteid_client.h 의 구현.
 *
 * 의존성: libcurl (HTTP), cJSON (JSON 인코딩 / 파싱).
 * 빌드:    gcc -c quoteid_client.c -o quoteid_client.o
 *          gcc example.c quoteid_client.o -lcurl -lcjson -o example
 *
 * 설계 메모:
 *   - libcurl easy handle 을 재사용 (connection pooling) — qid_client_t 안에 영구 보관.
 *   - JSON body 는 cJSON 으로 빌드, 응답은 동일 lib 으로 파싱. 호출 단위
 *     malloc 이 있지만 RPC 비용 (네트워크 RTT) 대비 무시할 수준.
 *   - 호출자 buffer (qid_record_t 등) 는 strncpy + NUL 보장.
 *   - HTTP 응답 code 매핑:
 *       200 → 응답 body 파싱
 *       400 → QID_ERR_BAD_REQUEST
 *       403 → QID_ERR_DENIED
 *       404 → QID_ERR_HTTP (raw QuoteID GET 외 routes 에서는 보통 안 나옴)
 *       500+ → QID_ERR_INTERNAL
 *       기타 → QID_ERR_HTTP
 */

#include "quoteid_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <pthread.h>

#include <curl/curl.h>
#include <cjson/cJSON.h>

struct qid_client {
    CURL* curl;
    char* base_url;
    char* engine_id;
    struct curl_slist* headers;  /* Content-Type: application/json */
};

/* ─── 내부 buffer (libcurl write callback) ──────────────────────────────── */

typedef struct {
    char* data;
    size_t len;
    size_t cap;
} qid_buf_t;

static size_t qid_write_cb(void* ptr, size_t size, size_t nmemb, void* userdata) {
    qid_buf_t* b = (qid_buf_t*)userdata;
    size_t add = size * nmemb;
    size_t need = b->len + add + 1;
    if (need > b->cap) {
        size_t cap = b->cap ? b->cap : 4096;
        while (cap < need) cap *= 2;
        char* p = (char*)realloc(b->data, cap);
        if (!p) return 0;
        b->data = p;
        b->cap = cap;
    }
    memcpy(b->data + b->len, ptr, add);
    b->len += add;
    b->data[b->len] = '\0';
    return add;
}

static void qid_buf_free(qid_buf_t* b) {
    free(b->data);
    b->data = NULL;
    b->len = 0;
    b->cap = 0;
}

/* ─── 헬퍼 — 문자열 safe copy ────────────────────────────────────────────── */

static void copy_str(char* dst, size_t cap, const char* src) {
    if (cap == 0) return;
    if (!src) { dst[0] = '\0'; return; }
    size_t n = strlen(src);
    if (n >= cap) n = cap - 1;
    memcpy(dst, src, n);
    dst[n] = '\0';
}

/* JSON 에서 string field 추출 (없으면 빈 문자열). */
static void copy_jstring(char* dst, size_t cap, const cJSON* obj, const char* key) {
    const cJSON* v = cJSON_GetObjectItemCaseSensitive(obj, key);
    if (cJSON_IsString(v) && v->valuestring) {
        copy_str(dst, cap, v->valuestring);
    } else {
        if (cap > 0) dst[0] = '\0';
    }
}

static double j_double(const cJSON* obj, const char* key) {
    const cJSON* v = cJSON_GetObjectItemCaseSensitive(obj, key);
    if (cJSON_IsNumber(v)) return v->valuedouble;
    return 0.0;
}

static int64_t j_i64(const cJSON* obj, const char* key) {
    const cJSON* v = cJSON_GetObjectItemCaseSensitive(obj, key);
    if (cJSON_IsNumber(v)) return (int64_t)v->valuedouble;
    return 0;
}

static uint64_t j_u64(const cJSON* obj, const char* key) {
    const cJSON* v = cJSON_GetObjectItemCaseSensitive(obj, key);
    if (cJSON_IsNumber(v)) return (uint64_t)v->valuedouble;
    return 0;
}

static int32_t j_i32(const cJSON* obj, const char* key) {
    return (int32_t)j_i64(obj, key);
}

/* status string ("OK" / "NOT_FOUND" / ...) → enum. proto3 의 default
   encoding 은 enum name 그대로. EmitDefaultValues 옵션도 동일. */
static qid_status_t parse_status(const cJSON* obj) {
    const cJSON* v = cJSON_GetObjectItemCaseSensitive(obj, "status");
    if (!cJSON_IsString(v) || !v->valuestring) return QID_STATUS_UNSPECIFIED;
    const char* s = v->valuestring;
    if (strcmp(s, "OK") == 0)                 return QID_STATUS_OK;
    if (strcmp(s, "NOT_FOUND") == 0)          return QID_STATUS_NOT_FOUND;
    if (strcmp(s, "EXPIRED") == 0)            return QID_STATUS_EXPIRED;
    if (strcmp(s, "ALREADY_CONSUMED") == 0)   return QID_STATUS_ALREADY_CONSUMED;
    return QID_STATUS_UNSPECIFIED;
}

/* JSON QuoteRecord → C 구조. */
static void parse_record(const cJSON* obj, qid_record_t* out) {
    memset(out, 0, sizeof(*out));
    const cJSON* rec = cJSON_GetObjectItemCaseSensitive(obj, "record");
    if (!cJSON_IsObject(rec)) return;
    copy_jstring(out->quote_id, sizeof(out->quote_id), rec, "quoteId");
    copy_jstring(out->pair,     sizeof(out->pair),     rec, "pair");
    copy_jstring(out->channel,  sizeof(out->channel),  rec, "channel");
    copy_jstring(out->site,     sizeof(out->site),     rec, "site");
    copy_jstring(out->tier,     sizeof(out->tier),     rec, "tier");
    copy_jstring(out->tenor,    sizeof(out->tenor),    rec, "tenor");
    copy_jstring(out->issuer,   sizeof(out->issuer),   rec, "issuer");
    out->bid                   = j_double(rec, "bid");
    out->ask                   = j_double(rec, "ask");
    out->issued_unix_nano      = j_i64(rec,    "issuedUnixNano");
    out->valid_until_unix_nano = j_i64(rec,    "validUntilUnixNano");
    out->sequence              = j_u64(rec,    "sequence");
}

/* ─── HTTP 호출 (공통) ──────────────────────────────────────────────────── */

static qid_err_t map_http_code(long code) {
    if (code >= 200 && code < 300) return QID_OK;
    if (code == 400) return QID_ERR_BAD_REQUEST;
    if (code == 403) return QID_ERR_DENIED;
    if (code >= 500) return QID_ERR_INTERNAL;
    return QID_ERR_HTTP;
}

static qid_err_t do_post_json(qid_client_t* c, const char* path,
                              const char* body, qid_buf_t* resp,
                              long* http_code_out) {
    char url[1024];
    snprintf(url, sizeof(url), "%s%s", c->base_url, path);
    curl_easy_setopt(c->curl, CURLOPT_URL, url);
    curl_easy_setopt(c->curl, CURLOPT_HTTPHEADER, c->headers);
    curl_easy_setopt(c->curl, CURLOPT_POSTFIELDS, body);
    curl_easy_setopt(c->curl, CURLOPT_POSTFIELDSIZE, (long)strlen(body));
    curl_easy_setopt(c->curl, CURLOPT_WRITEFUNCTION, qid_write_cb);
    curl_easy_setopt(c->curl, CURLOPT_WRITEDATA, resp);
    CURLcode cc = curl_easy_perform(c->curl);
    if (cc != CURLE_OK) {
        return QID_ERR_TRANSPORT;
    }
    long code = 0;
    curl_easy_getinfo(c->curl, CURLINFO_RESPONSE_CODE, &code);
    if (http_code_out) *http_code_out = code;
    return map_http_code(code);
}

/* ─── 클라이언트 생성 / 해제 ─────────────────────────────────────────────── */

qid_client_t* qid_client_new(const qid_client_options_t* opts) {
    if (!opts || !opts->base_url) return NULL;
    qid_client_t* c = (qid_client_t*)calloc(1, sizeof(*c));
    if (!c) return NULL;
    c->curl = curl_easy_init();
    if (!c->curl) { free(c); return NULL; }
    c->base_url  = strdup(opts->base_url);
    c->engine_id = strdup(opts->engine_id ? opts->engine_id : "");
    c->headers = curl_slist_append(NULL, "Content-Type: application/json");

    long timeout = opts->timeout_ms > 0 ? opts->timeout_ms : 1000;
    long connto  = opts->connect_timeout_ms > 0 ? opts->connect_timeout_ms : 500;
    curl_easy_setopt(c->curl, CURLOPT_TIMEOUT_MS, timeout);
    curl_easy_setopt(c->curl, CURLOPT_CONNECTTIMEOUT_MS, connto);
    curl_easy_setopt(c->curl, CURLOPT_NOSIGNAL, 1L);
    curl_easy_setopt(c->curl, CURLOPT_FORBID_REUSE, 0L);  /* connection 재사용 */

    if (opts->ca_file)   curl_easy_setopt(c->curl, CURLOPT_CAINFO,  opts->ca_file);
    if (opts->cert_file) curl_easy_setopt(c->curl, CURLOPT_SSLCERT, opts->cert_file);
    if (opts->key_file)  curl_easy_setopt(c->curl, CURLOPT_SSLKEY,  opts->key_file);
    if (opts->insecure_skip_verify) {
        curl_easy_setopt(c->curl, CURLOPT_SSL_VERIFYPEER, 0L);
        curl_easy_setopt(c->curl, CURLOPT_SSL_VERIFYHOST, 0L);
    }
    return c;
}

void qid_client_free(qid_client_t* c) {
    if (!c) return;
    if (c->curl) curl_easy_cleanup(c->curl);
    if (c->headers) curl_slist_free_all(c->headers);
    free(c->base_url);
    free(c->engine_id);
    free(c);
}

/* ─── Validate ─────────────────────────────────────────────────────────── */

static int64_t now_unix_nano(void) {
    struct timespec ts;
#ifdef CLOCK_REALTIME
    clock_gettime(CLOCK_REALTIME, &ts);
#else
    ts.tv_sec = time(NULL); ts.tv_nsec = 0;
#endif
    return (int64_t)ts.tv_sec * 1000000000LL + (int64_t)ts.tv_nsec;
}

qid_err_t qid_validate(qid_client_t* c,
                       const char* quote_id,
                       qid_validate_result_t* out) {
    if (!c || !quote_id || !out) return QID_ERR_BAD_REQUEST;
    memset(out, 0, sizeof(*out));

    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "quoteId", quote_id);
    cJSON_AddStringToObject(root, "engineId", c->engine_id);
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    char* body = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (!body) return QID_ERR_INTERNAL;

    qid_buf_t resp = {0};
    long code = 0;
    qid_err_t err = do_post_json(c, "/v1/quoteid/validate", body, &resp, &code);
    free(body);
    if (err != QID_OK) { qid_buf_free(&resp); return err; }

    cJSON* j = cJSON_Parse(resp.data ? resp.data : "");
    qid_buf_free(&resp);
    if (!j) return QID_ERR_JSON;

    out->status = parse_status(j);
    parse_record(j, &out->record);
    out->ord_rej_reason = j_i32(j, "ordRejReason");
    copy_jstring(out->reject_text, sizeof(out->reject_text), j, "rejectText");
    cJSON_Delete(j);
    return QID_OK;
}

/* ─── MarkConsumed ─────────────────────────────────────────────────────── */

qid_err_t qid_mark_consumed(qid_client_t* c,
                            const char* quote_id,
                            const char* consumer_id,
                            qid_mark_result_t* out) {
    if (!c || !quote_id || !consumer_id || !out) return QID_ERR_BAD_REQUEST;
    memset(out, 0, sizeof(*out));

    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "quoteId", quote_id);
    cJSON_AddStringToObject(root, "consumerId", consumer_id);
    cJSON_AddStringToObject(root, "engineId", c->engine_id);
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    char* body = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (!body) return QID_ERR_INTERNAL;

    qid_buf_t resp = {0};
    long code = 0;
    qid_err_t err = do_post_json(c, "/v1/quoteid/mark-consumed", body, &resp, &code);
    free(body);
    if (err != QID_OK) { qid_buf_free(&resp); return err; }

    cJSON* j = cJSON_Parse(resp.data ? resp.data : "");
    qid_buf_free(&resp);
    if (!j) return QID_ERR_JSON;

    out->status = parse_status(j);
    parse_record(j, &out->record);
    copy_jstring(out->consumed_by, sizeof(out->consumed_by), j, "consumedBy");
    out->ord_rej_reason = j_i32(j, "ordRejReason");
    copy_jstring(out->reject_text, sizeof(out->reject_text), j, "rejectText");
    cJSON_Delete(j);
    return QID_OK;
}

/* ─── BatchValidate ────────────────────────────────────────────────────── */

qid_err_t qid_batch_validate(qid_client_t* c,
                             const char* const* quote_ids,
                             size_t count,
                             qid_validate_result_t* out_results,
                             size_t* out_count_returned) {
    if (!c || !out_results) return QID_ERR_BAD_REQUEST;
    if (out_count_returned) *out_count_returned = 0;
    if (count == 0) return QID_OK;

    cJSON* root = cJSON_CreateObject();
    cJSON* arr = cJSON_AddArrayToObject(root, "quoteIds");
    for (size_t i = 0; i < count; i++) {
        cJSON_AddItemToArray(arr, cJSON_CreateString(quote_ids[i] ? quote_ids[i] : ""));
    }
    cJSON_AddStringToObject(root, "engineId", c->engine_id);
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    char* body = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (!body) return QID_ERR_INTERNAL;

    qid_buf_t resp = {0};
    long code = 0;
    qid_err_t err = do_post_json(c, "/v1/quoteid/batch-validate", body, &resp, &code);
    free(body);
    if (err != QID_OK) { qid_buf_free(&resp); return err; }

    cJSON* j = cJSON_Parse(resp.data ? resp.data : "");
    qid_buf_free(&resp);
    if (!j) return QID_ERR_JSON;

    cJSON* results = cJSON_GetObjectItemCaseSensitive(j, "results");
    size_t n = (size_t)cJSON_GetArraySize(results);
    if (n > count) n = count;
    for (size_t i = 0; i < n; i++) {
        cJSON* item = cJSON_GetArrayItem(results, (int)i);
        if (!item) continue;
        memset(&out_results[i], 0, sizeof(out_results[i]));
        out_results[i].status = parse_status(item);
        parse_record(item, &out_results[i].record);
        out_results[i].ord_rej_reason = j_i32(item, "ordRejReason");
        copy_jstring(out_results[i].reject_text, sizeof(out_results[i].reject_text),
                     item, "rejectText");
    }
    if (out_count_returned) *out_count_returned = n;
    cJSON_Delete(j);
    return QID_OK;
}

/* ─── BatchMarkConsumed ────────────────────────────────────────────────── */

qid_err_t qid_batch_mark_consumed(qid_client_t* c,
                                  const char* const* quote_ids,
                                  const char* const* consumer_ids,
                                  size_t count,
                                  qid_mark_result_t* out_results,
                                  size_t* out_count_returned) {
    if (!c || !out_results) return QID_ERR_BAD_REQUEST;
    if (out_count_returned) *out_count_returned = 0;
    if (count == 0) return QID_OK;

    cJSON* root = cJSON_CreateObject();
    cJSON* items = cJSON_AddArrayToObject(root, "items");
    for (size_t i = 0; i < count; i++) {
        cJSON* it = cJSON_CreateObject();
        cJSON_AddStringToObject(it, "quoteId",    quote_ids[i]    ? quote_ids[i]    : "");
        cJSON_AddStringToObject(it, "consumerId", consumer_ids[i] ? consumer_ids[i] : "");
        cJSON_AddItemToArray(items, it);
    }
    cJSON_AddStringToObject(root, "engineId", c->engine_id);
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    char* body = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (!body) return QID_ERR_INTERNAL;

    qid_buf_t resp = {0};
    long code = 0;
    qid_err_t err = do_post_json(c, "/v1/quoteid/batch-mark-consumed", body, &resp, &code);
    free(body);
    if (err != QID_OK) { qid_buf_free(&resp); return err; }

    cJSON* j = cJSON_Parse(resp.data ? resp.data : "");
    qid_buf_free(&resp);
    if (!j) return QID_ERR_JSON;

    cJSON* results = cJSON_GetObjectItemCaseSensitive(j, "results");
    size_t n = (size_t)cJSON_GetArraySize(results);
    if (n > count) n = count;
    for (size_t i = 0; i < n; i++) {
        cJSON* item = cJSON_GetArrayItem(results, (int)i);
        if (!item) continue;
        memset(&out_results[i], 0, sizeof(out_results[i]));
        out_results[i].status = parse_status(item);
        parse_record(item, &out_results[i].record);
        copy_jstring(out_results[i].consumed_by, sizeof(out_results[i].consumed_by),
                     item, "consumedBy");
        out_results[i].ord_rej_reason = j_i32(item, "ordRejReason");
        copy_jstring(out_results[i].reject_text, sizeof(out_results[i].reject_text),
                     item, "rejectText");
    }
    if (out_count_returned) *out_count_returned = n;
    cJSON_Delete(j);
    return QID_OK;
}

/* ─── 헬퍼 ─────────────────────────────────────────────────────────────── */

const char* qid_status_name(qid_status_t s) {
    switch (s) {
    case QID_STATUS_OK:               return "OK";
    case QID_STATUS_NOT_FOUND:        return "NOT_FOUND";
    case QID_STATUS_EXPIRED:          return "EXPIRED";
    case QID_STATUS_ALREADY_CONSUMED: return "ALREADY_CONSUMED";
    default:                          return "UNSPECIFIED";
    }
}

const char* qid_err_name(qid_err_t e) {
    switch (e) {
    case QID_OK:               return "OK";
    case QID_ERR_TRANSPORT:    return "TRANSPORT";
    case QID_ERR_HTTP:         return "HTTP";
    case QID_ERR_JSON:         return "JSON";
    case QID_ERR_DENIED:       return "DENIED";
    case QID_ERR_BAD_REQUEST:  return "BAD_REQUEST";
    case QID_ERR_INTERNAL:     return "INTERNAL";
    default:                   return "UNKNOWN";
    }
}

/* ─── Pool — multi-threaded 엔진용 ────────────────────────────────────── */

struct qid_client_pool {
    qid_client_t** all;     /* 전체 client array (lifecycle 보관) */
    qid_client_t** free;    /* free stack — top = free[free_n-1] */
    size_t cap;
    size_t free_n;
    pthread_mutex_t mu;
    pthread_cond_t  cv;     /* free 가 비어있을 때 wait */
    int             closed;
    /* 카운터 */
    uint64_t acquires;
    uint64_t contended;
};

qid_client_pool_t* qid_client_pool_new(const qid_client_options_t* opts, size_t size) {
    if (!opts || size == 0) return NULL;
    qid_client_pool_t* p = (qid_client_pool_t*)calloc(1, sizeof(*p));
    if (!p) return NULL;
    p->all  = (qid_client_t**)calloc(size, sizeof(qid_client_t*));
    p->free = (qid_client_t**)calloc(size, sizeof(qid_client_t*));
    if (!p->all || !p->free) {
        free(p->all); free(p->free); free(p);
        return NULL;
    }
    p->cap = size;
    /* N 개 client 사전 생성. 1 개라도 실패하면 전체 rollback. */
    for (size_t i = 0; i < size; i++) {
        qid_client_t* c = qid_client_new(opts);
        if (!c) {
            for (size_t j = 0; j < i; j++) qid_client_free(p->all[j]);
            free(p->all); free(p->free); free(p);
            return NULL;
        }
        p->all[i] = c;
        p->free[i] = c;
    }
    p->free_n = size;
    pthread_mutex_init(&p->mu, NULL);
    pthread_cond_init(&p->cv, NULL);
    return p;
}

void qid_client_pool_free(qid_client_pool_t* pool) {
    if (!pool) return;
    pthread_mutex_lock(&pool->mu);
    pool->closed = 1;
    pthread_cond_broadcast(&pool->cv);  /* acquire 대기 중인 thread 깨움 */
    pthread_mutex_unlock(&pool->mu);

    /* 모든 client 자원 해제 — 호출자가 in-use client 를 더 안 쓴다고 가정. */
    for (size_t i = 0; i < pool->cap; i++) {
        if (pool->all[i]) qid_client_free(pool->all[i]);
    }
    pthread_mutex_destroy(&pool->mu);
    pthread_cond_destroy(&pool->cv);
    free(pool->all);
    free(pool->free);
    free(pool);
}

qid_client_t* qid_client_pool_acquire(qid_client_pool_t* pool) {
    if (!pool) return NULL;
    pthread_mutex_lock(&pool->mu);
    pool->acquires++;
    int waited = 0;
    while (pool->free_n == 0 && !pool->closed) {
        waited = 1;
        pthread_cond_wait(&pool->cv, &pool->mu);
    }
    if (waited) pool->contended++;
    if (pool->closed) {
        pthread_mutex_unlock(&pool->mu);
        return NULL;
    }
    qid_client_t* c = pool->free[--pool->free_n];
    pthread_mutex_unlock(&pool->mu);
    return c;
}

qid_client_t* qid_client_pool_try_acquire(qid_client_pool_t* pool) {
    if (!pool) return NULL;
    pthread_mutex_lock(&pool->mu);
    pool->acquires++;
    if (pool->free_n == 0 || pool->closed) {
        if (pool->free_n == 0) pool->contended++;
        pthread_mutex_unlock(&pool->mu);
        return NULL;
    }
    qid_client_t* c = pool->free[--pool->free_n];
    pthread_mutex_unlock(&pool->mu);
    return c;
}

void qid_client_pool_release(qid_client_pool_t* pool, qid_client_t* c) {
    if (!pool || !c) return;
    pthread_mutex_lock(&pool->mu);
    if (pool->free_n < pool->cap) {
        pool->free[pool->free_n++] = c;
        pthread_cond_signal(&pool->cv);
    }
    /* free_n == cap 이면 호출자 버그 (이중 release) — 묵음 무시 */
    pthread_mutex_unlock(&pool->mu);
}

/* ─── Async engine (curl_multi 기반) ──────────────────────────────────── */

enum {
    QID_ASYNC_VALIDATE = 1,
    QID_ASYNC_MARK     = 2,
};

struct qid_async {
    int kind;
    /* 요청 */
    char* url;           /* full URL */
    char* body;          /* JSON body */
    /* libcurl */
    CURL* easy;
    struct curl_slist* headers;
    qid_buf_t resp;
    long http_code;
    /* 결과 */
    int done;
    qid_err_t err;
    pthread_mutex_t mu;
    pthread_cond_t  cv;
    /* engine 연결 — 큐 chain */
    qid_async_engine_t* eng;
    struct qid_async* next;
};

struct qid_async_engine {
    CURLM* multi;
    char* base_url;
    char* engine_id;
    /* TLS 옵션 — 모든 easy handle 에 복사 적용 */
    char* ca_file;
    char* cert_file;
    char* key_file;
    int insecure_skip_verify;
    long timeout_ms;
    long connect_timeout_ms;
    /* worker */
    pthread_t worker;
    pthread_mutex_t mu;
    pthread_cond_t  cv;        /* submit 도착 or close 신호 */
    int closed;
    /* submit queue (worker 가 multi 에 add 하기 전 대기) */
    qid_async_t* head;
    qid_async_t* tail;
};

static char* str_dup_or_empty(const char* s) {
    return strdup(s ? s : "");
}

static void async_set_done(qid_async_t* h, qid_err_t err) {
    pthread_mutex_lock(&h->mu);
    h->err = err;
    h->done = 1;
    pthread_cond_broadcast(&h->cv);
    pthread_mutex_unlock(&h->mu);
}

static void* async_worker(void* arg) {
    qid_async_engine_t* eng = (qid_async_engine_t*)arg;
    /* 활성 handles map — easy handle → qid_async_t */
    /* 작은 규모 가정 (운영 in-flight ~100). 선형 검색이 cache-friendly. */
    enum { MAX_INFLIGHT = 1024 };
    qid_async_t* slots[MAX_INFLIGHT] = {0};
    int slot_count = 0;

    while (1) {
        /* 1) submit 큐를 multi 에 add (closed 이면 즉시 break). */
        pthread_mutex_lock(&eng->mu);
        while (!eng->head && !eng->closed && slot_count == 0) {
            pthread_cond_wait(&eng->cv, &eng->mu);
        }
        if (eng->closed && !eng->head && slot_count == 0) {
            pthread_mutex_unlock(&eng->mu);
            break;
        }
        qid_async_t* pending = eng->head;
        eng->head = eng->tail = NULL;
        pthread_mutex_unlock(&eng->mu);

        while (pending) {
            qid_async_t* h = pending;
            pending = pending->next;
            h->next = NULL;
            if (slot_count < MAX_INFLIGHT) {
                curl_multi_add_handle(eng->multi, h->easy);
                slots[slot_count++] = h;
            } else {
                async_set_done(h, QID_ERR_INTERNAL); /* engine 포화 — 드물게 */
            }
        }

        /* 2) curl_multi 진행 + 완료된 handle 처리. */
        int still_running = 0;
        curl_multi_perform(eng->multi, &still_running);

        /* 짧은 timeout — submit 신호도 polling. */
        struct curl_waitfd extra_fds = {0};
        (void)extra_fds;
        int numfds = 0;
        curl_multi_wait(eng->multi, NULL, 0, 50, &numfds);
        curl_multi_perform(eng->multi, &still_running);

        int msgs_left = 0;
        CURLMsg* msg;
        while ((msg = curl_multi_info_read(eng->multi, &msgs_left)) != NULL) {
            if (msg->msg != CURLMSG_DONE) continue;
            CURL* easy = msg->easy_handle;
            qid_async_t* h = NULL;
            int idx = -1;
            for (int i = 0; i < slot_count; i++) {
                if (slots[i] && slots[i]->easy == easy) {
                    h = slots[i];
                    idx = i;
                    break;
                }
            }
            if (!h) continue;
            curl_multi_remove_handle(eng->multi, easy);
            qid_err_t err;
            if (msg->data.result != CURLE_OK) {
                err = QID_ERR_TRANSPORT;
            } else {
                long code = 0;
                curl_easy_getinfo(easy, CURLINFO_RESPONSE_CODE, &code);
                h->http_code = code;
                err = map_http_code(code);
            }
            async_set_done(h, err);
            slots[idx] = slots[--slot_count];
        }
    }

    /* close — 남은 in-flight 모두 TRANSPORT 로 신호. */
    for (int i = 0; i < slot_count; i++) {
        qid_async_t* h = slots[i];
        if (!h) continue;
        curl_multi_remove_handle(eng->multi, h->easy);
        async_set_done(h, QID_ERR_TRANSPORT);
    }
    return NULL;
}

qid_async_engine_t* qid_async_engine_new(const qid_client_options_t* opts) {
    if (!opts || !opts->base_url) return NULL;
    qid_async_engine_t* e = (qid_async_engine_t*)calloc(1, sizeof(*e));
    if (!e) return NULL;
    e->multi = curl_multi_init();
    if (!e->multi) { free(e); return NULL; }
    e->base_url  = str_dup_or_empty(opts->base_url);
    e->engine_id = str_dup_or_empty(opts->engine_id);
    e->ca_file   = opts->ca_file   ? str_dup_or_empty(opts->ca_file)   : NULL;
    e->cert_file = opts->cert_file ? str_dup_or_empty(opts->cert_file) : NULL;
    e->key_file  = opts->key_file  ? str_dup_or_empty(opts->key_file)  : NULL;
    e->insecure_skip_verify = opts->insecure_skip_verify ? 1 : 0;
    e->timeout_ms = opts->timeout_ms > 0 ? opts->timeout_ms : 1000;
    e->connect_timeout_ms = opts->connect_timeout_ms > 0 ? opts->connect_timeout_ms : 500;
    pthread_mutex_init(&e->mu, NULL);
    pthread_cond_init(&e->cv, NULL);
    if (pthread_create(&e->worker, NULL, async_worker, e) != 0) {
        curl_multi_cleanup(e->multi);
        pthread_mutex_destroy(&e->mu);
        pthread_cond_destroy(&e->cv);
        free(e->base_url); free(e->engine_id);
        free(e->ca_file); free(e->cert_file); free(e->key_file);
        free(e);
        return NULL;
    }
    return e;
}

void qid_async_engine_free(qid_async_engine_t* eng) {
    if (!eng) return;
    pthread_mutex_lock(&eng->mu);
    eng->closed = 1;
    pthread_cond_broadcast(&eng->cv);
    pthread_mutex_unlock(&eng->mu);
    pthread_join(eng->worker, NULL);
    curl_multi_cleanup(eng->multi);
    pthread_mutex_destroy(&eng->mu);
    pthread_cond_destroy(&eng->cv);
    free(eng->base_url); free(eng->engine_id);
    free(eng->ca_file); free(eng->cert_file); free(eng->key_file);
    free(eng);
}

/* submit 헬퍼 — easy handle 셋업 + 큐 enqueue. */
static qid_async_t* async_submit(qid_async_engine_t* eng,
                                 int kind,
                                 const char* path,
                                 cJSON* body_obj) {
    if (!eng) { cJSON_Delete(body_obj); return NULL; }
    qid_async_t* h = (qid_async_t*)calloc(1, sizeof(*h));
    if (!h) { cJSON_Delete(body_obj); return NULL; }
    h->kind = kind;
    h->eng = eng;
    pthread_mutex_init(&h->mu, NULL);
    pthread_cond_init(&h->cv, NULL);

    /* URL + body */
    size_t url_len = strlen(eng->base_url) + strlen(path) + 1;
    h->url = (char*)malloc(url_len);
    if (!h->url) { cJSON_Delete(body_obj); qid_async_free(h); return NULL; }
    snprintf(h->url, url_len, "%s%s", eng->base_url, path);
    h->body = cJSON_PrintUnformatted(body_obj);
    cJSON_Delete(body_obj);
    if (!h->body) { qid_async_free(h); return NULL; }

    /* easy handle */
    h->easy = curl_easy_init();
    if (!h->easy) { qid_async_free(h); return NULL; }
    h->headers = curl_slist_append(NULL, "Content-Type: application/json");
    curl_easy_setopt(h->easy, CURLOPT_URL, h->url);
    curl_easy_setopt(h->easy, CURLOPT_HTTPHEADER, h->headers);
    curl_easy_setopt(h->easy, CURLOPT_POSTFIELDS, h->body);
    curl_easy_setopt(h->easy, CURLOPT_POSTFIELDSIZE, (long)strlen(h->body));
    curl_easy_setopt(h->easy, CURLOPT_WRITEFUNCTION, qid_write_cb);
    curl_easy_setopt(h->easy, CURLOPT_WRITEDATA, &h->resp);
    curl_easy_setopt(h->easy, CURLOPT_TIMEOUT_MS, eng->timeout_ms);
    curl_easy_setopt(h->easy, CURLOPT_CONNECTTIMEOUT_MS, eng->connect_timeout_ms);
    curl_easy_setopt(h->easy, CURLOPT_NOSIGNAL, 1L);
    if (eng->ca_file)   curl_easy_setopt(h->easy, CURLOPT_CAINFO,  eng->ca_file);
    if (eng->cert_file) curl_easy_setopt(h->easy, CURLOPT_SSLCERT, eng->cert_file);
    if (eng->key_file)  curl_easy_setopt(h->easy, CURLOPT_SSLKEY,  eng->key_file);
    if (eng->insecure_skip_verify) {
        curl_easy_setopt(h->easy, CURLOPT_SSL_VERIFYPEER, 0L);
        curl_easy_setopt(h->easy, CURLOPT_SSL_VERIFYHOST, 0L);
    }

    /* enqueue */
    pthread_mutex_lock(&eng->mu);
    if (eng->closed) {
        pthread_mutex_unlock(&eng->mu);
        async_set_done(h, QID_ERR_TRANSPORT);
        return h;
    }
    if (!eng->head) {
        eng->head = eng->tail = h;
    } else {
        eng->tail->next = h;
        eng->tail = h;
    }
    pthread_cond_signal(&eng->cv);
    pthread_mutex_unlock(&eng->mu);
    return h;
}

qid_async_t* qid_validate_async(qid_async_engine_t* eng, const char* quote_id) {
    if (!quote_id) return NULL;
    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "quoteId", quote_id);
    cJSON_AddStringToObject(root, "engineId", eng ? eng->engine_id : "");
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    return async_submit(eng, QID_ASYNC_VALIDATE, "/v1/quoteid/validate", root);
}

qid_async_t* qid_mark_consumed_async(qid_async_engine_t* eng,
                                     const char* quote_id,
                                     const char* consumer_id) {
    if (!quote_id || !consumer_id) return NULL;
    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "quoteId", quote_id);
    cJSON_AddStringToObject(root, "consumerId", consumer_id);
    cJSON_AddStringToObject(root, "engineId", eng ? eng->engine_id : "");
    cJSON_AddNumberToObject(root, "tsUnixNano", (double)now_unix_nano());
    return async_submit(eng, QID_ASYNC_MARK, "/v1/quoteid/mark-consumed", root);
}

int qid_async_is_done(qid_async_t* h) {
    if (!h) return 1;
    pthread_mutex_lock(&h->mu);
    int d = h->done;
    pthread_mutex_unlock(&h->mu);
    return d;
}

qid_err_t qid_async_wait(qid_async_t* h) {
    if (!h) return QID_ERR_BAD_REQUEST;
    pthread_mutex_lock(&h->mu);
    while (!h->done) {
        pthread_cond_wait(&h->cv, &h->mu);
    }
    qid_err_t err = h->err;
    pthread_mutex_unlock(&h->mu);
    return err;
}

/* 응답 body → result struct 공통. body 가 비어있으면 JSON 오류로. */
static qid_err_t parse_async_validate(qid_async_t* h, qid_validate_result_t* out) {
    memset(out, 0, sizeof(*out));
    if (!h->resp.data) return QID_ERR_JSON;
    cJSON* j = cJSON_Parse(h->resp.data);
    if (!j) return QID_ERR_JSON;
    out->status = parse_status(j);
    parse_record(j, &out->record);
    out->ord_rej_reason = j_i32(j, "ordRejReason");
    copy_jstring(out->reject_text, sizeof(out->reject_text), j, "rejectText");
    cJSON_Delete(j);
    return QID_OK;
}

static qid_err_t parse_async_mark(qid_async_t* h, qid_mark_result_t* out) {
    memset(out, 0, sizeof(*out));
    if (!h->resp.data) return QID_ERR_JSON;
    cJSON* j = cJSON_Parse(h->resp.data);
    if (!j) return QID_ERR_JSON;
    out->status = parse_status(j);
    parse_record(j, &out->record);
    copy_jstring(out->consumed_by, sizeof(out->consumed_by), j, "consumedBy");
    out->ord_rej_reason = j_i32(j, "ordRejReason");
    copy_jstring(out->reject_text, sizeof(out->reject_text), j, "rejectText");
    cJSON_Delete(j);
    return QID_OK;
}

qid_err_t qid_async_get_validate(qid_async_t* h, qid_validate_result_t* out) {
    if (!h || !out) return QID_ERR_BAD_REQUEST;
    qid_err_t err = qid_async_wait(h);
    if (err != QID_OK) return err;
    return parse_async_validate(h, out);
}

qid_err_t qid_async_get_mark(qid_async_t* h, qid_mark_result_t* out) {
    if (!h || !out) return QID_ERR_BAD_REQUEST;
    qid_err_t err = qid_async_wait(h);
    if (err != QID_OK) return err;
    return parse_async_mark(h, out);
}

void qid_async_free(qid_async_t* h) {
    if (!h) return;
    if (h->easy)    curl_easy_cleanup(h->easy);
    if (h->headers) curl_slist_free_all(h->headers);
    qid_buf_free(&h->resp);
    free(h->url);
    free(h->body);
    pthread_mutex_destroy(&h->mu);
    pthread_cond_destroy(&h->cv);
    free(h);
}

qid_client_pool_stats_t qid_client_pool_stats(const qid_client_pool_t* pool) {
    qid_client_pool_stats_t s = {0};
    if (!pool) return s;
    /* 비-const lock — 통계 읽기지만 일관성 위해 lock. cast 는 표준 패턴. */
    pthread_mutex_t* mu = (pthread_mutex_t*)&pool->mu;
    pthread_mutex_lock(mu);
    s.size      = pool->cap;
    s.available = pool->free_n;
    s.acquires  = pool->acquires;
    s.contended = pool->contended;
    pthread_mutex_unlock(mu);
    return s;
}
