/*
 * wtgquery.c — POSIX socket + HTTP/1.1 GET /v1/chart + JSON 응답 →
 *   W9501S01_out_t 채움.
 *
 * 설계: cside/wtgprice 와 동일 — 외부 의존 0, 1회 connect/close, IPv4,
 *   간이 JSON 파서. 각 cside lib 는 자립 가능해야 하므로 wtgprice 의 helper
 *   를 복제 (link 공유 X — 운영 C 코드가 한 lib 만 link 해도 동작해야 함).
 */

#include "wtgquery.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <ctype.h>
#include <time.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <netdb.h>

#define DEFAULT_TIMEOUT_MS  5000
#define REQ_BUF_MAX         2048
#define RESP_BUF            (64 * 1024)   /* 봉 16개 응답 충분 */

/* ====================== socket / HTTP 토대 (wtgprice 동일) ====================== */

static int resolve_host(const char *host, struct in_addr *out_ip) {
    struct in_addr ip;
    if (inet_pton(AF_INET, host, &ip) == 1) { *out_ip = ip; return 0; }
    struct hostent *he = gethostbyname(host);
    if (he == NULL || he->h_addrtype != AF_INET || he->h_length == 0) return -1;
    memcpy(out_ip, he->h_addr_list[0], sizeof(struct in_addr));
    return 0;
}

static int connect_timeout(int sock, const struct sockaddr *addr, socklen_t alen, int timeout_ms) {
    int flags = fcntl(sock, F_GETFL, 0);
    if (flags < 0) return -1;
    if (fcntl(sock, F_SETFL, flags | O_NONBLOCK) < 0) return -1;
    int rc = connect(sock, addr, alen);
    if (rc == 0) { fcntl(sock, F_SETFL, flags); return 0; }
    if (errno != EINPROGRESS) return -1;
    fd_set wfds; FD_ZERO(&wfds); FD_SET(sock, &wfds);
    struct timeval tv; tv.tv_sec = timeout_ms / 1000; tv.tv_usec = (timeout_ms % 1000) * 1000;
    rc = select(sock + 1, NULL, &wfds, NULL, &tv);
    if (rc <= 0) { errno = (rc == 0) ? ETIMEDOUT : errno; return -1; }
    int sockerr = 0; socklen_t slen = sizeof(sockerr);
    if (getsockopt(sock, SOL_SOCKET, SO_ERROR, &sockerr, &slen) < 0) return -1;
    if (sockerr != 0) { errno = sockerr; return -1; }
    fcntl(sock, F_SETFL, flags);
    return 0;
}

static int send_all(int sock, const char *buf, size_t len) {
    size_t off = 0;
    while (off < len) {
        ssize_t n = send(sock, buf + off, len - off, 0);
        if (n < 0) { if (errno == EINTR) continue; return -1; }
        off += (size_t)n;
    }
    return 0;
}

static int recv_all(int sock, char *buf, size_t cap) {
    size_t off = 0;
    while (off + 1 < cap) {
        ssize_t n = recv(sock, buf + off, cap - 1 - off, 0);
        if (n == 0) break;
        if (n < 0) { if (errno == EINTR) continue; return -1; }
        off += (size_t)n;
    }
    buf[off] = 0;
    return (int)off;
}

static int parse_http_status(const char *buf) {
    if (strncmp(buf, "HTTP/1.", 7) != 0) return -1;
    const char *p = strchr(buf, ' ');
    return p ? atoi(p + 1) : -1;
}

static const char *http_body(const char *resp) {
    const char *p = strstr(resp, "\r\n\r\n");
    return p ? (p + 4) : NULL;
}

/* ====================== 간이 JSON 파서 (wtgprice 동일) ====================== */

