/*
 * wtgprice.c — POSIX socket + HTTP/1.1 + 간이 JSON 추출.
 *
 * 설계:
 *   · 호출마다 connect/close — connection pool 없음. swap_lock 빈도는
 *     체결당 1회이므로 충분.
 *   · IPv4 only. IPv6 필요 시 getaddrinfo path 후속.
 *   · JSON 응답은 mci-price 가 표준 Go encoding/json 으로 생성 — 잘 정의됨.
 *     본 SDK 의 간이 파서는 다음 가정:
 *       · whitespace 는 어디든 허용
 *       · escape 는 단순 ('\\' 다음 1바이트 그대로 출력)
 *       · unicode escape (\uXXXX) 미처리 — 식별자 필드엔 안 나옴
 *       · nested object 깊이 카운트로 span 결정
 */

#include "wtgprice.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <ctype.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <netdb.h>

#define DEFAULT_TIMEOUT_MS  5000
#define REQ_HEADER_MAX      1024
#define REQ_BODY_MAX        (4 * 1024)   /* swap req JSON 최대 */
#define RESP_BUF            (16 * 1024)  /* 응답 본문 한계 */

/* ====================== socket / HTTP 토대 ====================== */

static int resolve_host(const char *host, struct in_addr *out_ip) {
    struct in_addr ip;
    if (inet_pton(AF_INET, host, &ip) == 1) {
        *out_ip = ip;
        return 0;
    }
    struct hostent *he = gethostbyname(host);
    if (he == NULL || he->h_addrtype != AF_INET || he->h_length == 0) {
        return -1;
    }
    memcpy(out_ip, he->h_addr_list[0], sizeof(struct in_addr));
    return 0;
}

static int connect_timeout(int sock, const struct sockaddr *addr, socklen_t alen,
                           int timeout_ms) {
    int flags = fcntl(sock, F_GETFL, 0);
    if (flags < 0) return -1;
    if (fcntl(sock, F_SETFL, flags | O_NONBLOCK) < 0) return -1;

    int rc = connect(sock, addr, alen);
    if (rc == 0) {
        fcntl(sock, F_SETFL, flags);
        return 0;
    }
    if (errno != EINPROGRESS) return -1;

    fd_set wfds;
    FD_ZERO(&wfds);
    FD_SET(sock, &wfds);
    struct timeval tv;
    tv.tv_sec  = timeout_ms / 1000;
    tv.tv_usec = (timeout_ms % 1000) * 1000;

    rc = select(sock + 1, NULL, &wfds, NULL, &tv);
    if (rc <= 0) {
        errno = (rc == 0) ? ETIMEDOUT : errno;
        return -1;
    }
    int sockerr = 0;
    socklen_t slen = sizeof(sockerr);
    if (getsockopt(sock, SOL_SOCKET, SO_ERROR, &sockerr, &slen) < 0) return -1;
    if (sockerr != 0) {
        errno = sockerr;
        return -1;
    }
    fcntl(sock, F_SETFL, flags);
    return 0;
}

