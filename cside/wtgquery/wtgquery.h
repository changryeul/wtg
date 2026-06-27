/*
 * wtgquery.h — mds query-server W9501 시리즈의 WTG mci-chart 백엔드 wrapper.
 *
 * PoC 목표: 기존 NH 사내 client 코드의 W9501S01 broker RPC 호출을 본 SDK
 *   한 줄로 교체해 WTG mci-chart REST 로 직결한다. mds 의 W9501S01_in_t /
 *   _dat_t / _out_t 와 **메모리 레이아웃 동일** 한 struct 를 그대로 제공
 *   (mds header 의존 0) — drop-in 가능.
 *
 * 패턴: cside/wtgprice 와 동일 — 외부 의존 0 (POSIX socket + HTTP/1.1 +
 *   간이 JSON). AIX/Solaris/HPUX/Linux/Darwin 그대로 빌드.
 *
 * 백엔드: GET /v1/chart?pair=...&tf=1d&from=...&to=...&limit=N
 *   → 단일/다중 일봉 → W9501S01_out_t 채움.
 *
 * mds 와의 의미 매핑:
 *   pdcd "SPT" (현물환)      → tf "1d"
 *   pdcd "FWD" (선물환)      → 현 PoC 미지원 (forward-snapshot 별도 endpoint)
 *   symb "USDKRW"            → pair "USD/KRW"  (slash 삽입 — 6자 ↔ X/XXX)
 *   tenor "" (전체)          → 무시 (spot 만)
 *   bid/ask OHLC float       → "%.5f" sprintf (16-char ASCII)
 *   opened_at (UTC RFC3339)  → kymd "yyyymmdd" + khms "HHmmss"
 *
 * 사용 예 (자세히는 sample.c):
 *
 *   wtg_query_client_t cli;
 *   wtg_query_init(&cli, "mci-chart.internal", 8086, 1000);
 *
 *   W9501S01_in_t in;
 *   memset(&in, 0, sizeof(in));
 *   memcpy(in.pdcd, "SPT", 3);
 *   memcpy(in.symb, "USDKRW", 6);
 *
 *   // 캡 16 일봉. 호출자 할당 — flexible array 형태로.
 *   char out_buf[sizeof(W9501S01_out_t) + 16 * sizeof(W9501S01_dat_t)];
 *   W9501S01_out_t *out = (W9501S01_out_t *)out_buf;
 *
 *   int rc = wtg_query_w9501s01(&cli, &in, out, sizeof(out_buf));
 *   if (rc == WTGQUERY_OK) {
 *       int nrec = atoi(out->nrec);
 *       // out->data[0..nrec-1] 사용
 *   }
 */

#ifndef WTGQUERY_H
#define WTGQUERY_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* 반환 코드 — 0 = 성공, < 0 = 실패. wtgprice 와 동일 매핑. */
#define WTGQUERY_OK              0
#define WTGQUERY_E_INVALID      -1   /* NULL 또는 필수 필드 누락 */
#define WTGQUERY_E_RESOLVE      -2   /* host resolve 실패 */
#define WTGQUERY_E_SOCKET       -3   /* socket() / setsockopt() 실패 */
#define WTGQUERY_E_CONNECT      -4   /* connect() 실패 (timeout 포함) */
#define WTGQUERY_E_SEND         -5   /* send() 실패 */
#define WTGQUERY_E_RECV         -6   /* recv() 실패 또는 timeout */
#define WTGQUERY_E_PARSE        -7   /* HTTP / JSON 파싱 실패 */
#define WTGQUERY_E_HTTP_4XX     -8   /* HTTP 4xx */
#define WTGQUERY_E_HTTP_5XX     -9   /* HTTP 5xx */
#define WTGQUERY_E_OVERSIZE    -10   /* buffer 한계 초과 (out_cap 부족 등) */
#define WTGQUERY_E_UNSUPPORTED -11   /* 입력 조합 미지원 (예: pdcd FWD) */

#define WTGQUERY_ERRBODY_LEN       256
#define WTGQUERY_S03_MAX_RECORDS    16   /* S03 bulk 호출당 최대 통화쌍 수 */

/*
 * mds wire-compat types — src/query-server/W9500.h 의 정의를 그대로 복제.
 * 필드 순서·크기·padding 동일. mds 헤더와 sizeof / offsetof 일치해야 NH client
 * 코드가 drop-in 으로 link 가능.
 */
typedef struct {
    char pdcd  [ 4];   /* 'SPT' 현물환 / 'FWD' 선물환 */
    char type  [ 4];   /* 0:전체 1:원화 2:달러 (pdcd=SPT 일 때) */
    char symb  [16];   /* 통화쌍 (공백이면 전체) — 예: "USDKRW" */
    char tenor [16];   /* 테너 (공백이면 전체) */
} W9501S01_in_t;

