/*
 * sample.c — wtgprice 사용 예. 빌드 후 ./sample <host> <port> 로 실행하면
 * USD/KRW SPOT ↔ 1M swap 잠금 1회 발급 + 결과 출력.
 *
 * 매칭 엔진 통합 시 templates:
 *   1. wtg_price_init 1회 (프로세스 시작 시).
 *   2. 체결 직전 wtg_price_swap_lock 호출.
 *   3. valid_until_unix_nano 안에 매매 transaction submit (swap_id 첨부).
 *   4. timeout / 4xx / 5xx 어느 쪽도 거래 거부 — retry 금지.
 */

#include "wtgprice.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

static long long now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (long long)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

int main(int argc, char **argv) {
    const char *host = (argc > 1) ? argv[1] : "127.0.0.1";
    int port = (argc > 2) ? atoi(argv[2]) : 8082;

    wtg_price_client_t cli;
    int rc = wtg_price_init(&cli, host, port, /*timeout_ms=*/1000);
    if (rc != WTGPRICE_OK) {
        fprintf(stderr, "init: %s\n", wtg_price_strerror(rc));
        return 1;
    }

    wtg_swap_req_t req;
    memset(&req, 0, sizeof(req));
    req.pair        = "USD/KRW";
    req.near_tenor  = "SPOT";
    req.far_tenor   = "1M";
    req.profile     = "WEB.BRANCH.VIP";
    req.customer_id = "alice";
    req.side        = "buy_sell";
    req.amount      = 1000000.0;

    wtg_swap_result_t res;
    rc = wtg_price_swap_lock(&cli, &req, &res);
    if (rc != WTGPRICE_OK) {
        fprintf(stderr, "swap_lock: %s (http=%d errno=%d body=%s)\n",
                wtg_price_strerror(rc), cli.last_http_status, cli.last_errno,
                cli.last_error_body);
        return 1;
    }

    long long remain = res.valid_until_unix_nano - now_ns();
    printf("swap_id     : %s\n", res.swap_id);
    printf("pair        : %s\n", res.pair);
    printf("valid_remain: %lld ns (%.1f ms)\n", remain, remain / 1e6);
    printf("table_ver   : %lld\n", res.table_version);
    printf("near        : qid=%s tenor=%s bid=%.5f ask=%.5f raw=%.5f/%.5f\n",
           res.near.quote_id, res.near.tenor,
           res.near.bid, res.near.ask, res.near.raw_bid, res.near.raw_ask);
    printf("far         : qid=%s tenor=%s bid=%.5f ask=%.5f raw=%.5f/%.5f\n",
           res.far_.quote_id, res.far_.tenor,
           res.far_.bid, res.far_.ask, res.far_.raw_bid, res.far_.raw_ask);
    printf("swap_diff   : bid=%.5f ask=%.5f\n", res.bid_diff, res.ask_diff);

    /* 안전마진 — 매매 transaction 처리 budget 100ms 미만이면 폐기. */
    if (remain < 100 * 1000000LL) {
        fprintf(stderr, "남은 유효기간 부족 — 거래 폐기\n");
        return 2;
    }
    /* 여기서 매매 엔진의 broker transaction submit (swap_id 첨부). */
    return 0;
}
