/*
 * sample_spot.c — wtg_price_get_spot 사용 예. 빌드 후
 *
 *   ./sample_spot <host> <port> [<pairs_csv>] [<profile>] [<customer_id>]
 *
 * 으로 실행하면 USD/KRW 등의 현재 customer-applied bid/ask 를 받아 출력.
 * pairs_csv 콤마 구분 다중 (cap 16). customer_id 빈 문자열 이면 5-Layer 적용 skip.
 *
 * 매칭 엔진 / 운영 svc 통합 시:
 *   1. wtg_price_init 1회.
 *   2. 거래 진입 직전 wtg_price_get_spot 1회 (필요한 pair 묶음).
 *   3. table_version 이 변경 안 됐으면 stream 의 SubscribeCustomerQuote 로 갱신.
 */

#include "wtgprice.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
    const char *host    = (argc > 1) ? argv[1] : "127.0.0.1";
    int         port    = (argc > 2) ? atoi(argv[2]) : 8082;
    const char *pairs   = (argc > 3) ? argv[3] : "USD/KRW";
    const char *profile = (argc > 4) ? argv[4] : "WEB.BRANCH.VIP";
    const char *custid  = (argc > 5) ? argv[5] : "";

    wtg_price_client_t cli;
    int rc = wtg_price_init(&cli, host, port, /*timeout_ms=*/1000);
    if (rc != WTGPRICE_OK) {
        fprintf(stderr, "init: %s\n", wtg_price_strerror(rc));
        return 1;
    }

    wtg_spot_req_t req;
    memset(&req, 0, sizeof(req));
    req.pairs_csv   = pairs;
    req.profile     = profile;
    req.customer_id = custid;       /* "" 면 wtgprice.c 가 자동 skip */

    wtg_spot_result_t res;
    rc = wtg_price_get_spot(&cli, &req, &res);
    if (rc != WTGPRICE_OK) {
        fprintf(stderr, "get_spot: %s (http=%d errno=%d body=%s)\n",
                wtg_price_strerror(rc), cli.last_http_status, cli.last_errno,
                cli.last_error_body);
        return 1;
    }

    printf("table_ver   : %lld\n", res.table_version);
    printf("spot_count  : %d\n", res.spot_count);
    for (int i = 0; i < res.spot_count; i++) {
        printf("spot[%d]     : pair=%s bid=%.5f ask=%.5f raw=%.5f/%.5f src=%s\n",
               i,
               res.spots[i].pair,
               res.spots[i].bid, res.spots[i].ask,
               res.spots[i].raw_bid, res.spots[i].raw_ask,
               res.spots[i].source);
    }
    if (res.missing_count > 0) {
        printf("missing     : %d pair\n", res.missing_count);
        for (int i = 0; i < res.missing_count; i++) {
            printf("missing[%d]  : %s\n", i, res.missing[i]);
        }
    }
    return 0;
}
