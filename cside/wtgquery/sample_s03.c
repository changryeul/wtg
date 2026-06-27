/*
 * sample_s03.c — wtg_query_w9501s03 사용 예. S02 의 bulk — 다중 (exnm, symb)
 * 쌍을 한 호출에 조회. mds wire 그대로 — N pair input → N pair output.
 *
 *   ./sample_s03 <host> <port>
 *
 * 데모는 (BEST,USDKRW) + (SMB,USDKRW) + (KMB,USDKRW) 3개 고정 조회.
 * 실제 운영 client 는 NH 내부의 통화쌍 list 로 채워 호출.
 */

#include "wtgquery.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define NRECS 3

int main(int argc, char **argv) {
    const char *host = (argc > 1) ? argv[1] : "127.0.0.1";
    int         port = (argc > 2) ? atoi(argv[2]) : 8082;

    wtg_query_client_t cli;
    int rc = wtg_query_init(&cli, host, port, /*timeout_ms=*/1000);
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "init: %s\n", wtg_query_strerror(rc));
        return 1;
    }

    /* in / out — flexible array 형태. NRECS 슬롯. */
    char in_buf [sizeof(W9501S03_in_t)  + NRECS * sizeof(W9501S02_in_t)];
    char out_buf[sizeof(W9501S03_out_t) + NRECS * sizeof(W9501S02_out_t)];
    W9501S03_in_t  *req = (W9501S03_in_t  *)in_buf;
    W9501S03_out_t *res = (W9501S03_out_t *)out_buf;
    memset(in_buf,  0, sizeof(in_buf));
    memset(out_buf, 0, sizeof(out_buf));

    snprintf(req->nrec, sizeof(req->nrec), "%d", NRECS);

    static const char *exnms[NRECS] = { "BEST", "SMB", "KMB" };
    for (int i = 0; i < NRECS; i++) {
        strncpy(req->data[i].exnm, exnms[i], sizeof(req->data[i].exnm) - 1);
        strncpy(req->data[i].symb, "USDKRW",  sizeof(req->data[i].symb) - 1);
    }

    rc = wtg_query_w9501s03(&cli, req, sizeof(in_buf), res, sizeof(out_buf));
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "w9501s03: %s (http=%d errno=%d body=%s)\n",
                wtg_query_strerror(rc), cli.last_http_status, cli.last_errno,
                cli.last_error_body);
        return 1;
    }

    int nrec = atoi(res->nrec);
    printf("nrec        : %d\n", nrec);
    for (int i = 0; i < nrec; i++) {
        W9501S02_out_t *d = &res->data[i];
        printf("rec[%d]      : exnm=%.16s symb=%.16s\n", i, d->exnm, d->symb);
        printf("rec[%d] bid  : %s (src=%c)\n", i, d->bid, d->bid_source[0]);
        printf("rec[%d] ask  : %s (src=%c)\n", i, d->ask, d->ask_source[0]);
        printf("rec[%d] best : bid=%s ask=%s\n", i, d->bid_best, d->ask_best);
    }
    return 0;
}
