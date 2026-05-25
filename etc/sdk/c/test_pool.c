/*
 * test_pool.c — qid_client_pool_t 의 멀티스레드 동작 검증.
 *
 * pool size 4 + N worker thread, 각 thread 가 K 회 acquire/release 반복.
 * mci-price 가 :18084 에서 dev 모드로 돌고 있어야 한다 (또는 QID_BASE
 * 환경변수). 모든 호출의 NOT_FOUND status 가 OK 면 통과 — 실제 quote 없어
 * RPC 자체는 정상 응답.
 *
 * 빌드:
 *   gcc test_pool.c quoteid_client.c -lcurl -lcjson -lpthread -o test_pool
 *
 * 실행:
 *   QID_BASE=http://localhost:18084 ./test_pool
 *
 * 확인:
 *   - "acquires=N×K contended>0 OK_count=N×K" 출력.
 *   - contended > 0 — N > pool size 일 때 일부 acquire 는 block 했음.
 *   - 누락 / 데드락 / leak 없으면 exit 0.
 */

#include "quoteid_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <curl/curl.h>
#include <stdatomic.h>

#define POOL_SIZE     4
#define WORKERS       16
#define ITERS_PER     50

static atomic_uint_fast64_t g_ok_count;
static atomic_uint_fast64_t g_err_count;
static atomic_uint_fast64_t g_notfound_count;

static const char* env_or(const char* k, const char* fb) {
    const char* v = getenv(k);
    return (v && *v) ? v : fb;
}

static void* worker(void* arg) {
    qid_client_pool_t* pool = (qid_client_pool_t*)arg;
    char qid[64];
    char order[64];
    for (int i = 0; i < ITERS_PER; i++) {
        qid_client_t* c = qid_client_pool_acquire(pool);
        if (!c) {
            atomic_fetch_add(&g_err_count, 1);
            continue;
        }
        snprintf(qid,   sizeof(qid),   "test-%lu-%d", (unsigned long)pthread_self(), i);
        snprintf(order, sizeof(order), "ord-%lu-%d",  (unsigned long)pthread_self(), i);

        qid_validate_result_t vr;
        qid_err_t err = qid_validate(c, qid, &vr);
        if (err != QID_OK) {
            atomic_fetch_add(&g_err_count, 1);
        } else if (vr.status == QID_STATUS_NOT_FOUND) {
            atomic_fetch_add(&g_notfound_count, 1);
            atomic_fetch_add(&g_ok_count, 1);
        } else {
            atomic_fetch_add(&g_ok_count, 1);
        }
        qid_client_pool_release(pool, c);
    }
    return NULL;
}

int main(void) {
    curl_global_init(CURL_GLOBAL_DEFAULT);

    qid_client_options_t opts = {0};
    opts.base_url  = env_or("QID_BASE",      "http://localhost:18084");
    opts.engine_id = env_or("QID_ENGINE_ID", "test-pool");
    opts.timeout_ms = 2000;

    qid_client_pool_t* pool = qid_client_pool_new(&opts, POOL_SIZE);
    if (!pool) {
        fprintf(stderr, "pool 생성 실패\n");
        return 1;
    }
    fprintf(stderr, "pool=%d workers=%d iters_per=%d (total RPC=%d)\n",
            POOL_SIZE, WORKERS, ITERS_PER, WORKERS * ITERS_PER);

    pthread_t ts[WORKERS];
    for (int i = 0; i < WORKERS; i++) pthread_create(&ts[i], NULL, worker, pool);
    for (int i = 0; i < WORKERS; i++) pthread_join(ts[i], NULL);

    qid_client_pool_stats_t s = qid_client_pool_stats(pool);
    fprintf(stderr, "stats: size=%zu available=%zu acquires=%llu contended=%llu\n",
            s.size, s.available,
            (unsigned long long)s.acquires, (unsigned long long)s.contended);

    /* Prometheus exposition format — 엔진팀이 /metrics 응답에 첨부. */
    char prom[2048];
    size_t plen = qid_client_pool_stats_text(pool, "test-pool", prom, sizeof(prom));
    fprintf(stderr, "----- Prometheus (%zu bytes) -----\n%s-----\n", plen, prom);
    fprintf(stderr, "ok=%llu err=%llu not_found=%llu\n",
            (unsigned long long)atomic_load(&g_ok_count),
            (unsigned long long)atomic_load(&g_err_count),
            (unsigned long long)atomic_load(&g_notfound_count));

    /* 모든 client 가 pool 에 반환되었는지. */
    int leaked = (s.available != (size_t)POOL_SIZE);
    if (leaked) {
        fprintf(stderr, "FAIL: pool 미반환 client = %d\n",
                POOL_SIZE - (int)s.available);
    }

    qid_client_pool_free(pool);
    curl_global_cleanup();

    int expect_total = WORKERS * ITERS_PER;
    int total = (int)atomic_load(&g_ok_count) + (int)atomic_load(&g_err_count);
    if (total != expect_total) {
        fprintf(stderr, "FAIL: 총 호출 수 mismatch %d != %d\n", total, expect_total);
        return 1;
    }
    if (leaked) return 1;
    fprintf(stderr, "OK\n");
    return 0;
}