typedef struct {
    char symb     [16];
    char tenor    [16];
    char kymd     [16];   /* "yyyymmdd" */
    char khms     [16];   /* "HHmmss" */
    char bid_open [16];   /* "%.5f" ASCII */
    char bid_high [16];
    char bid_lowp [16];
    char bid_last [16];   /* close_bid */
    char ask_open [16];
    char ask_high [16];
    char ask_lowp [16];
    char ask_last [16];   /* close_ask */
    char expiymd  [16];   /* 만기일 (spot 은 "") */
} W9501S01_dat_t;

typedef struct {
    char pdcd  [ 4];   /* 입력 echo */
    char symb  [16];
    char tenor [16];
    char nrec  [ 4];   /* 데이터 건수 ASCII */
    W9501S01_dat_t data[0];   /* flexible — out_cap 으로 크기 명시 */
} W9501S01_out_t;

/*
 * W9501S02 — 거래소별 spot 호가 조회. mds 원형은 30+ 필드의 큰 struct (시초가,
 * 고저, 현재가, 시초대비, 전일대비, mid, base, fill 등). 본 PoC 는 mds 와
 * memory layout 동일한 출력 구조를 그대로 채워주되, WTG 백엔드 (/v1/best-stats)
 * 가 제공 안 하는 audit 성 필드 (시초대비/전일대비/base/fill 등) 는 "" 로 둠.
 * 채워지는 핵심 필드: exnm, symb, bid, ask, bid_best, ask_best, bid_source,
 * ask_source. mds 와 wire 일치 — NH client 가 receive struct 만 그대로 사용.
 */
typedef struct {
    char exnm    [16];   /* 거래소명 — "BEST" / "REUT" / "SMB" / "EBS" / "KMB" */
    char symb    [16];   /* 통화쌍 — 예: "USDKRW" */
    char pay_ymd [16];   /* 지급일자 (PoC 미사용 — "") */
    char exp_ymd [16];   /* 만기일자 (PoC 미사용 — "") */
} W9501S02_in_t;

typedef struct {
    char exnm        [16];   /* 입력 echo */
    char symb        [16];   /* 통화쌍코드 */
    char symb_cross  [16];   /* 크로스 통화쌍 — PoC 미사용 */
    char rdcode      [16];   /* realtime symbol — PoC 미사용 */
    char pay_ymd     [16];
    char exp_ymd     [16];
    char bid_open    [16];   /* 시초 — PoC 미사용 (BEST 의 시초는 봉 영역) */
    char bid_high    [16];
    char bid_lowp    [16];
    char bid_diff_c  [16];   /* '+', '-', ' ' — PoC 미사용 */
    char bid_diff    [16];
    char ask_open    [16];
    char ask_high    [16];
    char ask_lowp    [16];
    char ask_diff_c  [16];
    char ask_diff    [16];
    char bid_c       [16];
    char bid         [16];   /* 현재가 bid (라이브) */
    char ask_c       [16];
    char ask         [16];   /* 현재가 ask (라이브) */
    char bid_base    [16];   /* 기준가 — PoC 미사용 */
    char ask_base    [16];
    char fill_prc    [16];   /* 체결가 — PoC 미사용 */
    char mid_open    [16];
    char mid_high    [16];
    char mid_lowp    [16];
    char mid_diff_c  [16];
    char mid_diff    [16];
    char bid_best    [16];   /* BEST bid (항상 채워짐) */
    char ask_best    [16];   /* BEST ask */
    char bid_source  [ 1];   /* 1 char — 'S'(MB) 'K'(MB) 'E'(BS) 'C' 'B'(est) 'Z'(cust) */
    char ask_source  [ 1];
    /* 2024.06.25 mds 확장 — 누적 고저 (request-window). PoC 미사용. */
    char bid_high_a  [16];
    char bid_lowp_a  [16];
    char ask_high_a  [16];
    char ask_lowp_a  [16];
    /* __MID__ define 시 추가 — 본 PoC 는 not defined 가정. */
} W9501S02_out_t;

/*
 * W9501S03 — S02 의 bulk. 입력은 N pair 의 in[], 출력도 N out[].
 */
typedef struct {
    char nrec [ 6];          /* 입력 건수 ASCII */
    W9501S02_in_t data[0];   /* flexible — req_cap 으로 크기 명시 */
} W9501S03_in_t;

typedef struct {
    char nrec [ 6];          /* 응답 건수 ASCII */
    W9501S02_out_t data[0];  /* flexible — out_cap 으로 크기 명시 */
} W9501S03_out_t;