static const char *skip_ws(const char *p) {
    while (*p && (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')) p++;
    return p;
}

/* depth=0 컨텍스트에서 "key": 의 ':' 다음 위치 반환. nested skip. */
static const char *json_find_key(const char *body, const char *key) {
    const char *p = skip_ws(body);
    if (*p == '{') p++;
    size_t klen = strlen(key);
    int depth = 0, in_string = 0, escape = 0;
    while (*p) {
        if (escape) { escape = 0; p++; continue; }
        if (in_string) {
            if (*p == '\\') { escape = 1; p++; continue; }
            if (*p == '"') { in_string = 0; p++; continue; }
            p++; continue;
        }
        if (*p == '"') {
            if (depth == 0) {
                const char *kstart = p + 1;
                const char *kend = kstart;
                while (*kend && *kend != '"') {
                    if (*kend == '\\' && kend[1]) kend += 2;
                    else kend++;
                }
                if (*kend != '"') return NULL;
                if ((size_t)(kend - kstart) == klen && strncmp(kstart, key, klen) == 0) {
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
            p++; continue;
        }
        if (*p == '{' || *p == '[') { depth++; p++; continue; }
        if (*p == '}' || *p == ']') {
            if (depth == 0) return NULL;
            depth--; p++; continue;
        }
        p++;
    }
    return NULL;
}

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
            default: out[i++] = c; break;
            }
            val += 2;
        } else { out[i++] = *val++; }
    }
    if (*val != '"') return -1;
    out[i] = 0;
    return 0;
}

static int json_read_double(const char *val, double *out) {
    char *end = NULL;
    double v = strtod(val, &end);
    if (end == val) return -1;
    *out = v;
    return 0;
}

static int extract_string(const char *ctx, const char *key, char *out, size_t outsz) {
    const char *v = json_find_key(ctx, key);
    return (v == NULL) ? -1 : json_read_string(v, out, outsz);
}

static int extract_double(const char *ctx, const char *key, double *out) {
    const char *v = json_find_key(ctx, key);
    return (v == NULL) ? -1 : json_read_double(v, out);
}

/* object span skip — '{' 직후 위치 → 같은 depth 의 '}' 다음 위치. */
static const char *json_skip_object_body(const char *p) {
    int depth = 0, in_str = 0, esc = 0;
    while (*p) {
        if (esc) { esc = 0; p++; continue; }
        if (in_str) {
            if (*p == '\\') esc = 1;
            else if (*p == '"') in_str = 0;
            p++; continue;
        }
        if (*p == '"') { in_str = 1; p++; continue; }
        if (*p == '{' || *p == '[') { depth++; p++; continue; }
        if (*p == '}' || *p == ']') {
            if (depth == 0) return p + 1;
            depth--; p++; continue;
        }
        p++;
    }
    return NULL;
}

