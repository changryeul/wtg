/*
 * test_async.c — qid_async_engine_t 의 curl_multi 비동기 검증.
 *
 * N submit → 다른 작업 → wait/get 분리 흐름.  파이프라이닝 확인:
 * - 모든 호출이 직렬보다 빠른 wallclock 으로 끝나는지.
 * - 누락 / 데드락 없는지.
 *
 * 빌드:
 *   gcc test_async.c quoteid_client.c -lcurl -lcjson -lpthread -o test_async
 *
 * 실행 (mci-price :18084 에 dev 모드):
 *   QID_BASE=http://localhost:18084 ./test_async
 */

#include "quoteid_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <curl/curl.h>

#define N_REQUESTS 50

static const char* env_or(const char* k, const char* fb) {
    const char* v = getenv(k);
    return (v && *v) ? v : fb;
}

static double now_sec(void) {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return tv.tv_sec + tv.tv_usec / 1e6;
}

int main(void) {
    curl_global_init(CURL_GLOBAL_DEFAULT);

    qid_client_options_t opts = {0};
    opts.base_url  = env_or("QID_BASE",      "http://localhost:18084");
    opts.engine_id = env_or("QID_ENGINE_ID", "test-async");
    opts.timeout_ms = 2000;

    qid_async_engine_t* eng = qid_async_engine_new(&opts);
    if (!eng) {
        fprintf(stderr, "engine 생성 실패\n");
        return 1;
    }

    /* 1) N 요청 동시 submit. */
    double t0 = now_sec();
    qid_async_t* handles[N_REQUESTS];
    for (int i = 0; i < N_REQUESTS; i++) {
        char qid[64];
        snprintf(qid, sizeof(qid), "async-test-%d", i);
        handles[i] = qid_validate_async(eng, qid);
        if (!handles[i]) {
            fprintf(stderr, "submit %d 실패\n", i);
            return 1;
        }
    }
    double t_submit = now_sec();
    fprintf(stderr, "submit done %d in %.4fs\n", N_REQUESTS, t_submit - t0);

    /* 2) 다른 작업 (시뮬레이션) — 50ms 대기. 그 사이 worker 가 진행. */
    struct timespec ts = {0, 50 * 1000 * 1000};
    nanosleep(&ts, NULL);

    /* 3) 결과 수거. is_done 으로 미리 잡힌 게 몇 개나 있는지 보자. */
    int already_done = 0;
    for (int i = 0; i < N_REQUESTS; i++) {
        if (qid_async_is_done(handles[i])) already_done++;
    }
    fprintf(stderr, "after 50ms sleep: %d/%d already done\n", already_done, N_REQUESTS);

    /* 4) wait + get. */
    int ok = 0, not_found = 0, err = 0;
    for (int i = 0; i < N_REQUESTS; i++) {
        qid_validate_result_t vr;
        qid_err_t e = qid_async_get_validate(handles[i], &vr);
        if (e != QID_OK) { err++; }
        else if (vr.status == QID_STATUS_NOT_FOUND) { not_found++; ok++; }
        else { ok++; }
        qid_async_free(handles[i]);
    }
    double t_done = now_sec();
    fprintf(stderr, "all done in %.4fs (submit→done %.4fs)\n",
            t_done - t0, t_done - t_submit);
    fprintf(stderr, "ok=%d err=%d not_found=%d\n", ok, err, not_found);

    qid_async_engine_free(eng);
    curl_global_cleanup();

    if (ok != N_REQUESTS) {
        fprintf(stderr, "FAIL: ok=%d, want %d\n", ok, N_REQUESTS);
        return 1;
    }
    fprintf(stderr, "OK\n");
    return 0;
}
