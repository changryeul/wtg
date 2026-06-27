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

#define WTGQUERY_ERRBODY_LEN   256

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
 * wtg_query_strerror — 반환 코드의 짧은 식별 메시지. static const,
 * 호출자 free 안 함, thread-safe.
 */
const char *wtg_query_strerror(int code);

#ifdef __cplusplus
}
#endif

#endif /* WTGQUERY_H */
