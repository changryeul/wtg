/*
 * sample_s02.c — wtg_query_w9501s02 사용 예. mds 의 거래소별 spot 호가
 * 조회 wire 그대로 — exnm 와 symb 입력, mds output struct 출력.
 *
 *   ./sample_s02 <host> <port> [<exnm>] [<symb>]
 *
 * exnm: "BEST" (기본) / "SMB" / "KMB" / "EBS" / "REUT"
 */

#include "wtgquery.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
    const char *host = (argc > 1) ? argv[1] : "127.0.0.1";
    int         port = (argc > 2) ? atoi(argv[2]) : 8082;
    const char *exnm = (argc > 3) ? argv[3] : "BEST";
    const char *symb = (argc > 4) ? argv[4] : "USDKRW";

    wtg_query_client_t cli;
    int rc = wtg_query_init(&cli, host, port, /*timeout_ms=*/1000);
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "init: %s\n", wtg_query_strerror(rc));
        return 1;
    }

    W9501S02_in_t in;
    memset(&in, 0, sizeof(in));
    strncpy(in.exnm, exnm, sizeof(in.exnm) - 1);
    strncpy(in.symb, symb, sizeof(in.symb) - 1);

    W9501S02_out_t out;
    rc = wtg_query_w9501s02(&cli, &in, &out);
    if (rc != WTGQUERY_OK) {
        fprintf(stderr, "w9501s02: %s (http=%d errno=%d body=%s)\n",
                wtg_query_strerror(rc), cli.last_http_status, cli.last_errno,
                cli.last_error_body);
        return 1;
    }

    printf("exnm        : %.16s\n", out.exnm);
    printf("symb        : %.16s\n", out.symb);
    printf("bid         : %s (source=%c)\n", out.bid, out.bid_source[0]);
    printf("ask         : %s (source=%c)\n", out.ask, out.ask_source[0]);
    printf("bid_best    : %s\n", out.bid_best);
    printf("ask_best    : %s\n", out.ask_best);
    return 0;
}