/* 클라이언트 컨텍스트. 한 번 init 후 여러 호출 재사용. wtgprice 동일 패턴. */
typedef struct {
    char host[256];
    int  port;
    int  timeout_ms;                              /* 0 = 5000 */
    int  last_http_status;                        /* 디버그용 */
    int  last_errno;
    char last_error_body[WTGQUERY_ERRBODY_LEN];   /* 4xx/5xx 본문 일부 */
} wtg_query_client_t;

/*
 * wtg_query_init — 클라이언트 초기화.
 *
 * @cli         NULL 아님.
 * @host        mci-chart hostname 또는 IPv4 (예: "mci-chart.internal").
 * @port        포트 (보통 8086).
 * @timeout_ms  socket 단계별 timeout (권장 1000~2000). 0 이면 5000.
 *
 * 반환: WTGQUERY_OK 또는 WTGQUERY_E_INVALID.
 */
int wtg_query_init(wtg_query_client_t *cli,
                   const char *host, int port, int timeout_ms);

/*
 * wtg_query_w9501s01 — mds W9501S01 종가 조회의 WTG 백엔드 호출.
 *
 * @cli     init 된 클라이언트.
 * @in      W9501S01_in_t 입력. pdcd 'SPT' 만 PoC 지원 (FWD 는 _UNSUPPORTED).
 *          symb 가 공백/빈값이면 PoC 는 _INVALID (mds 의 "전체 조회" 는 별도
 *          phase — pair list 가 너무 큼).
 * @out     W9501S01_out_t — 호출자 할당. nrec / data[] 채워짐.
 *          out_cap 은 sizeof(*out) + N * sizeof(W9501S01_dat_t) 권장.
 *          PoC 는 N=16 (cside/wtgprice 와 동일 cap).
 * @out_cap out 의 전체 크기 (data 영역 포함). PoC 는 sample 가 사용.
 *
 * 반환: WTGQUERY_OK 또는 WTGQUERY_E_*. 실패 시 cli->last_http_status /
 *       cli->last_errno / cli->last_error_body 참조.
 *
 * 정책: 1회 시도 (retry 없음). 종가는 idempotent 라 retry 가능하지만 wtgprice
 *       와 일관성 유지 — 호출자가 정책 결정.
 */
int wtg_query_w9501s01(wtg_query_client_t *cli,
                       const W9501S01_in_t *in,
                       W9501S01_out_t *out, size_t out_cap);

/*
 * wtg_query_w9501s02 — mds W9501S02 거래소별 spot 호가 조회의 WTG 백엔드 호출.
 *
 * 백엔드: GET /v1/best-stats 의 BestSymbolStat (BestBid/BestAsk + SourceQuotes).
 * 매핑: exnm "BEST" → BestBid/BestAsk, "SMB"/"KMB"/"EBS"/"REUT" →
 *       SourceQuotes[exnm].
 *
 * @cli   init 된 클라이언트.
 * @in    W9501S02_in_t — exnm 와 symb 필수. pay_ymd/exp_ymd 는 PoC 미사용.
 * @out   W9501S02_out_t — 호출자 zero-init 권장. 핵심 필드 (exnm/symb/bid/
 *        ask/bid_best/ask_best/bid_source/ask_source) 채워짐, audit 성 필드는
 *        "". 호가 miss (exnm 가 active source 가 아님) 시 bid/ask "0.00000".
 *
 * 반환: WTGQUERY_OK 또는 WTGQUERY_E_*.
 */
int wtg_query_w9501s02(wtg_query_client_t *cli,
                       const W9501S02_in_t *in,
                       W9501S02_out_t *out);

/*
 * wtg_query_w9501s03 — W9501S02 의 bulk 버전.
 *
 * @cli      init 된 클라이언트.
 * @in       W9501S03_in_t — nrec ASCII + W9501S02_in_t[] flexible.
 * @in_cap   in 전체 크기.
 * @out      W9501S03_out_t — 호출자 zero-init 권장. data[] 채워짐.
 * @out_cap  out 전체 크기. data 슬롯 = (out_cap - sizeof(W9501S03_out_t)) /
 *           sizeof(W9501S02_out_t).
 *
 * 캡: WTGQUERY_S03_MAX_RECORDS (16) — 초과 시 _OVERSIZE.
 *
 * 반환: WTGQUERY_OK 또는 WTGQUERY_E_*.
 */
int wtg_query_w9501s03(wtg_query_client_t *cli,
                       const W9501S03_in_t *in, size_t in_cap,
                       W9501S03_out_t *out, size_t out_cap);

/*
 * wtg_query_strerror — 반환 코드의 짧은 식별 메시지. static const,
 * 호출자 free 안 함, thread-safe.
 */
const char *wtg_query_strerror(int code);

#ifdef __cplusplus
}
#endif

#endif /* WTGQUERY_H */
