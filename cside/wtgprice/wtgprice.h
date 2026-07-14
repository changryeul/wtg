/*
 * wtgprice.h — WTG mci-price 의 swap/lock endpoint 호출 C SDK.
 *
 * Phase S3-d — C 매칭 엔진이 FX swap 거래 직전에 호출해 두 leg quote_id +
 *   swap_id 를 받아오는 hot-path 용 클라이언트.
 *
 * 설계:
 *   · 외부 의존 0 — POSIX socket + HTTP/1.1 minimal + 간이 JSON 파서.
 *     cside/wtgpush 와 동일 원칙. mymq 운영 환경 (AIX/Solaris/HPUX/Linux/Darwin)
 *     에서 그대로 빌드.
 *   · TLS 없음 — Internal 망 전용. 향후 mTLS 도입 시 wtgprice_tls.c 별도.
 *   · retry 금지 — swap_lock 의 quote_id 가 unique 라 중복 발급 위험. 본 SDK 는
 *     단 1회 시도 후 결과 반환. 호출자가 정책 결정 (timeout 이면 거래 거부 등).
 *   · thread-safe — 각 호출이 자체 socket open/close. cli 는 read-only 외 last_*
 *     필드만 호출별 갱신 — 다중 스레드에서 cli 공유 시 호출자가 mutex.
 *
 * 사용 예 (자세히는 sample.c 참조):
 *
 *   wtg_price_client_t cli;
 *   wtg_price_init(&cli, "mci-price.internal", 8082, 1000);
 *
 *   wtg_swap_req_t req = {
 *       .pair        = "USD/KRW",
 *       .near_tenor  = "SPOT",
 *       .far_tenor   = "1M",
 *       .profile     = "WEB.BRANCH.VIP",
 *       .customer_id = "C12345",
 *       .side        = "buy_sell",
 *       .amount      = 1000000,
 *   };
 *   wtg_swap_result_t res;
 *   int rc = wtg_price_swap_lock(&cli, &req, &res);
 *   if (rc == WTGPRICE_OK) {
 *       // res.swap_id / res.near.quote_id / res.far_.quote_id 매매 transaction 첨부
 *   }
 */

#ifndef WTGPRICE_H
#define WTGPRICE_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/* 반환 코드 — 0 = 성공, < 0 = 실패. */
#define WTGPRICE_OK              0
#define WTGPRICE_E_INVALID      -1   /* 인자 NULL 또는 필수 필드 누락 */
#define WTGPRICE_E_RESOLVE      -2   /* gethostbyname / getaddrinfo 실패 */
#define WTGPRICE_E_SOCKET       -3   /* socket() / setsockopt() 실패 */
#define WTGPRICE_E_CONNECT      -4   /* connect() 실패 (timeout 포함) */
#define WTGPRICE_E_SEND         -5   /* send() 실패 */
#define WTGPRICE_E_RECV         -6   /* recv() 실패 또는 timeout */
#define WTGPRICE_E_PARSE        -7   /* HTTP 응답 파싱 / JSON 추출 실패 */
#define WTGPRICE_E_HTTP_4XX     -8   /* HTTP 4xx (validation / NOT_FOUND 등) */
#define WTGPRICE_E_HTTP_5XX     -9   /* HTTP 5xx (UNAVAILABLE / 부분 실패) */
#define WTGPRICE_E_OVERSIZE    -10   /* 입력/응답 buffer 한계 초과 */

/* 필드 길이 상수 — server 측 형식과 일치. */
#define WTGPRICE_PAIR_LEN          16   /* "USD/KRW\0" */
#define WTGPRICE_QUOTEID_LEN       64
#define WTGPRICE_TENOR_LEN         16   /* "SPOT" / "1W" / "1M" ... */
#define WTGPRICE_VDATE_LEN         16   /* "2026-07-15\0" */
#define WTGPRICE_PROFILE_LEN       64
#define WTGPRICE_CUSTID_LEN        64
#define WTGPRICE_SIDE_LEN          16
#define WTGPRICE_SOURCE_LEN         8   /* "BEST" / "CROSS" */
#define WTGPRICE_ERRBODY_LEN      256
#define WTGPRICE_SPOT_MAX_PAIRS    16   /* 한 get_spot 호출의 최대 pair 수 */
#define WTGPRICE_SPOT_MAX_MISSING  16   /* 동일 — missing 도 같은 cap */

