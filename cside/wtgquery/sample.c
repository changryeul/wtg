/*
 * sample.c — wtg_query_w9501s01 사용 예. 빌드 후
 *
 *   ./sample <host> <port> [<symb>]
 *
 * 으로 실행하면 mci-chart 에서 종가 봉을 받아 mds 의 W9501S01_out_t 형태로
 * 채운 결과를 출력. 기본 symb 는 "USDKRW".
 *
 * 매칭 시점:
 *   기존 NH 사내 코드의
 *     ret = mymq_call(broker, "W9501S01", &in, ..., &out, ...);
 *   를
 *     ret = wtg_query_w9501s01(&cli, &in, &out, sizeof(out_buf));
 *   로 교체 — wire 그대로, in/out type 동일 (memory layout).
 */

#include "wtgquery.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
    const char *host = (argc > 1) ? argv[1] : "127.0.0.1";
    int         port = (argc > 2) ? atoi(argv[2]) : 8086;
    const char *symb = (argc > 3) ? argv[3] : "USDKRW";

    wtg_query_client_t cli;
    int rc = wtg_query_init(&cli, host, port, /*timeout_ms=*/1000);
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "init: %s\n", wtg_query_strerror(rc));
        return 1;
    }

    W9501S01_in_t in;
    memset(&in, 0, sizeof(in));
    memcpy(in.pdcd, "SPT", 3);
    /* symb 는 16-char 필드. 짧으면 NUL padding (mds 도 strlen 기반). */
    strncpy(in.symb, symb, sizeof(in.symb) - 1);

    /* out buf — cap 16 일봉. */
    enum { CAP_RECORDS = 16 };
    char out_buf[sizeof(W9501S01_out_t) + CAP_RECORDS * sizeof(W9501S01_dat_t)];
    W9501S01_out_t *out = (W9501S01_out_t *)out_buf;

    rc = wtg_query_w9501s01(&cli, &in, out, sizeof(out_buf));
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "w9501s01: %s (http=%d errno=%d body=%s)\n",
                wtg_query_strerror(rc), cli.last_http_status, cli.last_errno,
                cli.last_error_body);
        return 1;
    }

    int nrec = atoi(out->nrec);
    printf("pdcd        : %.4s\n", out->pdcd);
    printf("symb        : %.16s\n", out->symb);
    printf("nrec        : %d\n", nrec);
    for (int i = 0; i < nrec; i++) {
        W9501S01_dat_t *d = &out->data[i];
        printf("rec[%d]      : symb=%s kymd=%s khms=%s\n", i, d->symb, d->kymd, d->khms);
        printf("rec[%d] bid  : open=%s high=%s low=%s last=%s\n",
               i, d->bid_open, d->bid_high, d->bid_lowp, d->bid_last);
        printf("rec[%d] ask  : open=%s high=%s low=%s last=%s\n",
               i, d->ask_open, d->ask_high, d->ask_lowp, d->ask_last);
    }
    return 0;
}