static int send_all(int sock, const char *buf, size_t len) {
    size_t off = 0;
    while (off < len) {
        ssize_t n = send(sock, buf + off, len - off, 0);
        if (n < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        off += (size_t)n;
    }
    return 0;
}

/* recv_all — 응답 전체 (헤더 + 본문) 를 connection close 까지 누적.
 * 반환: 누적 길이. 음수 = 실패. */
static int recv_all(int sock, char *buf, size_t cap) {
    size_t off = 0;
    while (off + 1 < cap) {
        ssize_t n = recv(sock, buf + off, cap - 1 - off, 0);
        if (n == 0) break;        /* peer close */
        if (n < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        off += (size_t)n;
    }
    buf[off] = 0;
    return (int)off;
}

/* HTTP status line 파싱. */
static int parse_http_status(const char *buf) {
    if (strncmp(buf, "HTTP/1.", 7) != 0) return -1;
    const char *p = strchr(buf, ' ');
    if (p == NULL) return -1;
    return atoi(p + 1);
}

/* 응답 본문 시작 — "\r\n\r\n" 다음. NULL 이면 헤더만 받음. */
static const char *http_body(const char *resp) {
    const char *p = strstr(resp, "\r\n\r\n");
    return p ? (p + 4) : NULL;
}

/* ====================== 간이 JSON 파서 ====================== */

/* skip whitespace. */
static const char *skip_ws(const char *p) {
    while (*p && (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')) p++;
    return p;
}

/* key 검색 — body 의 현재 object 컨텍스트에서 "key": 의 ':' 다음 위치 반환.
 * body 가 NULL-terminated 라 가정. body 는 '{' 자체 또는 그 직후 또는 그 안의
 * 어디든 — 진입 시 whitespace skip + 선두 '{' 자동 건너뜀 (top-level 진입 케이스).
 * nested object/array 는 깊이 카운트로 skip — 같은 depth 의 key 만 매칭.
 * 반환: 값의 시작 위치 (whitespace 이후), 또는 NULL. */
static const char *json_find_key(const char *body, const char *key) {
    const char *p = skip_ws(body);
    if (*p == '{') p++;   /* top-level object 컨텍스트 진입 — 이후의 '{' 는 nested */
    size_t klen = strlen(key);
    int depth = 0;
    int in_string = 0;
    int escape = 0;

    while (*p) {
        if (escape) { escape = 0; p++; continue; }
        if (in_string) {
            if (*p == '\\') { escape = 1; p++; continue; }
            if (*p == '"') { in_string = 0; p++; continue; }
            p++;
            continue;
        }
        if (*p == '"') {
            /* 문자열 시작 — depth==0 이면 key 후보. */
            if (depth == 0) {
                const char *kstart = p + 1;
                const char *kend = kstart;
                while (*kend && *kend != '"') {
                    if (*kend == '\\' && kend[1]) kend += 2;
                    else kend++;
                }
                if (*kend != '"') return NULL;
                /* match? */
                if ((size_t)(kend - kstart) == klen &&
                    strncmp(kstart, key, klen) == 0) {
                    /* skip closing quote + ws + ':' + ws */
                    p = kend + 1;
                    p = skip_ws(p);
                    if (*p != ':') return NULL;
                    p++;
                    p = skip_ws(p);
                    return p;
                }
                p = kend + 1;
                continue;
            }
            in_string = 1;
            p++;
            continue;
        }
        if (*p == '{' || *p == '[') { depth++; p++; continue; }
        if (*p == '}' || *p == ']') {
            if (depth == 0) return NULL; /* object 끝 */
            depth--;
            p++;
            continue;
        }
        p++;
    }
    return NULL;
}

/* 값 = 문자열 — out 에 NUL-terminated 로 복사. escape 단순 처리. */
static int json_read_string(const char *val, char *out, size_t outsz) {
    if (*val != '"') return -1;
    val++;
    size_t i = 0;
    while (*val && *val != '"') {
        if (i + 1 >= outsz) return -1;
        if (*val == '\\' && val[1]) {
            char c = val[1];
            switch (c) {
            case 'n': out[i++] = '\n'; break;
            case 't': out[i++] = '\t'; break;
            case 'r': out[i++] = '\r'; break;
            case '\\': out[i++] = '\\'; break;
            case '/': out[i++] = '/'; break;
            case '"': out[i++] = '"'; break;
            default:  out[i++] = c; break;
            }
            val += 2;
        } else {
            out[i++] = *val++;
        }
    }
    if (*val != '"') return -1;
    out[i] = 0;
    return 0;
}

/* 값 = number. JSON 의 number 형식 단순 처리. */
static int json_read_double(const char *val, double *out) {
    char *end = NULL;
    double v = strtod(val, &end);
    if (end == val) return -1;
    *out = v;
    return 0;
}

static int json_read_int64(const char *val, long long *out) {
    char *end = NULL;
    long long v = strtoll(val, &end, 10);
    if (end == val) return -1;
    *out = v;
    return 0;
}

/* 값 = object — 시작 '{' 직후 위치를 out_inside 에 반환.
 * 그 위치에서 json_find_key 를 호출하면 sub-object 의 같은 depth key 검색됨.
 * out_inside 가 NULL 이면 단순 발견 여부만. */
static int json_read_object(const char *val, const char **out_inside) {
    val = skip_ws(val);
    if (*val != '{') return -1;
    if (out_inside) *out_inside = val + 1;
    return 0;
}

/* helper — top-level (또는 주어진 컨텍스트) 에서 string 필드 추출. */
static int extract_string(const char *ctx, const char *key, char *out, size_t outsz) {
    const char *v = json_find_key(ctx, key);
    if (v == NULL) return -1;
    return json_read_string(v, out, outsz);
}

static int extract_double(const char *ctx, const char *key, double *out) {
    const char *v = json_find_key(ctx, key);
    if (v == NULL) return -1;
    return json_read_double(v, out);
}

static int extract_int64(const char *ctx, const char *key, long long *out) {
    const char *v = json_find_key(ctx, key);
    if (v == NULL) return -1;
    return json_read_int64(v, out);
}

/* leg 추출 — sub-object 진입 후 필수 필드 5개 + 옵션 2개. */
static int extract_leg(const char *ctx, const char *leg_key, wtg_swap_leg_t *out) {
    const char *v = json_find_key(ctx, leg_key);
    if (v == NULL) return -1;
    const char *inside = NULL;
    if (json_read_object(v, &inside) < 0) return -1;
    memset(out, 0, sizeof(*out));
    if (extract_string(inside, "quote_id", out->quote_id, sizeof(out->quote_id)) < 0) return -1;
    if (extract_string(inside, "tenor",    out->tenor,    sizeof(out->tenor)) < 0)    return -1;
    /* value_date 는 broken-date 일 때만 채워짐 — 미존재는 무시. */
    (void)extract_string(inside, "value_date", out->value_date, sizeof(out->value_date));
    if (extract_double(inside, "bid", &out->bid) < 0) return -1;
    if (extract_double(inside, "ask", &out->ask) < 0) return -1;
    if (extract_double(inside, "raw_bid", &out->raw_bid) < 0) return -1;
    if (extract_double(inside, "raw_ask", &out->raw_ask) < 0) return -1;
    return 0;
}

/* swap_diff 추출 — sub-object. 미존재면 0/0. */
static void extract_swap_diff(const char *ctx, double *bid_diff, double *ask_diff) {
    const char *v = json_find_key(ctx, "swap_diff");
    if (v == NULL) { *bid_diff = 0; *ask_diff = 0; return; }
    const char *inside = NULL;
    if (json_read_object(v, &inside) < 0) { *bid_diff = 0; *ask_diff = 0; return; }
    (void)extract_double(inside, "bid_diff", bid_diff);
    (void)extract_double(inside, "ask_diff", ask_diff);
}

/* ====================== TCP round-trip 공통 ====================== */

/* tcp_round_trip — connect + send + recv 전체. resp 는 NULL-terminated.
 * 반환: 누적 응답 길이 (>0) 또는 음수 WTGPRICE_E_*. cli->last_errno 만 갱신.
 * swap_lock / get_spot 양쪽에서 재사용. */
static int tcp_round_trip(wtg_price_client_t *cli,
                          const char *req, size_t req_len,
                          char *resp, size_t resp_cap) {
    struct in_addr ip;
    if (resolve_host(cli->host, &ip) < 0) {
        cli->last_errno = errno;
        return WTGPRICE_E_RESOLVE;
    }
    int sock = socket(AF_INET, SOCK_STREAM, 0);
    if (sock < 0) {
        cli->last_errno = errno;
        return WTGPRICE_E_SOCKET;
    }
    int one = 1;
    (void)setsockopt(sock, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

    struct timeval tv;
    tv.tv_sec  = cli->timeout_ms / 1000;
    tv.tv_usec = (cli->timeout_ms % 1000) * 1000;
    (void)setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
    (void)setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port   = htons((uint16_t)cli->port);
    addr.sin_addr   = ip;

    if (connect_timeout(sock, (struct sockaddr *)&addr, sizeof(addr), cli->timeout_ms) < 0) {
        cli->last_errno = errno;
        close(sock);
        return WTGPRICE_E_CONNECT;
    }
    if (send_all(sock, req, req_len) < 0) {
        cli->last_errno = errno;
        close(sock);
        return WTGPRICE_E_SEND;
    }
    int n = recv_all(sock, resp, resp_cap);
    close(sock);
    if (n <= 0) {
        cli->last_errno = errno;
        return WTGPRICE_E_RECV;
    }
    return n;
}

/* http_status_and_body — 응답에서 status 추출 + body 포인터 반환.
 * 4xx/5xx 시 cli->last_error_body 에 본문 보존. status_out 채움.
 * 반환: WTGPRICE_OK 면 body_out 유효. 그 외 음수 WTGPRICE_E_*. */
static int http_status_and_body(wtg_price_client_t *cli, const char *resp,
                                int *status_out, const char **body_out) {
    int status = parse_http_status(resp);
    if (status < 0) return WTGPRICE_E_PARSE;
    cli->last_http_status = status;
    *status_out = status;

    const char *bptr = http_body(resp);
    if (bptr == NULL) return WTGPRICE_E_PARSE;
    *body_out = bptr;

    if (status >= 400) {
        size_t blen = strlen(bptr);
        if (blen >= sizeof(cli->last_error_body)) blen = sizeof(cli->last_error_body) - 1;
        memcpy(cli->last_error_body, bptr, blen);
        cli->last_error_body[blen] = 0;
        return (status >= 500) ? WTGPRICE_E_HTTP_5XX : WTGPRICE_E_HTTP_4XX;
    }
    if (status != 200) return WTGPRICE_E_PARSE;
    return WTGPRICE_OK;
}

/* ====================== 본 API ====================== */

int wtg_price_init(wtg_price_client_t *cli, const char *host, int port, int timeout_ms) {
    if (cli == NULL || host == NULL || *host == 0 || port <= 0) {
        return WTGPRICE_E_INVALID;
    }
    memset(cli, 0, sizeof(*cli));
    size_t hlen = strlen(host);
    if (hlen + 1 > sizeof(cli->host)) return WTGPRICE_E_INVALID;
    memcpy(cli->host, host, hlen + 1);
    cli->port = port;
    cli->timeout_ms = (timeout_ms > 0) ? timeout_ms : DEFAULT_TIMEOUT_MS;
    return WTGPRICE_OK;
}

/* JSON 문자열 escape — 매우 단순. '"' / '\\' / control char 처리. 본 SDK 의
 * 모든 사용자 입력은 식별자 / pair / profile / customer_id 라 안전. */
static int json_escape_into(char *dst, size_t dstsz, const char *src) {
    size_t i = 0;
    while (*src) {
        char c = *src++;
        if (c == '"' || c == '\\') {
            if (i + 2 >= dstsz) return -1;
            dst[i++] = '\\';
            dst[i++] = c;
            continue;
        }
        if ((unsigned char)c < 0x20) {
            int n = snprintf(dst + i, dstsz - i, "\\u%04x", (unsigned)c);
            if (n < 0 || (size_t)n >= dstsz - i) return -1;
            i += (size_t)n;
            continue;
        }
        if (i + 1 >= dstsz) return -1;
        dst[i++] = c;
    }
    dst[i] = 0;
    return 0;
}

/* request body 조립. amount > 0 만 포함. */
static int build_swap_req_body(const wtg_swap_req_t *req, char *body, size_t cap) {
    if (req->pair == NULL || *req->pair == 0) return -1;
    if (req->profile == NULL || *req->profile == 0) return -1;

    char pair_esc[64], profile_esc[128], custid_esc[128], side_esc[32];
    char near_tenor_esc[32], near_vd_esc[32];
    char far_tenor_esc[32], far_vd_esc[32];
    pair_esc[0] = profile_esc[0] = custid_esc[0] = side_esc[0] = 0;
    near_tenor_esc[0] = near_vd_esc[0] = far_tenor_esc[0] = far_vd_esc[0] = 0;

    if (json_escape_into(pair_esc, sizeof(pair_esc), req->pair) < 0) return -1;
    if (json_escape_into(profile_esc, sizeof(profile_esc), req->profile) < 0) return -1;
    if (req->customer_id && *req->customer_id) {
        if (json_escape_into(custid_esc, sizeof(custid_esc), req->customer_id) < 0) return -1;
    }
    if (req->side && *req->side) {
        if (json_escape_into(side_esc, sizeof(side_esc), req->side) < 0) return -1;
    }
    if (req->near_tenor && *req->near_tenor)
        if (json_escape_into(near_tenor_esc, sizeof(near_tenor_esc), req->near_tenor) < 0) return -1;
    if (req->near_value_date && *req->near_value_date)
        if (json_escape_into(near_vd_esc, sizeof(near_vd_esc), req->near_value_date) < 0) return -1;
    if (req->far_tenor && *req->far_tenor)
        if (json_escape_into(far_tenor_esc, sizeof(far_tenor_esc), req->far_tenor) < 0) return -1;
    if (req->far_value_date && *req->far_value_date)
        if (json_escape_into(far_vd_esc, sizeof(far_vd_esc), req->far_value_date) < 0) return -1;

    /* near / far sub-object 조립. */
    char near_obj[128], far_obj[128];
    if (near_vd_esc[0]) {
        snprintf(near_obj, sizeof(near_obj), "{\"value_date\":\"%s\"}", near_vd_esc);
    } else if (near_tenor_esc[0]) {
        snprintf(near_obj, sizeof(near_obj), "{\"tenor\":\"%s\"}", near_tenor_esc);
    } else {
        return -1; /* near 필수 */
    }
    if (far_vd_esc[0]) {
        snprintf(far_obj, sizeof(far_obj), "{\"value_date\":\"%s\"}", far_vd_esc);
    } else if (far_tenor_esc[0]) {
        snprintf(far_obj, sizeof(far_obj), "{\"tenor\":\"%s\"}", far_tenor_esc);
    } else {
        return -1;
    }

    int n = snprintf(body, cap,
        "{\"pair\":\"%s\","
        "\"near\":%s,"
        "\"far\":%s,"
        "\"profile\":\"%s\""
        "%s%s%s"
        "%s%s%s"
        "%s",
        pair_esc, near_obj, far_obj, profile_esc,
        custid_esc[0] ? ",\"customer_id\":\"" : "",
        custid_esc[0] ? custid_esc : "",
        custid_esc[0] ? "\"" : "",
        side_esc[0] ? ",\"side\":\"" : "",
        side_esc[0] ? side_esc : "",
        side_esc[0] ? "\"" : "",
        ""
    );
    if (n < 0 || (size_t)n >= cap) return -1;
    if (req->amount > 0.0) {
        int n2 = snprintf(body + n, cap - n, ",\"amount\":%.10g}", req->amount);
        if (n2 < 0 || (size_t)(n + n2) >= cap) return -1;
        return n + n2;
    }
    /* close. 위 snprintf 가 닫는 '}' 안 넣었음. */
    if ((size_t)n + 1 >= cap) return -1;
    body[n++] = '}';
    body[n] = 0;
    return n;
}

int wtg_price_swap_lock(wtg_price_client_t *cli, const wtg_swap_req_t *req,
                        wtg_swap_result_t *out) {
    if (cli == NULL || req == NULL || out == NULL) return WTGPRICE_E_INVALID;
    cli->last_http_status = 0;
    cli->last_errno = 0;
    cli->last_error_body[0] = 0;
    memset(out, 0, sizeof(*out));

    /* 1. body. */
    char body[REQ_BODY_MAX];
    int body_len = build_swap_req_body(req, body, sizeof(body));
    if (body_len < 0) return WTGPRICE_E_INVALID;

    /* 2. header + body 를 한 buffer 에 모아 1회 send. */
    char request[REQ_HEADER_MAX + REQ_BODY_MAX];
    int hlen = snprintf(request, sizeof(request),
        "POST /v1/quote/swap/lock HTTP/1.1\r\n"
        "Host: %s:%d\r\n"
        "Content-Type: application/json\r\n"
        "Content-Length: %d\r\n"
        "Connection: close\r\n"
        "\r\n",
        cli->host, cli->port, body_len);
    if (hlen < 0 || hlen >= (int)sizeof(request)) return WTGPRICE_E_OVERSIZE;
    if ((size_t)(hlen + body_len) >= sizeof(request)) return WTGPRICE_E_OVERSIZE;
    memcpy(request + hlen, body, (size_t)body_len);
    int req_len = hlen + body_len;

    /* 3. round-trip. */
    char *resp = (char *)malloc(RESP_BUF);
    if (resp == NULL) return WTGPRICE_E_INVALID;
    int n = tcp_round_trip(cli, request, (size_t)req_len, resp, RESP_BUF);
    if (n < 0) { free(resp); return n; }

    /* 4. status + body. */
    int status;
    const char *bptr = NULL;
    int rc = http_status_and_body(cli, resp, &status, &bptr);
    if (rc != WTGPRICE_OK) { free(resp); return rc; }

    /* 5. JSON 추출 — 필수 필드 전부 OK 여야 성공. */
    if (extract_string(bptr, "swap_id", out->swap_id, sizeof(out->swap_id)) < 0) {
        free(resp); return WTGPRICE_E_PARSE;
    }
    (void)extract_string(bptr, "pair", out->pair, sizeof(out->pair));
    if (extract_int64(bptr, "issued_unix_nano", &out->issued_unix_nano) < 0 ||
        extract_int64(bptr, "valid_until_unix_nano", &out->valid_until_unix_nano) < 0 ||
        extract_int64(bptr, "table_version", &out->table_version) < 0) {
        free(resp); return WTGPRICE_E_PARSE;
    }
    if (extract_leg(bptr, "near", &out->near) < 0 ||
        extract_leg(bptr, "far",  &out->far_) < 0) {
        free(resp); return WTGPRICE_E_PARSE;
    }
    extract_swap_diff(bptr, &out->bid_diff, &out->ask_diff);
    free(resp);
    return WTGPRICE_OK;
}

/* ====================== get_spot ====================== */

/* URL query value escape — RFC 3986 unreserved + 매매에 안전한 ',' '/' 통과.
 * 식별자 / pair / profile / customer_id 는 모두 ASCII 라 단순 처리 가능. */
static int url_escape_into(char *dst, size_t dstsz, const char *src) {
    static const char hex[] = "0123456789ABCDEF";
    size_t i = 0;
    while (*src) {
        unsigned char c = (unsigned char)*src++;
        int safe = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                   (c >= '0' && c <= '9') ||
                   c == '-' || c == '.' || c == '_' || c == '~' ||
                   c == ',' || c == '/';
        if (safe) {
            if (i + 1 >= dstsz) return -1;
            dst[i++] = (char)c;
        } else {
            if (i + 3 >= dstsz) return -1;
            dst[i++] = '%';
            dst[i++] = hex[c >> 4];
            dst[i++] = hex[c & 0xF];
        }
    }
    dst[i] = 0;
    return 0;
}

/* JSON object span skip — '{' 직후 위치에서 같은 depth 의 '}' 까지 진행한
 * 다음 위치 반환. nested object / string / escape 안전. NULL = malformed. */
static const char *json_skip_object_body(const char *p) {
    int depth = 0, in_str = 0, esc = 0;
    while (*p) {
        if (esc) { esc = 0; p++; continue; }
        if (in_str) {
            if (*p == '\\') esc = 1;
            else if (*p == '"') in_str = 0;
            p++;
            continue;
        }
        if (*p == '"') { in_str = 1; p++; continue; }
        if (*p == '{' || *p == '[') { depth++; p++; continue; }
        if (*p == '}' || *p == ']') {
            if (depth == 0) return p + 1;
            depth--;
            p++;
            continue;
        }
        p++;
    }
    return NULL;
}

/* JSON string span skip — '"' 직후 위치에서 종료 '"' 다음 위치 반환. */
static const char *json_skip_string_body(const char *p) {
    while (*p) {
        if (*p == '\\' && p[1]) { p += 2; continue; }
        if (*p == '"') return p + 1;
        p++;
    }
    return NULL;
}

int wtg_price_get_spot(wtg_price_client_t *cli, const wtg_spot_req_t *req,
                       wtg_spot_result_t *out) {
    if (cli == NULL || req == NULL || out == NULL) return WTGPRICE_E_INVALID;
    if (req->pairs_csv == NULL || *req->pairs_csv == 0) return WTGPRICE_E_INVALID;
    if (req->profile   == NULL || *req->profile   == 0) return WTGPRICE_E_INVALID;
    cli->last_http_status = 0;
    cli->last_errno = 0;
    cli->last_error_body[0] = 0;
    memset(out, 0, sizeof(*out));

    /* 1. query 인자 escape. */
    char pairs_esc[512], profile_esc[256], custid_esc[256];
    custid_esc[0] = 0;
    if (url_escape_into(pairs_esc,   sizeof(pairs_esc),   req->pairs_csv) < 0)
        return WTGPRICE_E_OVERSIZE;
    if (url_escape_into(profile_esc, sizeof(profile_esc), req->profile) < 0)
        return WTGPRICE_E_OVERSIZE;
    if (req->customer_id && *req->customer_id) {
        if (url_escape_into(custid_esc, sizeof(custid_esc), req->customer_id) < 0)
            return WTGPRICE_E_OVERSIZE;
    }

    /* 2. GET request — body 없음. */
    char request[REQ_HEADER_MAX + 1024];
    int rlen;
    if (custid_esc[0]) {
        rlen = snprintf(request, sizeof(request),
            "GET /v1/quote/spot?pair=%s&profile=%s&customer_id=%s HTTP/1.1\r\n"
            "Host: %s:%d\r\n"
            "Connection: close\r\n"
            "\r\n",
            pairs_esc, profile_esc, custid_esc, cli->host, cli->port);
    } else {
        rlen = snprintf(request, sizeof(request),
            "GET /v1/quote/spot?pair=%s&profile=%s HTTP/1.1\r\n"
            "Host: %s:%d\r\n"
            "Connection: close\r\n"
            "\r\n",
            pairs_esc, profile_esc, cli->host, cli->port);
    }
    if (rlen < 0 || rlen >= (int)sizeof(request)) return WTGPRICE_E_OVERSIZE;

    /* 3. round-trip. */
    char *resp = (char *)malloc(RESP_BUF);
    if (resp == NULL) return WTGPRICE_E_INVALID;
    int n = tcp_round_trip(cli, request, (size_t)rlen, resp, RESP_BUF);
    if (n < 0) { free(resp); return n; }

    /* 4. status + body. */
    int status;
    const char *bptr = NULL;
    int rc = http_status_and_body(cli, resp, &status, &bptr);
    if (rc != WTGPRICE_OK) { free(resp); return rc; }

    /* 5. top-level. table_version 만 필수 — profile / snapshot_ts 는 운영자
     *    audit 용이라 SDK 에서 추출 생략. */
    if (extract_int64(bptr, "table_version", &out->table_version) < 0) {
        free(resp); return WTGPRICE_E_PARSE;
    }

    /* 6. spots[] iteration. */
    const char *spots_v = json_find_key(bptr, "spots");
    if (spots_v == NULL) { free(resp); return WTGPRICE_E_PARSE; }
    spots_v = skip_ws(spots_v);
    if (*spots_v != '[') { free(resp); return WTGPRICE_E_PARSE; }
    const char *cur = spots_v + 1;
    for (;;) {
        cur = skip_ws(cur);
        if (*cur == ']') { cur++; break; }
        if (*cur == ',') { cur = skip_ws(cur + 1); }
        if (*cur != '{') { free(resp); return WTGPRICE_E_PARSE; }
        if (out->spot_count >= WTGPRICE_SPOT_MAX_PAIRS) {
            free(resp); return WTGPRICE_E_OVERSIZE;
        }
        wtg_spot_entry_t *e = &out->spots[out->spot_count];
        memset(e, 0, sizeof(*e));
        const char *inside = cur + 1;   /* '{' 직후 */
        if (extract_string(inside, "pair", e->pair, sizeof(e->pair)) < 0 ||
            extract_double(inside, "bid",     &e->bid)     < 0 ||
            extract_double(inside, "ask",     &e->ask)     < 0 ||
            extract_double(inside, "raw_bid", &e->raw_bid) < 0 ||
            extract_double(inside, "raw_ask", &e->raw_ask) < 0) {
            free(resp); return WTGPRICE_E_PARSE;
        }
        (void)extract_string(inside, "source", e->source, sizeof(e->source));
        out->spot_count++;
        /* 이 entry 의 '}' 다음으로 진행. */
        cur = json_skip_object_body(cur + 1);
        if (cur == NULL) { free(resp); return WTGPRICE_E_PARSE; }
    }

    /* 7. missing[] — string array, omitempty (없을 수 있음). */
    const char *miss_v = json_find_key(bptr, "missing");
    if (miss_v != NULL) {
        miss_v = skip_ws(miss_v);
        if (*miss_v == '[') {
            const char *mcur = miss_v + 1;
            for (;;) {
                mcur = skip_ws(mcur);
                if (*mcur == ']') break;
                if (*mcur == ',') { mcur = skip_ws(mcur + 1); }
                if (*mcur != '"') { free(resp); return WTGPRICE_E_PARSE; }
                if (out->missing_count >= WTGPRICE_SPOT_MAX_MISSING) {
                    free(resp); return WTGPRICE_E_OVERSIZE;
                }
                if (json_read_string(mcur, out->missing[out->missing_count],
                                     sizeof(out->missing[0])) < 0) {
                    free(resp); return WTGPRICE_E_PARSE;
                }
                out->missing_count++;
                mcur = json_skip_string_body(mcur + 1);
                if (mcur == NULL) { free(resp); return WTGPRICE_E_PARSE; }
            }
        }
    }

    free(resp);
    return WTGPRICE_OK;
}

const char *wtg_price_strerror(int code) {
    switch (code) {
    case WTGPRICE_OK:           return "ok";
    case WTGPRICE_E_INVALID:    return "invalid argument";
    case WTGPRICE_E_RESOLVE:    return "host resolve failed";
    case WTGPRICE_E_SOCKET:     return "socket() failed";
    case WTGPRICE_E_CONNECT:    return "connect timeout or refused";
    case WTGPRICE_E_SEND:       return "send() failed";
    case WTGPRICE_E_RECV:       return "recv() failed or timeout";
    case WTGPRICE_E_PARSE:      return "response parse failed";
    case WTGPRICE_E_HTTP_4XX:   return "HTTP 4xx (validation / not found)";
    case WTGPRICE_E_HTTP_5XX:   return "HTTP 5xx (unavailable / partial failure)";
    case WTGPRICE_E_OVERSIZE:   return "buffer oversize";
    default:                    return "unknown error";
    }
}