/* URL escape — RFC 3986 unreserved + ',' '/' 통과. wtgprice url_escape_into 동일. */
static int url_escape_into(char *dst, size_t dstsz, const char *src) {
    static const char hex[] = "0123456789ABCDEF";
    size_t i = 0;
    while (*src) {
        unsigned char c = (unsigned char)*src++;
        int safe = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                   (c >= '0' && c <= '9') ||
                   c == '-' || c == '.' || c == '_' || c == '~' ||
                   c == ',' || c == '/' || c == ':' || c == 'T' || c == 'Z';
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

/* tcp_round_trip — connect+send+recv. wtgprice 와 동일. */
static int tcp_round_trip(wtg_query_client_t *cli, const char *req, size_t req_len,
                          char *resp, size_t resp_cap) {
    struct in_addr ip;
    if (resolve_host(cli->host, &ip) < 0) { cli->last_errno = errno; return WTGQUERY_E_RESOLVE; }
    int sock = socket(AF_INET, SOCK_STREAM, 0);
    if (sock < 0) { cli->last_errno = errno; return WTGQUERY_E_SOCKET; }
    int one = 1;
    (void)setsockopt(sock, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
    struct timeval tv;
    tv.tv_sec = cli->timeout_ms / 1000;
    tv.tv_usec = (cli->timeout_ms % 1000) * 1000;
    (void)setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
    (void)setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)cli->port);
    addr.sin_addr = ip;
    if (connect_timeout(sock, (struct sockaddr *)&addr, sizeof(addr), cli->timeout_ms) < 0) {
        cli->last_errno = errno; close(sock); return WTGQUERY_E_CONNECT;
    }
    if (send_all(sock, req, req_len) < 0) {
        cli->last_errno = errno; close(sock); return WTGQUERY_E_SEND;
    }
    int n = recv_all(sock, resp, resp_cap);
    close(sock);
    if (n <= 0) { cli->last_errno = errno; return WTGQUERY_E_RECV; }
    return n;
}

static int http_status_and_body(wtg_query_client_t *cli, const char *resp,
                                int *status_out, const char **body_out) {
    int status = parse_http_status(resp);
    if (status < 0) return WTGQUERY_E_PARSE;
    cli->last_http_status = status;
    *status_out = status;
    const char *bptr = http_body(resp);
    if (bptr == NULL) return WTGQUERY_E_PARSE;
    *body_out = bptr;
    if (status >= 400) {
        size_t blen = strlen(bptr);
        if (blen >= sizeof(cli->last_error_body)) blen = sizeof(cli->last_error_body) - 1;
        memcpy(cli->last_error_body, bptr, blen);
        cli->last_error_body[blen] = 0;
        return (status >= 500) ? WTGQUERY_E_HTTP_5XX : WTGQUERY_E_HTTP_4XX;
    }
    if (status != 200) return WTGQUERY_E_PARSE;
    return WTGQUERY_OK;
}

/* ====================== mds wire ↔ WTG 변환 ====================== */

/* trim trailing space — mds 의 char[N] 필드는 공백 padding 가능. */
static void trim_spaces(char *dst, const char *src, size_t n) {
    size_t end = 0;
    for (size_t i = 0; i < n && src[i] && src[i] != ' '; i++) {
        dst[end++] = src[i];
    }
    dst[end] = 0;
}

/* "USDKRW" → "USD/KRW", "100JPY/KRW" 는 PoC 미지원 (대부분 6 chars).
 * 반환: 0 = OK, -1 = 길이 6 아님. */
static int symb_to_pair(const char *symb, char *pair_out) {
    char trimmed[16];
    trim_spaces(trimmed, symb, 16);
    size_t L = strlen(trimmed);
    if (L != 6) return -1;
    pair_out[0] = trimmed[0];
    pair_out[1] = trimmed[1];
    pair_out[2] = trimmed[2];
    pair_out[3] = '/';
    pair_out[4] = trimmed[3];
    pair_out[5] = trimmed[4];
    pair_out[6] = trimmed[5];
    pair_out[7] = 0;
    return 0;
}

/* float → mds 16-char field. "%.5f" + zero-padding 으로 채움. */
static void float_to_field(double v, char *field16) {
    char tmp[32];
    int n = snprintf(tmp, sizeof(tmp), "%.5f", v);
    if (n < 0) n = 0;
    if (n > 15) n = 15;
    memcpy(field16, tmp, (size_t)n);
    field16[n] = 0;
    /* 남는 공간 (size 16, 우리는 NUL-terminated 가능 — mds 가 strtod 등 ASCII
     * 숫자 파싱이라 trailing NUL 이후 영역은 의미 없음). */
}

/* int → ASCII field. */
static void int_to_field(int v, char *field, size_t n) {
    snprintf(field, n, "%d", v);
}

/* RFC3339 "2026-06-12T00:00:00Z" → kymd "20260612" + khms "000000".
 * mci-chart 는 timezone Z 보장. 추가 fractional second 가 있어도 prefix 파싱. */
static int rfc3339_to_kymd_khms(const char *rfc, char *kymd, char *khms) {
    if (strlen(rfc) < 19) return -1;
    if (rfc[4] != '-' || rfc[7] != '-' || rfc[10] != 'T' ||
        rfc[13] != ':' || rfc[16] != ':') return -1;
    /* yyyymmdd */
    kymd[0] = rfc[0]; kymd[1] = rfc[1]; kymd[2] = rfc[2]; kymd[3] = rfc[3];
    kymd[4] = rfc[5]; kymd[5] = rfc[6];
    kymd[6] = rfc[8]; kymd[7] = rfc[9];
    kymd[8] = 0;
    /* HHmmss */
    khms[0] = rfc[11]; khms[1] = rfc[12];
    khms[2] = rfc[14]; khms[3] = rfc[15];
    khms[4] = rfc[17]; khms[5] = rfc[18];
    khms[6] = 0;
    return 0;
}

/* 오늘 - 7d ~ 오늘 의 RFC3339 from/to 생성. */
static void today_range_rfc3339(char *from, size_t from_sz, char *to, size_t to_sz) {
    time_t now = time(NULL);
    time_t past = now - 7 * 24 * 3600;
    struct tm tm_to, tm_from;
    gmtime_r(&now, &tm_to);
    gmtime_r(&past, &tm_from);
    strftime(from, from_sz, "%Y-%m-%dT00:00:00Z", &tm_from);
    strftime(to,   to_sz,   "%Y-%m-%dT%H:%M:%SZ", &tm_to);
}

/* ====================== 본 API ====================== */

int wtg_query_init(wtg_query_client_t *cli, const char *host, int port, int timeout_ms) {
    if (cli == NULL || host == NULL || *host == 0 || port <= 0) return WTGQUERY_E_INVALID;
    memset(cli, 0, sizeof(*cli));
    size_t hlen = strlen(host);
    if (hlen + 1 > sizeof(cli->host)) return WTGQUERY_E_INVALID;
    memcpy(cli->host, host, hlen + 1);
    cli->port = port;
    cli->timeout_ms = (timeout_ms > 0) ? timeout_ms : DEFAULT_TIMEOUT_MS;
    return WTGQUERY_OK;
}

int wtg_query_w9501s01(wtg_query_client_t *cli, const W9501S01_in_t *in,
                       W9501S01_out_t *out, size_t out_cap) {
    if (cli == NULL || in == NULL || out == NULL) return WTGQUERY_E_INVALID;
    if (out_cap < sizeof(W9501S01_out_t)) return WTGQUERY_E_OVERSIZE;
    cli->last_http_status = 0;
    cli->last_errno = 0;
    cli->last_error_body[0] = 0;

    /* 1. 입력 검증 + mds → WTG 매핑. */
    char pdcd_trim[8], symb_trim[16];
    trim_spaces(pdcd_trim, in->pdcd, 4);
    trim_spaces(symb_trim, in->symb, 16);
    if (strcmp(pdcd_trim, "SPT") != 0) {
        /* PoC: 현물환만. FWD 는 forward-snapshot 별도 endpoint. */
        return WTGQUERY_E_UNSUPPORTED;
    }
    if (*symb_trim == 0) {
        /* 전체 조회는 PoC 미지원 — pair list 가 너무 크고 mci-chart 가 pair 1개씩. */
        return WTGQUERY_E_INVALID;
    }
    char pair[8];
    if (symb_to_pair(in->symb, pair) < 0) return WTGQUERY_E_INVALID;

    /* 2. URL query 조립 — from/to = 오늘-7d ~ 오늘. limit 16 (cap). */
    char pair_esc[32], from_rfc[24], to_rfc[24], from_esc[32], to_esc[32];
    if (url_escape_into(pair_esc, sizeof(pair_esc), pair) < 0) return WTGQUERY_E_OVERSIZE;
    today_range_rfc3339(from_rfc, sizeof(from_rfc), to_rfc, sizeof(to_rfc));
    if (url_escape_into(from_esc, sizeof(from_esc), from_rfc) < 0) return WTGQUERY_E_OVERSIZE;
    if (url_escape_into(to_esc,   sizeof(to_esc),   to_rfc)   < 0) return WTGQUERY_E_OVERSIZE;

    /* out_cap 으로 data[] 슬롯 수 결정 — limit 에 박음. */
    size_t avail = out_cap - sizeof(W9501S01_out_t);
    int max_records = (int)(avail / sizeof(W9501S01_dat_t));
    if (max_records <= 0) return WTGQUERY_E_OVERSIZE;
    if (max_records > 16) max_records = 16;   /* PoC cap */

    char request[REQ_BUF_MAX];
    int rlen = snprintf(request, sizeof(request),
        "GET /v1/chart?pair=%s&tf=1d&from=%s&to=%s&limit=%d HTTP/1.1\r\n"
        "Host: %s:%d\r\n"
        "Connection: close\r\n"
        "\r\n",
        pair_esc, from_esc, to_esc, max_records, cli->host, cli->port);
    if (rlen < 0 || rlen >= (int)sizeof(request)) return WTGQUERY_E_OVERSIZE;

    /* 3. round-trip. */
    char *resp = (char *)malloc(RESP_BUF);
    if (resp == NULL) return WTGQUERY_E_INVALID;
    int n = tcp_round_trip(cli, request, (size_t)rlen, resp, RESP_BUF);
    if (n < 0) { free(resp); return n; }

    /* 4. status. */
    int status;
    const char *bptr = NULL;
    int rc = http_status_and_body(cli, resp, &status, &bptr);
    if (rc != WTGQUERY_OK) { free(resp); return rc; }

    /* 5. 응답 파싱 — bars[] iteration. */
    memset(out, 0, sizeof(W9501S01_out_t));
    /* 입력 echo. */
    memcpy(out->pdcd,  in->pdcd,  sizeof(out->pdcd));
    memcpy(out->symb,  in->symb,  sizeof(out->symb));
    memcpy(out->tenor, in->tenor, sizeof(out->tenor));

    const char *bars_v = json_find_key(bptr, "bars");
    if (bars_v == NULL) { free(resp); return WTGQUERY_E_PARSE; }
    bars_v = skip_ws(bars_v);
    if (*bars_v != '[') { free(resp); return WTGQUERY_E_PARSE; }
    const char *cur = bars_v + 1;
    int nrec = 0;

    /* trim 결과 — symb 만 dat 에 그대로 (mds 도 16-char). */
    char symb_dat[16];
    memset(symb_dat, 0, sizeof(symb_dat));
    memcpy(symb_dat, symb_trim, strlen(symb_trim));

    for (;;) {
        cur = skip_ws(cur);
        if (*cur == ']') break;
        if (*cur == ',') { cur = skip_ws(cur + 1); }
        if (*cur != '{') { free(resp); return WTGQUERY_E_PARSE; }
        if (nrec >= max_records) { free(resp); return WTGQUERY_E_OVERSIZE; }

        W9501S01_dat_t *d = &out->data[nrec];
        memset(d, 0, sizeof(*d));
        const char *inside = cur + 1;

        char opened_at[40] = {0};
        double open_bid, open_ask, high_bid, high_ask;
        double low_bid, low_ask, close_bid, close_ask;
        if (extract_string(inside, "opened_at", opened_at, sizeof(opened_at)) < 0 ||
            extract_double(inside, "open_bid",  &open_bid)  < 0 ||
            extract_double(inside, "open_ask",  &open_ask)  < 0 ||
            extract_double(inside, "high_bid",  &high_bid)  < 0 ||
            extract_double(inside, "high_ask",  &high_ask)  < 0 ||
            extract_double(inside, "low_bid",   &low_bid)   < 0 ||
            extract_double(inside, "low_ask",   &low_ask)   < 0 ||
            extract_double(inside, "close_bid", &close_bid) < 0 ||
            extract_double(inside, "close_ask", &close_ask) < 0) {
            free(resp); return WTGQUERY_E_PARSE;
        }

        /* mds 채우기. */
        memcpy(d->symb, symb_dat, sizeof(d->symb));
        /* tenor 는 spot 이라 ""; expiymd 도 "". */
        if (rfc3339_to_kymd_khms(opened_at, d->kymd, d->khms) < 0) {
            free(resp); return WTGQUERY_E_PARSE;
        }
        float_to_field(open_bid,  d->bid_open);
        float_to_field(high_bid,  d->bid_high);
        float_to_field(low_bid,   d->bid_lowp);
        float_to_field(close_bid, d->bid_last);
        float_to_field(open_ask,  d->ask_open);
        float_to_field(high_ask,  d->ask_high);
        float_to_field(low_ask,   d->ask_lowp);
        float_to_field(close_ask, d->ask_last);

        nrec++;
        cur = json_skip_object_body(cur + 1);
        if (cur == NULL) { free(resp); return WTGQUERY_E_PARSE; }
    }

    int_to_field(nrec, out->nrec, sizeof(out->nrec));
    free(resp);
    return WTGQUERY_OK;
}

const char *wtg_query_strerror(int code) {
    switch (code) {
    case WTGQUERY_OK:              return "ok";
    case WTGQUERY_E_INVALID:       return "invalid argument";
    case WTGQUERY_E_RESOLVE:       return "host resolve failed";
    case WTGQUERY_E_SOCKET:        return "socket() failed";
    case WTGQUERY_E_CONNECT:       return "connect timeout or refused";
    case WTGQUERY_E_SEND:          return "send() failed";
    case WTGQUERY_E_RECV:          return "recv() failed or timeout";
    case WTGQUERY_E_PARSE:         return "response parse failed";
    case WTGQUERY_E_HTTP_4XX:      return "HTTP 4xx";
    case WTGQUERY_E_HTTP_5XX:      return "HTTP 5xx";
    case WTGQUERY_E_OVERSIZE:      return "buffer oversize";
    case WTGQUERY_E_UNSUPPORTED:   return "unsupported input (e.g., FWD pdcd)";
    default:                       return "unknown error";
    }
}