/* 클라이언트 컨텍스트. 한 번 init 후 여러 호출에 재사용. */
typedef struct {
    char host[256];
    int  port;
    int  timeout_ms;          /* connect + send + recv 단계별 timeout. 0=5000ms */
    int  last_http_status;    /* 마지막 호출의 HTTP status — 디버그용 */
    int  last_errno;          /* 마지막 호출의 errno (E_CONNECT 등에서 의미) */
    char last_error_body[WTGPRICE_ERRBODY_LEN];  /* 4xx/5xx 본문 일부 */
} wtg_price_client_t;

/* swap_lock 요청. tenor / value_date 중 한 쪽만 채움 (둘 다 채워도 server 가
 * value_date 우선). NULL 은 미지정. side / amount 는 audit metadata. */
typedef struct {
    const char *pair;            /* 필수. 예: "USD/KRW" / "100JPY/KRW" */
    const char *near_tenor;      /* 예: "SPOT" / "1W" */
    const char *near_value_date; /* 예: "2026-06-15" */
    const char *far_tenor;       /* 예: "1M" */
    const char *far_value_date;
    const char *profile;         /* 필수. 예: "WEB.BRANCH.VIP" */
    const char *customer_id;     /* NULL 가능 */
    const char *side;            /* "buy_sell" / "sell_buy" — NULL 가능 */
    double      amount;          /* 0.0 면 응답에 미포함 */
} wtg_swap_req_t;

/* 한 leg 결과. interpolation / swap_bid 등 audit-only 필드는 본 hot-path
 * SDK 에서 추출 생략 — 필요하면 raw_body 보존 옵션을 후속 phase 에 추가. */
typedef struct {
    char   quote_id[WTGPRICE_QUOTEID_LEN];
    char   tenor[WTGPRICE_TENOR_LEN];
    char   value_date[WTGPRICE_VDATE_LEN];
    double bid;       /* customer-applied (체결가) */
    double ask;
    double raw_bid;   /* 시장 BEST */
    double raw_ask;
} wtg_swap_leg_t;

/* swap_lock 응답. */
typedef struct {
    char            swap_id[WTGPRICE_QUOTEID_LEN];
    char            pair[WTGPRICE_PAIR_LEN];
    long long       issued_unix_nano;
    long long       valid_until_unix_nano;
    long long       table_version;
    wtg_swap_leg_t  near;
    wtg_swap_leg_t  far_;   /* 'far' 가 일부 컴파일러 매크로 — 안전한 이름 */
    double          bid_diff;
    double          ask_diff;
} wtg_swap_result_t;

/*
 * wtg_price_init — 클라이언트 초기화.
 *
 * @cli         NULL 아님.
 * @host        mci-price 의 hostname 또는 IPv4 (예: "mci-price.internal").
 * @port        포트 (보통 8082).
 * @timeout_ms  각 socket 단계 timeout (권장 1000~2000). 0 이면 5000.
 *
 * 반환: WTGPRICE_OK 또는 WTGPRICE_E_INVALID.
 */
int wtg_price_init(wtg_price_client_t *cli,
                   const char *host, int port, int timeout_ms);

/*
 * wtg_price_swap_lock — POST /v1/quote/swap/lock.
 *
 * @cli   init 된 클라이언트.
 * @req   요청. pair / profile + (near_tenor 또는 near_value_date) +
 *        (far_tenor 또는 far_value_date) 가 필수.
 * @out   결과 — WTGPRICE_OK 일 때만 신뢰. 호출 전 zero-init 권장.
 *
 * 반환: WTGPRICE_OK 또는 WTGPRICE_E_*.
 * 실패 시 cli->last_http_status / cli->last_errno / cli->last_error_body 참조.
 *
 * 정책: retry 금지. 본 SDK 는 단 1회 시도. timeout 도 거래 거부로 처리.
 */
int wtg_price_swap_lock(wtg_price_client_t *cli,
                        const wtg_swap_req_t *req,
                        wtg_swap_result_t *out);

/* spot 한 pair 의 호가 + 마진 + raw 시장가. server SpotSnapshotEntry 의
 * 필수 필드만 추출 — spread / raw_spread 는 호출자가 ask-bid 로 도출 가능. */
