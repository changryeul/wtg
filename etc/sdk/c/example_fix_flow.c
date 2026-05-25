/*
 * example_fix_flow.c — WTG QuoteValidationService C SDK 사용 예제.
 *
 * FIX 4.4 NewOrderSingle (D) 처리 안에서 Validate → engine 자체 정책 →
 * MarkConsumed → fill / reject 의 전형적 흐름. 실제 매칭 엔진은 이걸
 * order handler 코드 안에 inline.
 *
 * 빌드:
 *   gcc example_fix_flow.c quoteid_client.c -lcurl -lcjson -o example_fix_flow
 *
 * 실행 (mci-price 가 :8082 에서 HTTPS, 엔진 mTLS):
 *   ./example_fix_flow A-mq4b3z-1f order-42
 *
 * 실행 (dev plain HTTP, mci-price --listen :8082 --quoteid-instance A):
 *   QID_BASE=http://localhost:8082 ./example_fix_flow A-mq4b3z-1f order-42
 */

#include "quoteid_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <curl/curl.h>

static const char* env_or(const char* key, const char* fallback) {
    const char* v = getenv(key);
    return (v && *v) ? v : fallback;
}

int main(int argc, char** argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: %s <quote_id> <order_id>\n", argv[0]);
        return 2;
    }
    const char* qid       = argv[1];
    const char* order_id  = argv[2];

    /* libcurl global — multi-thread 환경의 표준 1회 호출. */
    curl_global_init(CURL_GLOBAL_DEFAULT);

    qid_client_options_t opts = {0};
    opts.base_url  = env_or("QID_BASE",      "https://mci-price.internal:8443");
    opts.engine_id = env_or("QID_ENGINE_ID", "matching-A");
    opts.ca_file   = getenv("QID_CA_FILE");
    opts.cert_file = getenv("QID_CERT_FILE");
    opts.key_file  = getenv("QID_KEY_FILE");
    opts.timeout_ms = 1000;

    qid_client_t* c = qid_client_new(&opts);
    if (!c) {
        fprintf(stderr, "client 생성 실패\n");
        curl_global_cleanup();
        return 1;
    }

    /* 1) Validate — engine 자체 정책 검증 전 단계. */
    qid_validate_result_t vr;
    qid_err_t err = qid_validate(c, qid, &vr);
    if (err != QID_OK) {
        fprintf(stderr, "Validate transport err: %s\n", qid_err_name(err));
        goto done;
    }
    printf("[Validate] status=%s pair=%s bid=%.5f ask=%.5f profile=%s.%s.%s\n",
           qid_status_name(vr.status),
           vr.record.pair, vr.record.bid, vr.record.ask,
           vr.record.channel, vr.record.site, vr.record.tier);
    if (vr.status != QID_STATUS_OK) {
        /* OrdRejReason 을 FIX ExecutionReport tag 103 에 그대로. */
        printf("REJECT order — OrdRejReason=%d (%s)\n",
               vr.ord_rej_reason, vr.reject_text);
        goto done;
    }

    /* 2) 엔진 자체 정책 (slippage / side / tier 한도 etc.) 검증 — 여기 생략. */
    /*    실제 엔진은 vr.record.bid / ask 를 기준으로 사용자 요청가 비교. */

    /* 3) MarkConsumed — atomic 사용 표시. race 충돌 시 정확히 한 호출만 OK. */
    qid_mark_result_t mr;
    err = qid_mark_consumed(c, qid, order_id, &mr);
    if (err != QID_OK) {
        fprintf(stderr, "MarkConsumed transport err: %s\n", qid_err_name(err));
        goto done;
    }
    printf("[MarkConsumed] status=%s\n", qid_status_name(mr.status));
    if (mr.status == QID_STATUS_OK) {
        printf("FILL order — %s @ bid=%.5f / ask=%.5f\n",
               order_id, mr.record.bid, mr.record.ask);
    } else if (mr.status == QID_STATUS_ALREADY_CONSUMED) {
        printf("REJECT order — quote 가 이미 다른 주문(%s) 에 사용됨, OrdRejReason=%d\n",
               mr.consumed_by, mr.ord_rej_reason);
    } else {
        printf("REJECT order — OrdRejReason=%d (%s)\n",
               mr.ord_rej_reason, mr.reject_text);
    }

done:
    qid_client_free(c);
    curl_global_cleanup();
    return 0;
}