typedef struct {
    char    pair[WTGPRICE_PAIR_LEN];     /* "USD/KRW" */
    double  bid;                          /* customer-applied */
    double  ask;                          /* customer-applied */
    double  raw_bid;                      /* 시장 BEST bid */
    double  raw_ask;                      /* 시장 BEST ask */
    char    source[WTGPRICE_SOURCE_LEN]; /* "BEST" | "CROSS" */
} wtg_spot_entry_t;

/* spot 요청. pairs_csv 는 콤마 구분 ("USD/KRW,EUR/KRW"). cap 16. */
typedef struct {
    const char *pairs_csv;       /* 필수. 예: "USD/KRW,EUR/KRW" */
    const char *profile;         /* 필수. 예: "WEB.BRANCH.VIP" */
    const char *customer_id;     /* NULL 가능 — 5-Layer 적용 skip */
} wtg_spot_req_t;

/* spot 응답. 고정 cap 배열 — 동적 할당 0. */
typedef struct {
    long long          table_version;
    int                spot_count;                  /* spots[] 유효 길이 */
    wtg_spot_entry_t   spots[WTGPRICE_SPOT_MAX_PAIRS];
    int                missing_count;               /* missing[] 유효 길이 */
    char               missing[WTGPRICE_SPOT_MAX_MISSING][WTGPRICE_PAIR_LEN];
} wtg_spot_result_t;

/*
 * wtg_price_get_spot — GET /v1/quote/spot?pair=...&profile=...&customer_id=...
 *
 * 매칭 엔진 / 운영 svc 가 통화쌍의 현재 customer-applied bid/ask 를 1회 호출로
 * 조회. spot-only lite path — forward tenor 루프 / 봉 / 마진 audit 우회.
 * 다중 pair bulk (cap 16) 지원. 응답이 cap 을 넘으면 WTGPRICE_E_OVERSIZE.
 *
 * @cli   init 된 클라이언트.
 * @req   요청. pairs_csv + profile 필수. customer_id NULL 가능.
 * @out   결과 — WTGPRICE_OK 일 때만 신뢰. 호출 전 zero-init 권장.
 *
 * 반환: WTGPRICE_OK 또는 WTGPRICE_E_*.
 * 실패 시 cli->last_http_status / cli->last_errno / cli->last_error_body 참조.
 *
 * 정책: 응답이 idempotent 라 retry 가능하지만 본 SDK 는 swap_lock 과 일관성
 * 차원에서 1회 시도만. 호출자가 정책 결정.
 */
int wtg_price_get_spot(wtg_price_client_t *cli,
                       const wtg_spot_req_t *req,
                       wtg_spot_result_t *out);

/*
 * wtg_price_strerror — 반환 코드의 짧은 식별 메시지. static const,
 * 호출자가 free 안 함, thread-safe.
 */
const char *wtg_price_strerror(int code);


/* ====================== 수동 스왑포인트 등록 (W2006A01 대체) ======================
 * trn 딜러 화면의 스왑포인트 등록/해제 — mds W9504A01 tp call 을
 * POST /v1/pricing/swap (mci-price) 직결로 대체한다. 반영은 etcd pricing
 * doc CAS write → 전 mci-price 인스턴스 hot reload.
 * tenor 는 WTG 표기 ("1W"/"1M"/"2M"/"3M"/"6M"/"1Y"/"SPT"/"TOD"/"TOM"). */
typedef struct {
    char   tenor[8];
    double bid;
    double ask;
} wtg_swap_point_t;

/* 등록/갱신 — points×npoint upsert. 성공 = WTGPRICE_OK. */
int wtg_price_swap_point_set(wtg_price_client_t *cli, const char *pair,
                             const wtg_swap_point_t *points, int npoint);

/* 해제 — pair 의 수동 스왑포인트 전체 삭제 (mds regTp=2 동등). */
int wtg_price_swap_point_clear(wtg_price_client_t *cli, const char *pair);


/* 시장 시세 생존 게이트 — WTR005 의 mds market_get/sendclient_flag 대체.
 * open_out: 1=시세 전송 중 / 0=중단. 반환 WTGPRICE_OK 외엔 판정 불가 (호출측
 * 이 보수적으로 주문 거부 또는 통과 정책 결정). pair 는 NULL 허용 (시장 전체). */
int wtg_price_market_open(wtg_price_client_t *cli, const char *market,
                          const char *pair, int *open_out);

#ifdef __cplusplus
}
#endif

#endif /* WTGPRICE_H */
